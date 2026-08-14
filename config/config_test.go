package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

const minimal = `
server:
  mode: stdio
ssh:
  command: "ssh -tt user@bastion"
  prompt_regex: '(?m)^[^\n]*[$#]\s*$'
`

func TestLoadAppliesDefaults(t *testing.T) {
	cfg, err := Load(write(t, minimal))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Lib.Server.Mode != "stdio" {
		t.Errorf("mode = %q, want stdio", cfg.Lib.Server.Mode)
	}
	if cfg.SSH.LoginTimeout != DefaultLoginTimeout {
		t.Errorf("login_timeout = %v, want %v", cfg.SSH.LoginTimeout, DefaultLoginTimeout)
	}
	if cfg.SSH.Term != DefaultTerm {
		t.Errorf("term = %q, want %q", cfg.SSH.Term, DefaultTerm)
	}
	if cfg.SSH.Cols != DefaultCols {
		t.Errorf("cols = %d, want %d", cfg.SSH.Cols, DefaultCols)
	}
	if cfg.SSH.MaxOutputBytes != DefaultMaxOutputBytes {
		t.Errorf("max_output_bytes = %d, want %d", cfg.SSH.MaxOutputBytes, DefaultMaxOutputBytes)
	}
	if len(cfg.SSH.InitCommands) != len(DefaultInitCommands) {
		t.Errorf("init_commands = %v, want %v", cfg.SSH.InitCommands, DefaultInitCommands)
	}

	// Both default to on, and an omitted key must not read as false.
	if !cfg.SSH.UseSentinelValue() {
		t.Error("use_sentinel should default to true")
	}
	if !cfg.SSH.RequireConfirmationValue() {
		t.Error("require_confirmation should default to true")
	}
	if cfg.SSH.AllowReconnect {
		t.Error("allow_reconnect should default to false")
	}
}

// The two tri-state settings are the ones where "absent" and "false" must not be confused:
// silently defaulting an explicit `false` back to true would re-enable a confirmation the
// operator deliberately turned off.
func TestLoadHonoursExplicitFalse(t *testing.T) {
	cfg, err := Load(write(t, minimal+`
  use_sentinel: false
  require_confirmation: false
  allow_reconnect: true
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SSH.UseSentinelValue() {
		t.Error("explicit use_sentinel: false was not honoured")
	}
	if cfg.SSH.RequireConfirmationValue() {
		t.Error("explicit require_confirmation: false was not honoured")
	}
	if !cfg.SSH.AllowReconnect {
		t.Error("allow_reconnect: true was not honoured")
	}
}

func TestLoadParsesDurations(t *testing.T) {
	cfg, err := Load(write(t, minimal+`
  login_timeout: 90s
  command_timeout: 10m
  recovery_timeout: 5s
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SSH.LoginTimeout != 90*time.Second {
		t.Errorf("login_timeout = %v, want 90s", cfg.SSH.LoginTimeout)
	}
	if cfg.SSH.CommandTimeout != 10*time.Minute {
		t.Errorf("command_timeout = %v, want 10m", cfg.SSH.CommandTimeout)
	}
	if cfg.SSH.RecoveryTimeout != 5*time.Second {
		t.Errorf("recovery_timeout = %v, want 5s", cfg.SSH.RecoveryTimeout)
	}
}

func TestLoadRejectsIncompleteConfig(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"no ssh block", "server:\n  mode: stdio\n"},
		{"no command", "ssh:\n  prompt_regex: 'x'\n"},
		{"no prompt regex", "ssh:\n  command: 'ssh host'\n"},
		{"prompt regex does not compile", "ssh:\n  command: 'ssh host'\n  prompt_regex: '([unclosed'\n"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Load(write(t, tc.body)); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Error("expected an error for a missing config file")
	}
}

// A transcript can be switched off outright, which must survive the defaulting pass.
func TestLoadTranscriptDisabled(t *testing.T) {
	cfg, err := Load(write(t, minimal+"  transcript_bytes: -1\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SSH.TranscriptBytes != 0 {
		t.Errorf("transcript_bytes = %d, want 0 (disabled)", cfg.SSH.TranscriptBytes)
	}
}
