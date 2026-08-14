// Package session drives a single long-lived interactive shell over a PTY.
//
// The premise, from PLAN.md, is that the SSH command can be executed exactly once: the
// target is a Privileged Access Management bastion that dispatches on a specially-crafted
// username, so neither `ssh host cmd` nor channel multiplexing is available. Commands are
// therefore typed into one persistent shell and their output is scraped back out.
//
// Two mechanisms frame that output. A per-command sentinel marker carrying $? gives exact
// boundaries and a real exit code, and is preferred. The configured prompt regex frames
// login, backstops recovery, and takes over entirely if the remote shell turns out not to
// support the sentinel.
package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/spirilis/generic-go-mcp/logging"
)

// State is the session's lifecycle position.
type State int

const (
	// StateStarting is the window between spawning the command and completing login.
	StateStarting State = iota
	// StateAwaitingPrompt means login output never matched the prompt regex. The session
	// is alive but unusable until the regex is corrected — deliberately not fatal, so the
	// transcript can be inspected and the regex retuned at runtime.
	StateAwaitingPrompt
	// StateReady means the shell is at a prompt and idle.
	StateReady
	// StateBusy means a command is executing.
	StateBusy
	// StateDesynced means a command was abandoned and the shell could not be brought back
	// to a known position.
	StateDesynced
	// StateDead means the shell is gone.
	StateDead
)

func (s State) String() string {
	switch s {
	case StateStarting:
		return "starting"
	case StateAwaitingPrompt:
		return "awaiting_prompt"
	case StateReady:
		return "ready"
	case StateBusy:
		return "busy"
	case StateDesynced:
		return "desynced"
	case StateDead:
		return "dead"
	}
	return "unknown"
}

// Errors returned by Execute and friends.
var (
	ErrTimeout        = errors.New("timed out waiting for the shell")
	ErrNotReady       = errors.New("session is not ready")
	ErrDead           = errors.New("session is dead; the command that opened it cannot be re-run, so start a new session")
	ErrAwaitingPrompt = errors.New("the configured prompt regex has not matched the shell's output; read the session transcript and correct it with tune_prompt_regex")
	ErrDesynced       = errors.New("the shell could not be resynchronised after an abandoned command")
)

// Config parameterizes a Session. It mirrors the ssh: block of the config file, kept as a
// separate type so this package stays independently testable.
type Config struct {
	Command         string
	PromptRegex     string
	LoginTimeout    time.Duration
	CommandTimeout  time.Duration
	RecoveryTimeout time.Duration
	Term            string
	Cols, Rows      uint16
	InitCommands    []string
	UseSentinel     bool
	MaxOutputBytes  int
	TranscriptBytes int
	AllowReconnect  bool
}

// Result is one command's outcome.
type Result struct {
	Output    string
	ExitCode  int  // -1 when the sentinel is unavailable
	HasExit   bool // false when ExitCode is not meaningful
	Duration  time.Duration
	Truncated bool
}

// Status is a point-in-time snapshot for the status resource.
type Status struct {
	State            string    `json:"state"`
	PromptRegex      string    `json:"prompt_regex"`
	SentinelEnabled  bool      `json:"sentinel_enabled"`
	Command          string    `json:"command"`
	ConnectedAt      time.Time `json:"connected_at"`
	CommandsRun      int       `json:"commands_run"`
	Reconnected      bool      `json:"reconnected"`
	ReconnectAllowed bool      `json:"reconnect_allowed"`
	TranscriptBytes  int       `json:"transcript_bytes"`
	LastError        string    `json:"last_error,omitempty"`
}

// Session owns the PTY, the shell behind it, and the single-flight discipline that keeps
// exactly one command on the wire at a time.
type Session struct {
	cfg Config

	// slot has capacity 1 and is the single-flight gate. A channel rather than a Mutex
	// because acquisition must be selectable against a cancelled context.
	slot chan struct{}

	mu     sync.Mutex
	notify chan struct{} // capacity 1; poked by the reader, consumed by the sole waiter

	// Guarded by mu.
	acc           []byte // cleaned output accumulated since the current region began
	scanned       int    // how far into acc the marker search has already looked
	elided        int    // bytes dropped from the middle of acc by compaction
	elideAt       int    // where in acc that elision happened
	state         State
	prompt        *regexp.Regexp
	sentinel      bool
	readErr       error
	gen           int // spawn generation, so a dead reader cannot clobber a new session
	connectedAt   time.Time
	commandsRun   int
	reconnectUsed bool
	lastError     string
	observer      func(Status)
	idleTails     [][]byte

	procMu sync.Mutex
	ptmx   *os.File
	proc   *exec.Cmd

	transcript *Transcript
	accLimit   int
}

