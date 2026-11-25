package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestManager returns a Manager whose sessions are local `bash -i` shells, the same
// stand-in for a bastion the session tests use. Skipped under -short.
func newTestManager(t *testing.T, max int, idleTimeout time.Duration) *Manager {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping shell integration test in short mode")
	}
	m := NewManager(max, idleTimeout)
	t.Cleanup(m.CloseAll)
	return m
}

// startReady opens a session and waits for its background handshake to finish, which is
// what a caller of start_session does by polling list_sessions.
func startReady(t *testing.T, m *Manager, params map[string]string) string {
	t.Helper()
	id, err := m.Start("", testConfig(), params)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForState(t, m, id, StateReady)
	return id
}

// waitForState polls until a session reaches want. Transitions driven by the handshake
// goroutine are asynchronous, so there is nothing to synchronise on but the state itself.
func waitForState(t *testing.T, m *Manager, id string, want State) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		sess, ok := m.Get("", id)
		if !ok {
			t.Fatalf("session %s disappeared while waiting for %q", id, want)
		}
		st := sess.Status()
		if st.State == want.String() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("session %s state = %q, want %q (last_error: %q, transcript: %q)",
				id, st.State, want, st.LastError, sess.Transcript(2048))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestManagerOpensAndEndsASession(t *testing.T) {
	m := newTestManager(t, 4, 0)

	id := startReady(t, m, map[string]string{"host": "localhost"})
	if m.Len() != 1 {
		t.Fatalf("Len() = %d, want 1", m.Len())
	}

	sess, ok := m.Get("", id)
	if !ok {
		t.Fatal("Get did not find the session just started")
	}
	res, err := sess.Execute(context.Background(), "echo hello")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(res.Output, "hello") {
		t.Errorf("output = %q, want it to contain hello", res.Output)
	}

	// The parameters are echoed back so a listing says which target a session reached.
	infos := m.List("")
	if len(infos) != 1 {
		t.Fatalf("List() returned %d entries, want 1", len(infos))
	}
	if infos[0].Parameters["host"] != "localhost" {
		t.Errorf("listed parameters = %v, want host=localhost", infos[0].Parameters)
	}

	if !m.End("", id) {
		t.Error("End reported that the session was not found")
	}
	if m.Len() != 0 {
		t.Errorf("Len() = %d after End, want 0", m.Len())
	}
	if _, ok := m.Get("", id); ok {
		t.Error("Get still resolves an ended session id")
	}
	if m.End("", id) {
		t.Error("End reported success for an id it had already removed")
	}
}

// The cap exists because every session is a real privileged login, and the caller is a
// model that will otherwise keep opening them.
func TestManagerEnforcesTheSessionCap(t *testing.T) {
	m := newTestManager(t, 2, 0)

	first := startReady(t, m, nil)
	startReady(t, m, nil)

	_, err := m.Start("", testConfig(), nil)
	if err == nil {
		t.Fatal("expected the third Start to be refused")
	}
	if !errors.Is(err, ErrSessionLimit) {
		t.Errorf("error = %v, want it to wrap ErrSessionLimit", err)
	}

	// Ending one frees its slot rather than merely hiding it.
	m.End("", first)
	if _, err := m.Start("", testConfig(), nil); err != nil {
		t.Errorf("Start after End: %v", err)
	}
}

// A target that cannot be reached must not be able to wedge the server: dead sessions are
// kept so their cause can be read, but they hold no connection and must not consume a slot.
func TestManagerCapIgnoresDeadSessions(t *testing.T) {
	m := newTestManager(t, 1, 0)

	id := startReady(t, m, nil)
	sess, _ := m.Get("", id)

	// Kill the shell the way one really dies: pull the PTY out from under the reader.
	sess.closeProc()
	waitForState(t, m, id, StateDead)

	if live := m.Live(); live != 0 {
		t.Errorf("Live() = %d with one dead session, want 0", live)
	}
	if _, err := m.Start("", testConfig(), nil); err != nil {
		t.Errorf("a dead session consumed the only slot: %v", err)
	}
	// The dead one is still listed, so the model can see why it died.
	if m.Len() != 2 {
		t.Errorf("Len() = %d, want 2 (one dead, one live)", m.Len())
	}
}

