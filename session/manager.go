package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/spirilis/generic-go-mcp/logging"
)

// Errors the manager returns. Both are conditions a model can act on — end something, or
// fix the id — so tools surface them as tool errors rather than protocol errors.
var (
	ErrSessionLimit  = errors.New("session limit reached")
	ErrNoSuchSession = errors.New("no such session")
)

// Manager owns the live set of sessions.
//
// It deliberately knows nothing about MCP: sessions are created, looked up and ended here,
// and everything protocol-shaped (registering a resource pair, pushing an updated
// notification) hangs off the lifecycle observer instead. That keeps the collection
// testable without standing up a server, which is most of what these tests need to do.
//
// Single-flight execution is *not* this type's problem. Each Session already serialises its
// own commands through a capacity-1 slot channel, so "one command at a time" simply becomes
// per-session the moment there is more than one session, with nothing to add here.
type Manager struct {
	mu       sync.Mutex
	sessions map[string]*entry
	order    []string // creation order, so listings are stable rather than map-random

	max         int
	idleTimeout time.Duration

	onAdd    func(id string, s *Session)
	onRemove func(id string)

	stopOnce sync.Once
	stop     chan struct{}
}

type entry struct {
	id        string
	sess      *Session
	params    map[string]string
	createdAt time.Time
	lastUsed  time.Time
}

// Info is one session's entry in a listing.
type Info struct {
	ID string `json:"id"`
	// Parameters are the values start_session was called with, echoed back so a listing
	// says which target a session actually reached rather than only that it exists.
	Parameters map[string]string `json:"parameters,omitempty"`
	CreatedAt  time.Time         `json:"created_at"`
	LastUsed   time.Time         `json:"last_used"`
	Status     Status            `json:"status"`
}

// NewManager returns a Manager admitting at most max live sessions. A non-zero idleTimeout
// starts a reaper; zero means sessions live until they are ended or the process exits.
func NewManager(max int, idleTimeout time.Duration) *Manager {
	if max <= 0 {
		max = 1
	}
	m := &Manager{
		sessions:    make(map[string]*entry),
		max:         max,
		idleTimeout: idleTimeout,
		stop:        make(chan struct{}),
	}
	if idleTimeout > 0 {
		go m.reap()
	}
	return m
}

// SetLifecycleObserver registers callbacks fired when a session joins or leaves the
// manager. onAdd runs before the session's login handshake begins, which is what lets a
// subscriber watch that handshake's own state transitions instead of missing all of them.
//
// Neither callback runs under the manager's lock, so both may call back into it. Set them
// before the first Start.
func (m *Manager) SetLifecycleObserver(onAdd func(id string, s *Session), onRemove func(id string)) {
	m.mu.Lock()
	m.onAdd, m.onRemove = onAdd, onRemove
	m.mu.Unlock()
}

// Start creates a session from an already-rendered config, spawns its shell, and returns
// the id under which it is now reachable. The login handshake runs in the background.
//
// The split is deliberate. Spawning fails fast and for reasons the caller can do something
// about (no such command, no PTY), so it happens synchronously and its error is returned —
// an id is never handed back for a session that has no shell behind it. The handshake is
// the slow half, and how slow is a property of the bastion: banners, MFA and PAM dispatch
// routinely outlast an MCP client's tool-call deadline. Blocking on it would mean a client
// that gave up on the call while the session it asked for came up anyway, alive and
// audited, with nobody holding its id. Callers learn readiness from list_sessions or the
// session's status resource; a command sent too early fails cleanly through checkRunnable.
func (m *Manager) Start(cfg Config, params map[string]string) (string, error) {
	// New and Spawn run under the lock so the cap is enforced by construction. Two
	// concurrent start_session calls are entirely possible — the transport dispatches
	// every message in its own goroutine — and a check-then-act cap would let both
	// through. Spawn is a fork/exec, not a network round trip, so the section is short.
	m.mu.Lock()
	if live := m.liveCountLocked(); live >= m.max {
		m.mu.Unlock()
		return "", fmt.Errorf("%w (%d live); end one with end_session before starting another", ErrSessionLimit, m.max)
	}

	sess, err := New(cfg)
	if err != nil {
		m.mu.Unlock()
		return "", err
	}
	if err := sess.Spawn(); err != nil {
		m.mu.Unlock()
		return "", err
	}

	now := time.Now()
	id := m.newIDLocked()
	m.sessions[id] = &entry{id: id, sess: sess, params: params, createdAt: now, lastUsed: now}
	m.order = append(m.order, id)
	onAdd := m.onAdd
	m.mu.Unlock()

	if onAdd != nil {
		onAdd(id, sess)
	}
	go sess.Handshake(context.Background())

	logging.Info("session started", "session_id", id, "command", cfg.Command)
	return id, nil
}

// Get returns the session registered under id.
//
// It also marks the session as used. That timestamp is taken when a command is handed the
// session rather than when the command finishes, so a long-running command looks idle by
// this clock — which is why the reaper independently refuses to touch a busy session.
func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.sessions[id]
	if !ok {
		return nil, false
	}
	e.lastUsed = time.Now()
	return e.sess, true
}