// New validates cfg and prepares a Session. It does not spawn anything; call Start.
func New(cfg Config) (*Session, error) {
	re, err := regexp.Compile(cfg.PromptRegex)
	if err != nil {
		return nil, fmt.Errorf("prompt_regex does not compile: %w", err)
	}
	accLimit := 2*cfg.MaxOutputBytes + (64 << 10)
	return &Session{
		cfg:        cfg,
		slot:       make(chan struct{}, 1),
		notify:     make(chan struct{}, 1),
		prompt:     re,
		sentinel:   cfg.UseSentinel,
		state:      StateStarting,
		transcript: NewTranscript(cfg.TranscriptBytes),
		accLimit:   accLimit,
	}, nil
}

// Start spawns the configured command and performs the login handshake.
//
// A handshake that never sees the prompt is reported but not fatal: the session lands in
// StateAwaitingPrompt so the regex can be corrected at runtime. Start returns an error
// only when the command itself could not be spawned.
func (s *Session) Start(ctx context.Context) error {
	if err := s.Spawn(); err != nil {
		return err
	}
	s.Handshake(ctx)
	return nil
}

// Spawn starts the command under a PTY and returns as soon as it is running, without
// waiting for a prompt. It is the fast, reliably-failing half of Start: everything that can
// go wrong here (a command that does not exist, no PTY available) does so in microseconds
// and is worth reporting to whoever asked for the session.
//
// Callers that split the two must call Handshake afterwards; until they do, the session
// sits in StateStarting and Execute refuses cleanly.
func (s *Session) Spawn() error { return s.spawn() }

// Handshake waits for the login prompt, probes sentinel support, and runs the init
// commands, leaving the session in StateReady — or in StateAwaitingPrompt if the prompt
// regex never matched, which is recoverable via SetPromptRegex and Resync.
//
// This is the slow half, and how slow is a property of the far end rather than of anything
// this process controls: banners, MFA and PAM dispatch on a bastion routinely outlast an
// MCP client's patience. It reports nothing because there is nothing a caller could do with
// the answer that reading the session's state would not tell it better.
func (s *Session) Handshake(ctx context.Context) { s.handshake(ctx) }

func (s *Session) spawn() error {
	proc := exec.Command("sh", "-c", s.cfg.Command)
	proc.Env = append(os.Environ(), "TERM="+s.cfg.Term)

	// pty.Start wires the child's stdin/stdout/stderr to the pts, which also keeps it
	// away from this process's stdio — where the JSON-RPC stream lives.
	ptmx, err := pty.Start(proc)
	if err != nil {
		return fmt.Errorf("failed to start %q: %w", s.cfg.Command, err)
	}
	if err := pty.Setsize(ptmx, &pty.Winsize{Rows: s.cfg.Rows, Cols: s.cfg.Cols}); err != nil {
		logging.Warn("could not set PTY window size", "error", err)
	}

	s.procMu.Lock()
	s.ptmx, s.proc = ptmx, proc
	s.procMu.Unlock()

	s.mu.Lock()
	s.gen++
	gen := s.gen
	s.acc, s.scanned, s.elided, s.elideAt = s.acc[:0], 0, 0, 0
	s.readErr = nil
	s.state = StateStarting
	s.connectedAt = time.Now()
	s.mu.Unlock()

	go s.readLoop(gen, ptmx)
	logging.Info("shell session started", "command", s.cfg.Command)
	return nil
}

