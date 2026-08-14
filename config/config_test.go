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

// sshBlock is the ssh: portion alone, for tests that need to supply their own server:
// block rather than appending to minimal's (a second top-level server: key would be a
// duplicate YAML key, not a merge).
const sshBlock = `
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

	// use_sentinel defaults to on, and an omitted key must not read as false.
	if !cfg.SSH.UseSentinelValue() {
		t.Error("use_sentinel should default to true")
	}
	if cfg.SSH.AllowReconnect {
		t.Error("allow_reconnect should default to false")
	}

	// legacy_compat defaults to on (the inverse of the library's own default), and with
	// no explicit require_confirmation, confirmation follows it: MRTR has no legacy
	// equivalent, so a minimal config gets a usable run_bash for a legacy client rather
	// than one that requires a confirmation round trip it can never complete.
	if !cfg.LegacyCompatEnabled() {
		t.Error("legacy_compat should default to true")
	}
	if cfg.SSH.RequireConfirmationValue(cfg.LegacyCompatEnabled()) {
		t.Error("require_confirmation should default to false when legacy_compat is enabled")
	}
}

// The tri-state settings are the ones where "absent" and "false" must not be confused:
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
	if cfg.SSH.RequireConfirmationValue(cfg.LegacyCompatEnabled()) {
		t.Error("explicit require_confirmation: false was not honoured")
	}
	if !cfg.SSH.AllowReconnect {
		t.Error("allow_reconnect: true was not honoured")
	}
}

// An explicit require_confirmation: true must survive even though legacy_compat defaults
// to on — the operator asked for confirmation, and it still works for any caller that did
// negotiate 2026-07-28 (a legacy caller gets the documented MRTR refusal instead, which is
// its own explicit choice to accept once it sets this).
func TestLoadExplicitRequireConfirmationOverridesCompatDefault(t *testing.T) {
	cfg, err := Load(write(t, minimal+"  require_confirmation: true\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.LegacyCompatEnabled() {
		t.Fatal("legacy_compat should still default to true")
	}
	if !cfg.SSH.RequireConfirmationValue(cfg.LegacyCompatEnabled()) {
		t.Error("explicit require_confirmation: true was not honoured despite legacy_compat being on")
	}
}

func TestLoadLegacyCompatCanBeDisabled(t *testing.T) {
	body := "server:\n  mode: stdio\n  legacy_compat:\n    enabled: false\n" + sshBlock
	cfg, err := Load(write(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LegacyCompatEnabled() {
		t.Error("explicit legacy_compat.enabled: false was not honoured")
	}
	// With compat off, require_confirmation reverts to its original always-on default.
	if !cfg.SSH.RequireConfirmationValue(cfg.LegacyCompatEnabled()) {
		t.Error("require_confirmation should default to true when legacy_compat is disabled")
	}
}

func TestLoadLegacyCompatSessionTTL(t *testing.T) {
	body := "server:\n  mode: stdio\n  legacy_compat:\n    enabled: true\n    session_ttl: 45m\n" + sshBlock
	cfg, err := Load(write(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.LegacyCompatSessionTTL(), 45*time.Minute; got != want {
		t.Errorf("legacy_compat.session_ttl = %v, want %v", got, want)
	}
}

func TestLoadLegacyCompatSessionTTLDefaultsZero(t *testing.T) {
	cfg, err := Load(write(t, minimal))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.LegacyCompatSessionTTL(); got != 0 {
		t.Errorf("legacy_compat.session_ttl = %v, want 0 (compat.Overlay's own default)", got)
	}
}

func TestLoadRejectsBadLegacyCompatSessionTTL(t *testing.T) {
	body := "server:\n  mode: stdio\n  legacy_compat:\n    session_ttl: not-a-duration\n" + sshBlock
	if _, err := Load(write(t, body)); err == nil {
		t.Error("expected an error for an unparseable session_ttl")
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

// logging.file is read by this package, not the library, from the same logging: block
// the library reads level/format from.
func TestLoadParsesLoggingFile(t *testing.T) {
	cfg, err := Load(write(t, minimal+"logging:\n  file: /var/log/mcp-linux-over-ssh.log\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Logging.File != "/var/log/mcp-linux-over-ssh.log" {
		t.Errorf("logging.file = %q, want /var/log/mcp-linux-over-ssh.log", cfg.Logging.File)
	}
	// The library's own parse of the same block must not choke on the extra key.
	if cfg.Lib.Logging.Level != "info" {
		t.Errorf("logging.level = %q, want default info", cfg.Lib.Logging.Level)
	}
}

func TestLoadLoggingFileDefaultsEmpty(t *testing.T) {
	cfg, err := Load(write(t, minimal))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Logging.File != "" {
		t.Errorf("logging.file = %q, want empty (stderr)", cfg.Logging.File)
	}
}