// The point of the whole change: two sessions are two shells. Session already serialises
// its own commands through a capacity-1 slot, so this checks the manager did not
// accidentally introduce sharing between them.
func TestManagerKeepsSessionsIndependent(t *testing.T) {
	m := newTestManager(t, 4, 0)
	ctx := context.Background()

	a, b := startReady(t, m, nil), startReady(t, m, nil)
	sessA, _ := m.Get("", a)
	sessB, _ := m.Get("", b)

	// Compared against B's own starting directory rather than against a literal, since
	// both shells inherit this process's cwd and that cwd may itself sit under /tmp.
	pwd := func(sess *Session, label string) string {
		t.Helper()
		res, err := sess.Execute(ctx, "pwd")
		if err != nil {
			t.Fatalf("Execute pwd on %s: %v", label, err)
		}
		return strings.TrimSpace(res.Output)
	}
	beforeB := pwd(sessB, "B")

	if _, err := sessA.Execute(ctx, "cd /tmp"); err != nil {
		t.Fatalf("Execute cd on A: %v", err)
	}
	if _, err := sessA.Execute(ctx, "MARKER=session_a"); err != nil {
		t.Fatalf("Execute assignment on A: %v", err)
	}

	// Working directory persists on A...
	if got := pwd(sessA, "A"); got != "/tmp" {
		t.Errorf("A pwd = %q, want /tmp", got)
	}
	// ...and did not move B.
	if got := pwd(sessB, "B"); got != beforeB {
		t.Errorf("B pwd moved from %q to %q — the sessions are sharing a shell", beforeB, got)
	}

	res, err := sessB.Execute(ctx, "echo \"[${MARKER:-unset}]\"")
	if err != nil {
		t.Fatalf("Execute echo on B: %v", err)
	}
	if !strings.Contains(res.Output, "[unset]") {
		t.Errorf("B saw A's variable: %q", res.Output)
	}
}

// main.go hangs resource registration off these callbacks, so a missed add leaves a session
// with no status resource and a missed remove leaks one for a session that is gone.
func TestManagerLifecycleObserverFiresOnceEachWay(t *testing.T) {
	m := newTestManager(t, 4, 0)

	var mu sync.Mutex
	var added, removed []string
	m.SetLifecycleObserver(
		func(id, _ string, s *Session) {
			if s == nil {
				t.Error("onAdd received a nil session")
			}
			mu.Lock()
			added = append(added, id)
			mu.Unlock()
		},
		func(id, _ string) {
			mu.Lock()
			removed = append(removed, id)
			mu.Unlock()
		},
	)

	id := startReady(t, m, nil)

	mu.Lock()
	gotAdded := append([]string(nil), added...)
	mu.Unlock()
	if len(gotAdded) != 1 || gotAdded[0] != id {
		t.Fatalf("added = %v, want exactly [%s]", gotAdded, id)
	}

	m.End("", id)
	m.End("", id) // the second is a no-op and must not announce a second removal

	mu.Lock()
	gotRemoved := append([]string(nil), removed...)
	mu.Unlock()
	if len(gotRemoved) != 1 || gotRemoved[0] != id {
		t.Errorf("removed = %v, want exactly [%s]", gotRemoved, id)
	}
}