// readLoop drains the PTY for the lifetime of one spawn. It must never block on anything
// slow: a stalled reader backs up into the remote shell.
func (s *Session) readLoop(gen int, ptmx *os.File) {
	var filter ansiFilter
	buf := make([]byte, 32<<10)
	for {
		n, err := ptmx.Read(buf)
		if n > 0 {
			raw := buf[:n]
			s.transcript.Write(raw)
			if clean := filter.filter(raw); len(clean) > 0 {
				s.mu.Lock()
				if s.gen == gen {
					s.acc = append(s.acc, clean...)
				}
				s.mu.Unlock()
			}
			s.poke()
		}
		if err != nil {
			s.mu.Lock()
			var observer func(Status)
			var snap Status
			if s.gen == gen {
				s.readErr = err
				if s.state != StateDead {
					s.state = StateDead
					s.lastError = err.Error()
					observer, snap = s.observer, s.statusLocked()
				}
			}
			s.mu.Unlock()
			s.poke()
			if observer != nil {
				observer(snap)
			}
			return
		}
	}
}

// poke wakes the waiter without ever blocking the reader.
func (s *Session) poke() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func (s *Session) handshake(ctx context.Context) {
	if _, _, err := s.wait(ctx, s.cfg.LoginTimeout, s.promptScan()); err != nil {
		// Snapshot what login actually produced. This is the haystack tune_prompt_regex
		// tests candidates against, and it is the whole reason a failed handshake leaves
		// the session alive rather than killing it.
		s.recordIdleTail()
		logging.Warn("prompt regex did not match during login; session is alive but unusable until it is corrected",
			"prompt_regex", s.cfg.PromptRegex, "error", err)
		s.setState(StateAwaitingPrompt, "prompt regex did not match during login")
		return
	}
	s.recordIdleTail()
	logging.Info("shell prompt detected")

	if s.cfg.UseSentinel {
		s.probeSentinel(ctx)
	}

	for _, c := range s.cfg.InitCommands {
		if _, err := s.runCommand(ctx, c, s.cfg.RecoveryTimeout); err != nil {
			logging.Warn("init command failed", "command", c, "error", err)
		}
	}

	s.setState(StateReady, "")
}

// probeSentinel checks that the remote shell can emit a marker line before any command
// depends on one. A restricted shell that rejects `;`, `printf` or `$?` fails here and the
// session degrades to prompt-only framing rather than hanging on first use.
func (s *Session) probeSentinel(ctx context.Context) {
	m := newMarker(newNonce())
	timeout := 10 * time.Second
	if s.cfg.LoginTimeout < timeout {
		timeout = s.cfg.LoginTimeout
	}

	s.resetRegion()
	if err := s.writeLine(m.probe()); err != nil {
		s.setSentinel(false)
		return
	}
	idx, _, err := s.wait(ctx, timeout, markerScan(m))
	if err != nil {
		logging.Warn("remote shell did not echo the sentinel marker; falling back to prompt-only framing (no exit codes)", "error", err)
		s.setSentinel(false)
		_ = s.resync(ctx, s.cfg.RecoveryTimeout)
		return
	}
	s.settle(idx)
	s.setSentinel(true)
	logging.Debug("sentinel framing confirmed")
}

// Execute runs cmd in the shell. Exactly one Execute is in flight at a time; callers
// beyond the first wait for the slot or for their own context to be cancelled.
func (s *Session) Execute(ctx context.Context, cmd string) (*Result, error) {
	select {
	case s.slot <- struct{}{}:
		defer func() { <-s.slot }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	if err := s.checkRunnable(); err != nil {
		return nil, err
	}

	s.setState(StateBusy, "")
	res, err := s.runCommand(ctx, cmd, s.cfg.CommandTimeout)
	if err != nil {
		s.recoverSession(err)
		return nil, err
	}

	// Not setState: the counter and the state belong to the same status snapshot, so they
	// move under one lock and the observer is told once, about both.
	s.mu.Lock()
	s.commandsRun++
	s.state = StateReady
	observer, snap := s.observer, s.statusLocked()
	s.mu.Unlock()

	if observer != nil {
		observer(snap)
	}
	return res, nil
}

func (s *Session) checkRunnable() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.state {
	case StateReady:
		return nil
	case StateAwaitingPrompt:
		return ErrAwaitingPrompt
	case StateDesynced:
		return ErrDesynced
	case StateDead:
		return ErrDead
	default:
		return fmt.Errorf("%w: %s", ErrNotReady, s.state)
	}
}

