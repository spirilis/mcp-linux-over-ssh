package session

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// These exercise the real spawn/PTY/handshake/execute path against a local interactive
// shell. `bash -i` behaves like the remote shell the server actually talks to — it is a
// PTY session that prints a prompt and echoes — so it covers the same code with no bastion
// in the loop. Skipped under -short.
func newTestSession(t *testing.T, mutate func(*Config)) *Session {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping shell integration test in short mode")
	}

	cfg := Config{
		Command:         "bash --norc --noprofile -i",
		PromptRegex:     `(?m)^[^\n]*[$#]\s*$`,
		LoginTimeout:    20 * time.Second,
		CommandTimeout:  20 * time.Second,
		RecoveryTimeout: 10 * time.Second,
		Term:            "dumb",
		Cols:            500,
		Rows:            24,
		InitCommands:    []string{"stty -echo", "unset PROMPT_COMMAND"},
		UseSentinel:     true,
		MaxOutputBytes:  1 << 20,
		TranscriptBytes: 64 << 10,
	}
	if mutate != nil {
		mutate(&cfg)
	}

	sess, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := sess.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	if got := sess.Status().State; got != StateReady.String() {
		t.Fatalf("session state after Start = %q, want %q (transcript: %q)",
			got, StateReady, sess.Transcript(2048))
	}
	return sess
}

func TestSessionExecutesAndReportsExitCode(t *testing.T) {
	sess := newTestSession(t, nil)
	ctx := context.Background()

	res, err := sess.Execute(ctx, "echo hello")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Output != "hello" {
		t.Errorf("output = %q, want %q", res.Output, "hello")
	}
	if !res.HasExit || res.ExitCode != 0 {
		t.Errorf("exit = %d (has=%v), want 0", res.ExitCode, res.HasExit)
	}

	res, err = sess.Execute(ctx, "false")
	if err != nil {
		t.Fatalf("Execute(false): %v", err)
	}
	if !res.HasExit || res.ExitCode != 1 {
		t.Errorf("exit for `false` = %d (has=%v), want 1", res.ExitCode, res.HasExit)
	}

	// Both streams land in the output: the shell merges them onto the one PTY.
	res, err = sess.Execute(ctx, "echo out; echo err >&2")
	if err != nil {
		t.Fatalf("Execute(stderr): %v", err)
	}
	if !strings.Contains(res.Output, "out") || !strings.Contains(res.Output, "err") {
		t.Errorf("output = %q, want both stdout and stderr", res.Output)
	}
}

// The point of holding one shell open is that shell state survives between calls.
func TestSessionStatePersistsBetweenCommands(t *testing.T) {
	sess := newTestSession(t, nil)
	ctx := context.Background()

	if _, err := sess.Execute(ctx, "cd /tmp && export MARKER=persisted"); err != nil {
		t.Fatalf("Execute(cd): %v", err)
	}

	res, err := sess.Execute(ctx, "pwd; echo $MARKER")
	if err != nil {
		t.Fatalf("Execute(pwd): %v", err)
	}
	if !strings.Contains(res.Output, "/tmp") || !strings.Contains(res.Output, "persisted") {
		t.Errorf("output = %q, want the working directory and variable to have persisted", res.Output)
	}
}

func TestSessionMultilineOutput(t *testing.T) {
	sess := newTestSession(t, nil)

	res, err := sess.Execute(context.Background(), "printf 'a\\nb\\nc\\n'")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Output != "a\nb\nc" {
		t.Errorf("output = %q, want %q", res.Output, "a\nb\nc")
	}
}

// A command that outruns its timeout must not leave the shell mid-stream for whoever calls
// next: the session interrupts it, drains back to a prompt, and carries on.
func TestSessionRecoversFromTimeout(t *testing.T) {
	sess := newTestSession(t, func(c *Config) {
		c.CommandTimeout = 2 * time.Second
	})
	ctx := context.Background()

	_, err := sess.Execute(ctx, "sleep 30")
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("Execute(sleep 30) error = %v, want ErrTimeout", err)
	}

	if got := sess.Status().State; got != StateReady.String() {
		t.Fatalf("state after timeout = %q, want %q (transcript: %q)",
			got, StateReady, sess.Transcript(2048))
	}

	res, err := sess.Execute(ctx, "echo recovered")
	if err != nil {
		t.Fatalf("Execute after recovery: %v", err)
	}
	if res.Output != "recovered" {
		t.Errorf("output after recovery = %q, want %q", res.Output, "recovered")
	}
}