// end_session means end it. A caller asking to close a session is not asking to be blocked
// by whatever it happened to be running.
//
// What the abandoned command *returns* is deliberately not asserted, because it is the far
// end's choice rather than this code's. Closing the PTY makes the local bash SIGHUP its
// job, print "Hangup", and then go on to run the rest of the command line — including the
// sentinel printf — so the command frames cleanly and reports exit 129. A shell that dies
// outright instead yields ErrDead. Both are honest, and both are the point: the caller
// learns promptly that the command did not complete normally, rather than blocking until
// command_timeout.
func TestManagerEndCancelsACommandInFlight(t *testing.T) {
	m := newTestManager(t, 4, 0)
	id := startReady(t, m, nil)
	sess, _ := m.Get("", id)

	type outcome struct {
		res *Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := sess.Execute(context.Background(), "sleep 30")
		done <- outcome{res, err}
	}()

	// Let the command actually reach the shell before pulling it out.
	waitForState(t, m, id, StateBusy)

	ended := make(chan bool, 1)
	go func() { ended <- m.End("", id) }()

	select {
	case ok := <-ended:
		if !ok {
			t.Error("End reported that the session was not found")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("End blocked on the command in flight instead of cancelling it")
	}

	select {
	case got := <-done:
		// `sleep 30` completing normally would be exit 0 after thirty seconds. Anything
		// else — an error, or a signal status — means it was cut short, which is all this
		// needs to establish.
		if got.err == nil && got.res != nil && got.res.HasExit && got.res.ExitCode == 0 {
			t.Errorf("the abandoned command reported a clean exit 0: %+v", got.res)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the in-flight command never returned after its session was ended")
	}

	// The id is unusable either way, which is the part a caller of end_session relies on.
	if _, ok := m.Get("", id); ok {
		t.Error("Get still resolves a session that was ended")
	}
}

func TestManagerReapsIdleSessionsButNotBusyOnes(t *testing.T) {
	m := newTestManager(t, 4, 2*time.Second)

	idle := startReady(t, m, nil)
	busy := startReady(t, m, nil)

	busySess, _ := m.Get("", busy)
	go func() { _, _ = busySess.Execute(context.Background(), "sleep 8") }()
	waitForState(t, m, busy, StateBusy)

	// Polled through List rather than Get: Get stamps the session as used, so polling with
	// it would hold the very session under test permanently un-idle. That asymmetry is the
	// intended contract — a client checking list_sessions is not using a session — and it
	// is worth a test noticing.
	deadline := time.Now().Add(20 * time.Second)
	for listed(m, idle) {
		if time.Now().After(deadline) {
			t.Fatal("the idle session was never reaped")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// A command that outruns the idle timeout is exactly what must survive it: by the
	// lastUsed clock a long command looks perfectly idle, so the reaper has to consult the
	// session's state as well.
	if !listed(m, busy) {
		t.Error("the reaper closed a session with a command still running")
	}
}

// listed reports whether id is still registered, without marking it as used.
func listed(m *Manager, id string) bool {
	for _, in := range m.List("") {
		if in.ID == id {
			return true
		}
	}
	return false
}

// A prompt regex is only final once rendered, so an unusable one arrives here rather than
// at startup. It has to be an ordinary error a caller can read, not a panic or a session
// that exists but can never work.
func TestManagerStartRejectsAnUncompilablePromptRegex(t *testing.T) {
	m := NewManager(4, 0)
	t.Cleanup(m.CloseAll)

	cfg := testConfig()
	cfg.PromptRegex = "([unclosed"

	if _, err := m.Start("", cfg, nil); err == nil {
		t.Fatal("expected an error for a prompt regex that does not compile")
	}
	if m.Len() != 0 {
		t.Errorf("Len() = %d after a rejected Start, want 0", m.Len())
	}
}

func TestManagerCloseAllEndsEverything(t *testing.T) {
	m := newTestManager(t, 4, 0)

	startReady(t, m, nil)
	startReady(t, m, nil)
	if m.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", m.Len())
	}

	m.CloseAll()
	if m.Len() != 0 {
		t.Errorf("Len() = %d after CloseAll, want 0", m.Len())
	}
	// main.go calls it twice by design — deferred, plus explicitly before the "shutdown
	// complete" line — so a second pass must be a no-op rather than a panic on a closed
	// channel.
	m.CloseAll()
}

// newFlagFile returns a gate over a path in a fresh temp directory, plus functions to raise
// and clear the flag. Deliberately the real FlagFileGate over a real file rather than a
// stub: the interesting behaviour is the whole loop from a file appearing to a shell dying,
// and a stub would only prove the manager talks to whatever it is handed.
func newFlagFile(t *testing.T) (gate *FlagFileGate, raise, clear func()) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "disabled")
	return NewFlagFileGate(path),
		func() {
			t.Helper()
			if err := os.WriteFile(path, nil, 0o644); err != nil {
				t.Fatalf("raising the flag: %v", err)
			}
		},
		func() {
			t.Helper()
			if err := os.Remove(path); err != nil {
				t.Fatalf("clearing the flag: %v", err)
			}
		}
}

// The refusal has to reach the caller before anything is spawned. This needs no shell, since
// the gate is consulted ahead of session.New.
func TestManagerGateRefusesNewSessions(t *testing.T) {
	m := NewManager(4, 0)
	t.Cleanup(m.CloseAll)

	gate, raise, _ := newFlagFile(t)
	m.SetGate(gate, 0) // no poll loop: this is only about admission
	raise()

	_, err := m.Start("", testConfig(), nil)
	if err == nil {
		t.Fatal("Start succeeded while the disable flag was raised")
	}
	if !errors.Is(err, ErrDisabled) {
		t.Errorf("error = %v, want it to wrap ErrDisabled", err)
	}
	if m.Len() != 0 {
		t.Errorf("Len() = %d after a refused Start, want 0 — something was spawned anyway", m.Len())
	}
}

// The gate outranks the cap. Both refuse, but only one of them is true, and telling a caller
// to end sessions it cannot end — because they are already being torn down — sends it round
// a loop that cannot terminate.
func TestManagerGateOutranksTheSessionCap(t *testing.T) {
	m := NewManager(1, 0)
	t.Cleanup(m.CloseAll)

	gate, raise, _ := newFlagFile(t)
	m.SetGate(gate, 0)
	raise()

	_, err := m.Start("", testConfig(), nil)
	if errors.Is(err, ErrSessionLimit) {
		t.Errorf("error = %v, want the administrative reason rather than the cap", err)
	}
	if !errors.Is(err, ErrDisabled) {
		t.Errorf("error = %v, want it to wrap ErrDisabled", err)
	}
}

// The core of the feature: a flag raised mid-flight ages open sessions out, but never at the
// cost of a command already running. The busy session goes when its command finishes and not
// before, which is what makes this a controlled stop rather than a kill.
func TestManagerGateTerminatesIdleSessionsAndWaitsForBusyOnes(t *testing.T) {
	m := newTestManager(t, 4, 0)

	gate, raise, clear := newFlagFile(t)
	m.SetGate(gate, 100*time.Millisecond)

	idle := startReady(t, m, nil)
	busy := startReady(t, m, nil)

	busySess, _ := m.Get("", busy)
	commandDone := make(chan struct{})
	go func() {
		defer close(commandDone)
		_, _ = busySess.Execute(context.Background(), "sleep 4")
	}()
	waitForState(t, m, busy, StateBusy)

	raise()

	// Polled through List, not Get: Get stamps a session as used, and the point here is what
	// happens to sessions nobody is touching.
	waitUntilGone(t, m, idle, 10*time.Second, "the idle session was never terminated after the flag was raised")

	// ...and the busy one is still there, because its command is still running. Asserted
	// after the idle one is already gone, so the poll loop has demonstrably run.
	if !listed(m, busy) {
		t.Fatal("a session running a command was terminated instead of being left to finish")
	}

	<-commandDone
	waitUntilGone(t, m, busy, 10*time.Second, "the busy session was never terminated after its command finished")

	// And the far side of the switch: removing the file restores service with no restart.
	clear()
	if _, err := m.Start("", testConfig(), nil); err != nil {
		t.Errorf("Start after clearing the flag: %v", err)
	}
}

// waitUntilGone polls List until id is no longer registered.
func waitUntilGone(t *testing.T, m *Manager, id string, within time.Duration, msg string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for listed(m, id) {
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A session belongs to whoever opened it. To anyone else it must be indistinguishable from
// an id that was never issued — and a lookup by someone else must not count as use, or a
// caller probing for another's session could hold it open past the idle reaper forever.
func TestManagerScopesSessionsToTheirOwner(t *testing.T) {
	m := newTestManager(t, 4, 0)

	id, err := m.Start("alice", testConfig(), nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := m.Start("bob", testConfig(), nil); err != nil {
		t.Fatalf("Start: %v", err)
	}

	before := m.List("alice")
	if len(before) != 1 || before[0].ID != id {
		t.Fatalf("alice sees %+v, want exactly her own session", before)
	}
	if got := m.List(""); len(got) != 0 {
		t.Errorf("the empty owner sees %d sessions it did not open; it is an owner like any other, not a wildcard", len(got))
	}

	time.Sleep(5 * time.Millisecond)
	for _, who := range []string{"bob", "", "alice "} {
		if _, ok := m.Get(who, id); ok {
			t.Errorf("Get(%q) found alice's session", who)
		}
		if m.End(who, id) {
			t.Errorf("End(%q) ended alice's session", who)
		}
	}
	if after := m.List("alice"); len(after) != 1 || !after[0].LastUsed.Equal(before[0].LastUsed) {
		t.Errorf("a stranger's lookup changed alice's session: %+v", after)
	}

	if _, ok := m.Get("alice", id); !ok {
		t.Error("alice cannot find her own session")
	}
	if !m.End("alice", id) {
		t.Error("alice cannot end her own session")
	}
	// The cap and shutdown are the server's, not any owner's.
	if m.Len() != 1 {
		t.Errorf("Len = %d after alice ended hers, want bob's 1", m.Len())
	}
}