// runCommand does the actual write-and-scrape. It assumes the caller holds the slot and
// makes no state-machine decisions of its own, so the handshake can use it too.
func (s *Session) runCommand(ctx context.Context, cmd string, timeout time.Duration) (*Result, error) {
	started := time.Now()
	useSentinel := s.sentinelEnabled()

	var m marker
	line := cmd
	if useSentinel {
		m = newMarker(newNonce())
		line = m.command(cmd)
	}

	s.resetRegion()
	if err := s.writeLine(line); err != nil {
		return nil, err
	}

	var (
		idx      int
		exitCode int
		err      error
	)
	if useSentinel {
		idx, exitCode, err = s.wait(ctx, timeout, markerScan(m))
	} else {
		idx, _, err = s.wait(ctx, timeout, s.promptScan())
	}
	if err != nil {
		return nil, err
	}

	region, elided := s.regionUpTo(idx)

	// The marker is emitted before the shell redraws its prompt, so returning the instant
	// it is seen leaves those prompt bytes in flight — they would arrive after the next
	// command's region had already been opened and show up prefixed to its output. Wait
	// for the shell to come to rest first.
	if useSentinel {
		s.settle(idx)
	}
	if useSentinel {
		region = m.stripEcho(region)
	} else {
		region = stripLeadingEcho(region, cmd)
	}

	out, truncated := truncateMiddle(trimTrailingNewline(string(region)), s.cfg.MaxOutputBytes)
	res := &Result{
		Output:    out,
		Duration:  time.Since(started),
		Truncated: truncated || elided,
		ExitCode:  -1,
	}
	if useSentinel {
		res.ExitCode, res.HasExit = exitCode, true
	}
	return res, nil
}

// recoverSession brings the shell back to a known position after a command was abandoned.
//
// The shell is shared, so simply walking away from a timed-out read would hand the next
// caller a stream still carrying the previous command's output. Recovery runs on a fresh
// context because the request's own context is, by this point, usually already cancelled.
func (s *Session) recoverSession(reason error) {
	logging.Warn("command did not complete; resynchronising the shell", "reason", reason)

	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.RecoveryTimeout)
	defer cancel()

	s.resetRegion()
	if err := s.writeRaw([]byte{0x03}); err == nil { // Ctrl-C
		if _, _, err := s.wait(ctx, s.cfg.RecoveryTimeout, s.promptScan()); err == nil {
			logging.Info("shell resynchronised")
			s.setState(StateReady, "")
			return
		}
	}

	if s.cfg.AllowReconnect && !s.reconnectedAlready() {
		logging.Warn("resynchronisation failed; using the one permitted reconnect")
		if err := s.reconnect(); err != nil {
			logging.Error("reconnect failed", "error", err)
			s.setState(StateDead, fmt.Sprintf("reconnect failed: %v", err))
		}
		return
	}

	logging.Error("shell could not be resynchronised and reconnect is not available")
	s.setState(StateDesynced, fmt.Sprintf("could not resynchronise after: %v", reason))
}

func (s *Session) reconnect() error {
	s.closeProc()
	s.mu.Lock()
	s.reconnectUsed = true
	s.mu.Unlock()

	if err := s.spawn(); err != nil {
		return err
	}
	s.handshake(context.Background())
	return nil
}

// Resync sends a bare newline and waits for the prompt. It is how a session sitting in
// StateAwaitingPrompt is promoted once the regex has been corrected.
func (s *Session) Resync(ctx context.Context) error {
	select {
	case s.slot <- struct{}{}:
		defer func() { <-s.slot }()
	case <-ctx.Done():
		return ctx.Err()
	}
	return s.resync(ctx, s.cfg.RecoveryTimeout)
}

func (s *Session) resync(ctx context.Context, timeout time.Duration) error {
	if s.isDead() {
		return ErrDead
	}
	s.resetRegion()
	if err := s.writeLine(""); err != nil {
		return err
	}
	if _, _, err := s.wait(ctx, timeout, s.promptScan()); err != nil {
		return err
	}
	s.setState(StateReady, "")
	return nil
}