// End closes the session registered under id and removes it, reporting whether one was
// found.
//
// A command still in flight on that session is cancelled, not waited for: the PTY closes
// under it and it returns ErrDead. That is exactly what process shutdown already does to a
// running command, and it is the plainer reading of the tool's name — a caller asking to
// end a session is not asking to be blocked by whatever it was doing.
func (m *Manager) End(id string) bool {
	m.mu.Lock()
	e, ok := m.sessions[id]
	if ok {
		delete(m.sessions, id)
		m.removeOrderLocked(id)
	}
	onRemove := m.onRemove
	m.mu.Unlock()

	if !ok {
		return false
	}
	// Closed before it is unregistered, so a client subscribed to this session's status
	// sees it go dead and only then sees it leave the catalog. The other order announces a
	// disappearance whose reason was never delivered.
	_ = e.sess.Close()
	if onRemove != nil {
		onRemove(id)
	}
	logging.Info("session ended", "session_id", id)
	return true
}

// List snapshots every session in creation order.
func (m *Manager) List() []Info {
	m.mu.Lock()
	entries := make([]*entry, 0, len(m.order))
	for _, id := range m.order {
		if e, ok := m.sessions[id]; ok {
			entries = append(entries, e)
		}
	}
	m.mu.Unlock()

	// Status takes the session's own lock, so it is called outside the manager's.
	out := make([]Info, 0, len(entries))
	for _, e := range entries {
		out = append(out, Info{
			ID:         e.id,
			Parameters: e.params,
			CreatedAt:  e.createdAt,
			LastUsed:   e.lastUsed,
			Status:     e.sess.Status(),
		})
	}
	return out
}

// Len reports how many sessions are registered, live or dead.
func (m *Manager) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

// Live reports how many sessions still have a shell behind them, which is what Max is
// enforced against.
func (m *Manager) Live() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.liveCountLocked()
}

// Max is the configured cap.
func (m *Manager) Max() int { return m.max }

// CloseAll ends every session and stops the reaper. Safe to call more than once, which
// main.go relies on: it closes deferred and again explicitly, so "shutdown complete" is not
// printed before the shells are actually gone.
//
// It drains until the map is empty rather than closing a snapshot of it. A single pass would
// miss a session created between the snapshot and the loop, and a missed session here is a
// privileged bastion login left open — the exact thing this server must not do.
func (m *Manager) CloseAll() {
	m.stopOnce.Do(func() { close(m.stop) })

	for {
		m.mu.Lock()
		ids := append([]string(nil), m.order...)
		m.mu.Unlock()

		if len(ids) == 0 {
			return
		}
		for _, id := range ids {
			m.End(id)
		}
	}
}

// liveCountLocked counts sessions that are not dead. Dead sessions are kept so a model can
// read *why* one died, but they hold no SSH connection and must not count against the cap —
// otherwise a target that fails to connect would wedge the server after max attempts.
func (m *Manager) liveCountLocked() int {
	n := 0
	for _, e := range m.sessions {
		if e.sess.Status().State != StateDead.String() {
			n++
		}
	}
	return n
}

func (m *Manager) removeOrderLocked(id string) {
	for i, existing := range m.order {
		if existing == id {
			m.order = append(m.order[:i], m.order[i+1:]...)
			return
		}
	}
}

// newIDLocked mints an unused session id. Random rather than sequential: an id is the only
// handle a model has on a privileged session, and a counter restarts at the same values
// every time this process does, so a stale "s-1" from an earlier run would silently address
// a different bastion.
func (m *Manager) newIDLocked() string {
	for {
		var b [4]byte
		if _, err := rand.Read(b[:]); err != nil {
			// Only reachable if the system entropy source is broken; time is unique
			// enough to keep the map consistent, which is all this needs to guarantee.
			return fmt.Sprintf("s-%x", time.Now().UnixNano())
		}
		id := "s-" + hex.EncodeToString(b[:])
		if _, taken := m.sessions[id]; !taken {
			return id
		}
	}
}

// reap closes sessions that have gone idleTimeout without being used. It runs only when an
// idle timeout was configured.
func (m *Manager) reap() {
	interval := m.idleTimeout / 4
	if interval < time.Second {
		interval = time.Second
	}
	if interval > time.Minute {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stop:
			return
		case <-ticker.C:
			for _, id := range m.idleCandidates() {
				logging.Info("closing idle session", "session_id", id, "idle_timeout", m.idleTimeout)
				m.End(id)
			}
		}
	}
}

// idleCandidates collects the ids eligible for reaping. The manager's own state is read
// under its lock and everything else happens outside it, since both Session.Status and End
// take locks of their own.
func (m *Manager) idleCandidates() []string {
	cutoff := time.Now().Add(-m.idleTimeout)

	type candidate struct {
		id   string
		sess *Session
	}
	m.mu.Lock()
	stale := make([]candidate, 0, len(m.sessions))
	for _, e := range m.sessions {
		if e.lastUsed.Before(cutoff) {
			stale = append(stale, candidate{e.id, e.sess})
		}
	}
	m.mu.Unlock()

	ids := make([]string, 0, len(stale))
	for _, c := range stale {
		// Two states are busy in the sense that matters here, and neither shows up in
		// lastUsed. Get stamps that clock when a session is handed to a command, not when
		// the command finishes, so anything long-running looks perfectly idle by it; and a
		// session still logging in was never handed out at all, though a login on a bastion
		// with banners and MFA can easily outlast a short idle timeout. Reaping either one
		// would destroy work in progress. Both are bounded — by command_timeout and
		// login_timeout — so neither can defer reaping indefinitely.
		switch c.sess.Status().State {
		case StateBusy.String(), StateStarting.String():
			continue
		}
		ids = append(ids, c.id)
	}
	return ids
}
