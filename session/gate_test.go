package session

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These need no shell, so unlike most of this package they run under -short.

func TestFlagFileGateBlocksOnlyWhileTheFileExists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disabled")
	g := NewFlagFileGate(path)

	if err := g.Blocked(); err != nil {
		t.Fatalf("Blocked() with no flag file = %v, want nil", err)
	}

	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("creating the flag file: %v", err)
	}
	err := g.Blocked()
	if err == nil {
		t.Fatal("Blocked() = nil with the flag file present")
	}
	if !errors.Is(err, ErrDisabled) {
		t.Errorf("error = %v, want it to wrap ErrDisabled", err)
	}

	// The whole point of the control is that it is reversible without a restart, so the
	// same gate object has to answer differently once the file goes away.
	if err := os.Remove(path); err != nil {
		t.Fatalf("removing the flag file: %v", err)
	}
	if err := g.Blocked(); err != nil {
		t.Errorf("Blocked() after removing the flag file = %v, want nil", err)
	}
}

// An empty file is the ordinary case — `touch` is how this gets used — but the gate must key
// on existence alone, or a control that looks armed silently is not.
func TestFlagFileGateIgnoresTheFileContents(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name string
		body string
	}{
		{"empty", ""},
		{"reason text", "maintenance window, back at 09:00\n"},
		{"the word false", "false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "-"))
			if err := os.WriteFile(path, []byte(tc.body), 0o644); err != nil {
				t.Fatalf("creating the flag file: %v", err)
			}
			if err := NewFlagFileGate(path).Blocked(); err == nil {
				t.Errorf("Blocked() = nil for a flag file containing %q", tc.body)
			}
		})
	}
}

// A directory is a perfectly good flag: `mkdir` is as available as `touch`, and stat does
// not care. Worth pinning so nobody "fixes" this into a regular-file check.
func TestFlagFileGateAcceptsADirectoryAsTheFlag(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disabled.d")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("creating the flag directory: %v", err)
	}
	if err := NewFlagFileGate(path).Blocked(); err == nil {
		t.Error("Blocked() = nil for a flag path that is a directory")
	}
}

// The error text goes straight to an MCP client whose reader is a model, so it has to say
// what happened and that a person has to fix it — not merely that something failed.
func TestFlagFileGateExplainsItselfAndNamesTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disabled")
	g := NewFlagFileGate(path)
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("creating the flag file: %v", err)
	}

	msg := g.Blocked().Error()
	for _, want := range []string{"disabled by flag file", "requires user intervention to proceed", path} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not contain %q", msg, want)
		}
	}
}

// A relative path is resolved once, at construction. Otherwise the operator who created the
// file and the server looking for it can disagree about which file they meant, since the
// working directory belongs to whichever MCP client launched the process.
func TestFlagFileGateResolvesItsPathToAbsolute(t *testing.T) {
	g := NewFlagFileGate("relative/disabled")
	if !filepath.IsAbs(g.Path()) {
		t.Errorf("Path() = %q, want an absolute path", g.Path())
	}
}

// Fail closed. A check that cannot be performed is not evidence that the operator wants this
// server running: the flag exists to stop something reaching privileged bastion sessions,
// and "I could not tell" has to mean stop.
func TestFlagFileGateFailsClosedWhenItCannotCheck(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so stat cannot be made to fail this way")
	}

	dir := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	g := NewFlagFileGate(filepath.Join(dir, "disabled"))
	if err := g.Blocked(); err != nil {
		t.Fatalf("Blocked() before the directory was locked = %v, want nil", err)
	}

	// No execute bit: stat on anything inside now returns EACCES rather than ENOENT.
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	err := g.Blocked()
	if err == nil {
		t.Fatal("Blocked() = nil when the flag file could not be checked; the gate failed open")
	}
	if !errors.Is(err, ErrDisabled) {
		t.Errorf("error = %v, want it to wrap ErrDisabled", err)
	}
	if !strings.Contains(err.Error(), "could not be checked") {
		t.Errorf("error %q does not say the check itself failed, so an operator cannot tell this apart from a raised flag", err)
	}
}