// SetPromptRegex installs a new prompt regex. The session is not resynchronised here;
// callers that want the session promoted out of StateAwaitingPrompt call Resync after.
func (s *Session) SetPromptRegex(expr string) error {
	re, err := regexp.Compile(expr)
	if err != nil {
		return fmt.Errorf("regex does not compile: %w", err)
	}
	s.mu.Lock()
	s.prompt = re
	s.cfg.PromptRegex = expr
	observer, snap := s.observer, s.statusLocked()
	s.mu.Unlock()

	logging.Info("prompt regex updated", "prompt_regex", expr)
	// The regex is part of the status snapshot, and a repair that leaves the state alone
	// (the session was already ready) would otherwise change the resource silently.
	if observer != nil {
		observer(snap)
	}
	return nil
}

// TestPromptRegex dry-runs expr and returns what it would have matched, so a candidate can
// be checked before it is installed.
//
// It matches against recorded idle tails rather than the transcript, because those are
// what the live matcher actually sees: a buffer that ends where the shell stopped writing.
// In the transcript the same prompt is usually followed by the next command's output on
// the same line — no newline is emitted after a prompt — so an end-anchored pattern that
// works perfectly in practice would appear to match nothing. The transcript is still used
// as a fallback when no idle tail has been recorded yet.
func (s *Session) TestPromptRegex(expr string, limit int) ([]string, error) {
	re, err := regexp.Compile(expr)
	if err != nil {
		return nil, fmt.Errorf("regex does not compile: %w", err)
	}
	if limit <= 0 {
		limit = 10
	}

	s.mu.Lock()
	tails := make([][]byte, len(s.idleTails))
	copy(tails, s.idleTails)
	s.mu.Unlock()

	seen := make(map[string]bool)
	var out []string
	add := func(hay []byte) {
		for _, f := range re.FindAll(hay, limit) {
			if m := string(f); !seen[m] && len(out) < limit {
				seen[m] = true
				out = append(out, m)
			}
		}
	}

	for _, t := range tails {
		add(t)
	}
	if len(out) == 0 {
		add(normalizeNewlines(stripANSI(s.transcript.Tail(0))))
	}
	return out, nil
}

// Transcript returns the most recent n raw bytes the shell emitted, escape sequences
// intact.
func (s *Session) Transcript(n int) []byte { return s.transcript.Tail(n) }

// Status snapshots the session for the status resource.
func (s *Session) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statusLocked()
}

// statusLocked builds the snapshot with s.mu already held, so a mutation can hand the
// observer the exact state it just produced rather than racing another writer for it.
func (s *Session) statusLocked() Status {
	return Status{
		State:            s.state.String(),
		PromptRegex:      s.cfg.PromptRegex,
		SentinelEnabled:  s.sentinel,
		Command:          s.cfg.Command,
		ConnectedAt:      s.connectedAt,
		CommandsRun:      s.commandsRun,
		Reconnected:      s.reconnectUsed,
		ReconnectAllowed: s.cfg.AllowReconnect,
		TranscriptBytes:  s.transcript.Size(),
		LastError:        s.lastError,
	}
}

// Close terminates the shell.
func (s *Session) Close() error {
	s.setState(StateDead, "closed")
	s.closeProc()
	return nil
}

func (s *Session) closeProc() {
	s.procMu.Lock()
	ptmx, proc := s.ptmx, s.proc
	s.ptmx, s.proc = nil, nil
	s.procMu.Unlock()

	if ptmx != nil {
		_ = ptmx.Close()
	}
	if proc != nil && proc.Process != nil {
		_ = proc.Process.Kill()
		_, _ = proc.Process.Wait()
	}
}

// --- scanning ---------------------------------------------------------------------

// scanFn looks for a completion signal in buf. from is a hint of where an incremental
// search may resume; scanners that need their own windowing ignore it.
type scanFn func(buf []byte, from int) (idx, extra int, done bool)

func markerScan(m marker) scanFn {
	return func(buf []byte, from int) (int, int, bool) {
		start, code, ok := m.find(buf, from)
		return start, code, ok
	}
}

func (s *Session) promptScan() scanFn {
	re := s.promptRegex()
	return func(buf []byte, _ int) (int, int, bool) {
		idx := findPrompt(re, buf, 0)
		return idx, 0, idx >= 0
	}
}

// Bounds for settling the shell after a command's marker has been seen.
const (
	settleQuiet = 250 * time.Millisecond
	settleCap   = 2 * time.Second
)