func TestSessionCancelledContextRecovers(t *testing.T) {
	sess := newTestSession(t, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	if _, err := sess.Execute(ctx, "sleep 30"); err == nil {
		t.Fatal("expected an error from the cancelled command")
	}

	res, err := sess.Execute(context.Background(), "echo after-cancel")
	if err != nil {
		t.Fatalf("Execute after cancellation: %v", err)
	}
	if res.Output != "after-cancel" {
		t.Errorf("output = %q, want %q", res.Output, "after-cancel")
	}
}

// The transport dispatches every request in its own goroutine, so concurrent tools/call is
// real. Output must not interleave or be attributed to the wrong caller.
func TestSessionSerializesConcurrentCommands(t *testing.T) {
	sess := newTestSession(t, nil)

	const n = 8
	var wg sync.WaitGroup
	results := make([]string, n)
	errs := make([]error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := sess.Execute(context.Background(), "echo id-"+string(rune('a'+i)))
			if err != nil {
				errs[i] = err
				return
			}
			results[i] = res.Output
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		want := "id-" + string(rune('a'+i))
		if results[i] != want {
			t.Errorf("goroutine %d output = %q, want %q", i, results[i], want)
		}
	}
}

// Enormous output is bounded in what comes back, but must still be consumed in full so the
// session stays synchronised.
func TestSessionTruncatesHugeOutputAndStaysUsable(t *testing.T) {
	sess := newTestSession(t, func(c *Config) {
		c.MaxOutputBytes = 4096
		c.CommandTimeout = 60 * time.Second
	})
	ctx := context.Background()

	res, err := sess.Execute(ctx, "for i in $(seq 1 20000); do echo line-$i-padding-padding-padding; done")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.Truncated {
		t.Error("expected the output to be reported as truncated")
	}
	if !res.HasExit || res.ExitCode != 0 {
		t.Errorf("exit = %d (has=%v), want 0", res.ExitCode, res.HasExit)
	}
	if !strings.Contains(res.Output, "line-1-") {
		t.Error("expected the head of the output to be preserved")
	}
	if !strings.Contains(res.Output, "line-20000-") {
		t.Error("expected the tail of the output to be preserved")
	}

	after, err := sess.Execute(ctx, "echo still-here")
	if err != nil {
		t.Fatalf("Execute after huge output: %v", err)
	}
	if after.Output != "still-here" {
		t.Errorf("output = %q, want %q; the session desynchronised", after.Output, "still-here")
	}
}

// Prompt-only framing is the fallback for a shell that cannot emit the sentinel.
func TestSessionPromptOnlyFraming(t *testing.T) {
	sess := newTestSession(t, func(c *Config) {
		c.UseSentinel = false
	})

	res, err := sess.Execute(context.Background(), "echo no-sentinel")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(res.Output, "no-sentinel") {
		t.Errorf("output = %q, want it to contain %q", res.Output, "no-sentinel")
	}
	if res.HasExit {
		t.Error("prompt-only framing cannot report an exit code")
	}
}

// A prompt regex that never matches leaves the session alive and repairable rather than
// dead, which is what makes tune_prompt_regex worth having.
func TestSessionAwaitingPromptIsRecoverableAtRuntime(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping shell integration test in short mode")
	}

	sess, err := New(Config{
		Command:         "bash --norc --noprofile -i",
		PromptRegex:     `THIS_WILL_NEVER_MATCH_ANYTHING`,
		LoginTimeout:    3 * time.Second,
		CommandTimeout:  10 * time.Second,
		RecoveryTimeout: 10 * time.Second,
		Term:            "dumb",
		Cols:            500,
		Rows:            24,
		UseSentinel:     true,
		MaxOutputBytes:  1 << 20,
		TranscriptBytes: 64 << 10,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := sess.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	if got := sess.Status().State; got != StateAwaitingPrompt.String() {
		t.Fatalf("state = %q, want %q", got, StateAwaitingPrompt)
	}
	if _, err := sess.Execute(context.Background(), "echo hi"); !errors.Is(err, ErrAwaitingPrompt) {
		t.Fatalf("Execute error = %v, want ErrAwaitingPrompt", err)
	}

	// What the model would do: check a candidate against the transcript, then install it.
	matches, err := sess.TestPromptRegex(`(?m)^[^\n]*[$#]\s*$`, 5)
	if err != nil {
		t.Fatalf("TestPromptRegex: %v", err)
	}
	if len(matches) == 0 {
		t.Fatalf("expected the corrected regex to match the transcript %q", sess.Transcript(2048))
	}

	if err := sess.SetPromptRegex(`(?m)^[^\n]*[$#]\s*$`); err != nil {
		t.Fatalf("SetPromptRegex: %v", err)
	}
	if err := sess.Resync(context.Background()); err != nil {
		t.Fatalf("Resync: %v", err)
	}
	if got := sess.Status().State; got != StateReady.String() {
		t.Fatalf("state after resync = %q, want %q", got, StateReady)
	}

	res, err := sess.Execute(context.Background(), "echo repaired")
	if err != nil {
		t.Fatalf("Execute after repair: %v", err)
	}
	if res.Output != "repaired" {
		t.Errorf("output = %q, want %q", res.Output, "repaired")
	}
}

// Regression: a healthy session's scrollback has each prompt followed on the same line by
// the next command's output, because no newline is emitted after a prompt. Testing a
// candidate regex against that scrollback made a perfectly good end-anchored pattern look
// like it matched nothing. Candidates are checked against idle tails — the buffer as the
// live matcher sees it — instead.
func TestPromptRegexTestableOnHealthySession(t *testing.T) {
	sess := newTestSession(t, nil)

	for _, cmd := range []string{"echo one", "echo two", "echo three"} {
		if _, err := sess.Execute(context.Background(), cmd); err != nil {
			t.Fatalf("Execute(%q): %v", cmd, err)
		}
	}

	matches, err := sess.TestPromptRegex(`(?m)^[^\n]*[$#]\s*$`, 5)
	if err != nil {
		t.Fatalf("TestPromptRegex: %v", err)
	}
	if len(matches) == 0 {
		t.Errorf("expected the live prompt regex to match after commands had run; transcript tail: %q",
			sess.Transcript(512))
	}

	none, err := sess.TestPromptRegex(`ZZZ_NEVER_MATCHES_ZZZ`, 5)
	if err != nil {
		t.Fatalf("TestPromptRegex: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("a non-matching regex should report no matches, got %v", none)
	}
}

// The status observer is what drives resources/updated for the status resource, so every
// change to the snapshot the resource serves has to reach it — including the ones that do
// not move the state, and the busy->ready edge at the end of a command, which Execute
// completes without going through setState.
func TestStatusObserverSeesEveryStatusChange(t *testing.T) {
	sess := newTestSession(t, nil)

	var mu sync.Mutex
	var seen []Status
	sess.SetStatusObserver(func(st Status) {
		mu.Lock()
		seen = append(seen, st)
		mu.Unlock()
	})

	if _, err := sess.Execute(context.Background(), "echo observed"); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Execute notifies from the calling goroutine, so everything it had to say has been
	// said by now. Judge the command edges on this prefix alone: a later notification
	// would report a ready session too, and would mask a missing one here.
	mu.Lock()
	duringExec := append([]Status(nil), seen...)
	mu.Unlock()

	var sawBusy, sawReady bool
	for _, st := range duringExec {
		switch st.State {
		case StateBusy.String():
			sawBusy = true
		case StateReady.String():
			sawReady = sawReady || st.CommandsRun > 0
		}
	}
	if !sawBusy {
		t.Errorf("observer never saw the session go busy; snapshots: %+v", duringExec)
	}
	if !sawReady {
		t.Errorf("observer never saw the session return to ready with the command counted; snapshots: %+v", duringExec)
	}

	const newRegex = `(?m)^[^\n]*[$#][ ]*$`
	if err := sess.SetPromptRegex(newRegex); err != nil {
		t.Fatalf("SetPromptRegex: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(seen) == len(duringExec) {
		t.Fatalf("installing a prompt regex notified nobody; snapshots: %+v", seen)
	}
	if last := seen[len(seen)-1]; last.PromptRegex != newRegex {
		t.Errorf("last snapshot's prompt regex = %q, want %q", last.PromptRegex, newRegex)
	}
}

// Close runs after whatever actually killed the session, so it must not overwrite the
// cause or announce a death the subscriber has already been told about.
func TestDeathIsAnnouncedOnceWithItsOriginalCause(t *testing.T) {
	sess := newTestSession(t, nil)

	var mu sync.Mutex
	var deaths []Status
	sess.SetStatusObserver(func(st Status) {
		if st.State != StateDead.String() {
			return
		}
		mu.Lock()
		deaths = append(deaths, st)
		mu.Unlock()
	})

	// Pull the PTY out from under the reader goroutine, which is how a session dies for
	// real: the shell goes away and the next read fails.
	sess.closeProc()

	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(deaths)
		mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the reader goroutine never announced the session's death")
		}
		time.Sleep(5 * time.Millisecond)
	}

	mu.Lock()
	cause := deaths[0].LastError
	mu.Unlock()
	if cause == "" || cause == "closed" {
		t.Errorf("death was announced with cause %q, want the read error that caused it", cause)
	}

	// Close twice: the first follows a death that has already been announced, the second
	// is the redundant one main.go makes by design.
	for i := 0; i < 2; i++ {
		if err := sess.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(deaths) != 1 {
		t.Errorf("death announced %d times, want once; snapshots: %+v", len(deaths), deaths)
	}
	if got := sess.Status().LastError; got != cause {
		t.Errorf("last_error after Close = %q, want the original cause %q", got, cause)
	}
}

func TestSessionRejectsBadPromptRegex(t *testing.T) {
	if _, err := New(Config{Command: "true", PromptRegex: "([unclosed"}); err == nil {
		t.Error("expected New to reject a prompt regex that does not compile")
	}
}