// Idle tails are snapshots of the buffer taken whenever the shell falls silent — which is
// exactly the shape the prompt matcher runs against. Keeping a few makes it possible to
// test a candidate prompt regex under realistic conditions instead of against scrollback.
const (
	maxIdleTails  = 5
	idleTailBytes = 512
)

// recordIdleTailLocked snapshots the end of the current buffer. Caller holds mu.
func (s *Session) recordIdleTailLocked() {
	n := len(s.acc)
	if n == 0 {
		return
	}
	if n > idleTailBytes {
		n = idleTailBytes
	}
	tail := make([]byte, n)
	copy(tail, s.acc[len(s.acc)-n:])

	s.idleTails = append(s.idleTails, tail)
	if len(s.idleTails) > maxIdleTails {
		s.idleTails = s.idleTails[1:]
	}
}

func (s *Session) recordIdleTail() {
	s.mu.Lock()
	s.recordIdleTailLocked()
	s.mu.Unlock()
}

// settle waits for the shell to come to rest after the sentinel marker at or before index
// after, so the next command starts from a known position.
//
// It returns as soon as the prompt appears, which is the normal case and usually immediate.
// Quiescence is the fallback: if no new bytes arrive for settleQuiet, the shell is treated
// as idle regardless of whether the prompt regex matched. That fallback is what keeps a
// misconfigured prompt regex from corrupting every subsequent command's output — the one
// failure this would otherwise cause silently.
func (s *Session) settle(after int) {
	// Drop a stale wake-up left over from the marker's own arrival. Whatever it stood for
	// is already in acc and will be seen by the scan below; leaving it queued would make
	// the quiet timer measure nothing and let settle return before the shell is idle.
	select {
	case <-s.notify:
	default:
	}

	re := s.promptRegex()
	hardCap := time.NewTimer(settleCap)
	defer hardCap.Stop()
	quiet := time.NewTimer(settleQuiet)
	defer quiet.Stop()

	for {
		s.mu.Lock()
		idx := findPrompt(re, s.acc, after)
		if idx >= 0 {
			s.recordIdleTailLocked()
		}
		s.mu.Unlock()
		if idx >= 0 {
			return
		}

		select {
		case <-s.notify:
			// More output arrived: rescan, and restart the silence window.
			if !quiet.Stop() {
				select {
				case <-quiet.C:
				default:
				}
			}
			quiet.Reset(settleQuiet)
		case <-quiet.C:
			logging.Debug("shell went quiet without matching the prompt regex after a command")
			s.recordIdleTail()
			return
		case <-hardCap.C:
			logging.Debug("shell did not settle within the cap after a command")
			s.recordIdleTail()
			return
		}
	}
}

// wait blocks until fn reports completion, the timeout expires, ctx is cancelled, or the
// shell dies. Only one waiter exists at a time, which is what makes the single-slot
// notify channel sufficient.
func (s *Session) wait(ctx context.Context, timeout time.Duration, fn scanFn) (idx, extra int, err error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		s.mu.Lock()
		s.compactLocked()
		from := s.scanned - scanOverlap
		if from < 0 {
			from = 0
		}
		i, x, done := fn(s.acc, from)
		if !done {
			s.scanned = len(s.acc)
		}
		readErr := s.readErr
		s.mu.Unlock()

		if done {
			return i, x, nil
		}
		if readErr != nil {
			return 0, 0, fmt.Errorf("%w: %v", ErrDead, readErr)
		}

		select {
		case <-s.notify:
		case <-ctx.Done():
			return 0, 0, ctx.Err()
		case <-timer.C:
			return 0, 0, ErrTimeout
		}
	}
}

// compactLocked bounds memory for a command that produces enormous output by dropping the
// middle of the buffer. Only bytes the scanner has already passed are eligible, and the
// head is preserved so the returned output keeps its beginning as well as its end.
//
// The read itself is never stopped early: draining through to the marker is what keeps the
// session synchronised for the next caller.
func (s *Session) compactLocked() {
	if len(s.acc) <= s.accLimit {
		return
	}
	keepHead := s.cfg.MaxOutputBytes / 2
	safe := s.scanned - scanOverlap
	if safe <= keepHead {
		return
	}
	dropped := safe - keepHead
	s.acc = append(s.acc[:keepHead], s.acc[safe:]...)
	s.scanned -= dropped
	s.elided += dropped
	s.elideAt = keepHead
}

// regionUpTo assembles the output captured before idx, re-inserting a note where
// compaction dropped bytes. The bool reports whether anything was dropped: compaction is
// just as lossy as the final truncation, and a caller told only about the latter would
// believe it had the complete output when it did not.
func (s *Session) regionUpTo(idx int) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if idx > len(s.acc) {
		idx = len(s.acc)
	}
	if s.elided == 0 || s.elideAt > idx {
		out := make([]byte, idx)
		copy(out, s.acc[:idx])
		return out, false
	}
	note := []byte(fmt.Sprintf("\n...[%d bytes elided]...\n", s.elided))
	out := make([]byte, 0, idx+len(note))
	out = append(out, s.acc[:s.elideAt]...)
	out = append(out, note...)
	out = append(out, s.acc[s.elideAt:idx]...)
	return out, true
}

// --- small helpers ----------------------------------------------------------------

func (s *Session) resetRegion() {
	s.mu.Lock()
	s.acc, s.scanned, s.elided, s.elideAt = s.acc[:0], 0, 0, 0
	s.mu.Unlock()
	// Drop any stale wake-up so the next wait blocks until genuinely new data arrives.
	select {
	case <-s.notify:
	default:
	}
}

func (s *Session) writeLine(line string) error { return s.writeRaw([]byte(line + "\n")) }

func (s *Session) writeRaw(b []byte) error {
	s.procMu.Lock()
	ptmx := s.ptmx
	s.procMu.Unlock()
	if ptmx == nil {
		return ErrDead
	}
	if _, err := ptmx.Write(b); err != nil {
		return fmt.Errorf("failed writing to the shell: %w", err)
	}
	return nil
}

func (s *Session) promptRegex() *regexp.Regexp {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.prompt
}

func (s *Session) sentinelEnabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sentinel
}

func (s *Session) setSentinel(v bool) {
	s.mu.Lock()
	s.sentinel = v
	observer, snap := s.observer, s.statusLocked()
	s.mu.Unlock()

	if observer != nil {
		observer(snap)
	}
}

// SetStatusObserver registers a callback invoked whenever the snapshot Status returns
// changes — a state transition, but also a prompt regex or sentinel-framing change that
// leaves the state alone. It is how the status resource announces itself as updated.
//
// The callback receives a snapshot taken under the session lock and runs after that lock
// is released, so it sees a coherent Status and must not call back into the session.
//
// Each snapshot is internally consistent, but two of them are not ordered against each
// other: a mutation that snapshots first can be beaten to the callback by one that
// snapshots later. Commands are single-flighted, so in practice only the reader
// goroutine's death path can race anything. Callers that translate this into a "go
// re-read" signal, which is what resources/updated is, are unaffected; a caller that
// treated the snapshots as a stream and believed the last one would be wrong.
func (s *Session) SetStatusObserver(fn func(Status)) {
	s.mu.Lock()
	s.observer = fn
	s.mu.Unlock()
}

func (s *Session) setState(st State, lastErr string) {
	s.mu.Lock()
	// Death is announced once, with the cause that caused it. Close follows whatever
	// killed the session — a read error, a failed reconnect — and would otherwise
	// overwrite that diagnosis with "closed" and re-announce a transition that already
	// happened. main.go closes the session twice by design (see its shutdown path), so
	// this is the load-bearing half of that.
	if s.state == StateDead && st == StateDead {
		s.mu.Unlock()
		return
	}
	// Nothing else promotes a dead session but a reconnect, which spawns afresh and
	// resets the state itself.
	if s.state != StateDead || st == StateDead {
		s.state = st
	}
	if lastErr != "" {
		s.lastError = lastErr
	} else if st == StateReady {
		s.lastError = ""
	}
	observer, snap := s.observer, s.statusLocked()
	s.mu.Unlock()

	if observer != nil {
		observer(snap)
	}
}

func (s *Session) isDead() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state == StateDead
}

func (s *Session) reconnectedAlready() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reconnectUsed
}

func newNonce() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func trimTrailingNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
