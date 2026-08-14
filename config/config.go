// Package config loads mcp-linux-over-ssh's configuration.
//
// The same YAML file feeds two parsers: generic-go-mcp's own config.Load reads the
// server: and logging: blocks, and this package reads the ssh: block. The library's
// config.Config has no extension point, so a second unmarshal of the same file is the
// least surprising way to carry our own settings.
package config

import (
	"fmt"
	"os"
	"regexp"
	"time"

	libconfig "github.com/spirilis/generic-go-mcp/config"
	"gopkg.in/yaml.v3"
)

// SSHConfig describes the single long-lived shell session this server drives.
type SSHConfig struct {
	// Command is the shell command line that opens the session, run via `sh -c`.
	// It is executed exactly once for the process lifetime.
	Command string `yaml:"command"`

	// PromptRegex matches the remote shell's prompt. It frames the initial login and
	// backstops recovery; per-command framing uses the sentinel when available.
	PromptRegex string `yaml:"prompt_regex"`

	LoginTimeout    time.Duration `yaml:"login_timeout"`
	CommandTimeout  time.Duration `yaml:"command_timeout"`
	RecoveryTimeout time.Duration `yaml:"recovery_timeout"`

	Term string `yaml:"term"`
	Cols uint16 `yaml:"cols"`
	Rows uint16 `yaml:"rows"`

	// InitCommands run once after login, before the session is declared ready.
	InitCommands []string `yaml:"init_commands"`

	// UseSentinel enables per-command end markers carrying $?. Pointer so an explicit
	// `false` is distinguishable from an omitted key (default true).
	UseSentinel *bool `yaml:"use_sentinel"`

	MaxOutputBytes  int `yaml:"max_output_bytes"`
	TranscriptBytes int `yaml:"transcript_bytes"`

	// AllowReconnect permits re-running Command once if the session dies. Off by
	// default: the whole premise of this server is that the command cannot simply be
	// re-run at will.
	AllowReconnect bool `yaml:"allow_reconnect"`

	// RequireConfirmation gates every execution behind an MRTR elicitation. Pointer so
	// an explicit `false` is distinguishable from an omitted key (default true).
	RequireConfirmation *bool `yaml:"require_confirmation"`
}

// Config is the whole file: the library's half plus ours.
type Config struct {
	Lib *libconfig.Config
	SSH SSHConfig
}

type fileEnvelope struct {
	SSH SSHConfig `yaml:"ssh"`
}

// Defaults applied to any field the file leaves unset.
const (
	DefaultLoginTimeout    = 60 * time.Second
	DefaultCommandTimeout  = 300 * time.Second
	DefaultRecoveryTimeout = 15 * time.Second
	DefaultTerm            = "dumb"
	DefaultCols            = 500
	DefaultRows            = 24
	DefaultMaxOutputBytes  = 1 << 20 // 1 MiB
	DefaultTranscriptBytes = 256 << 10
)

// DefaultInitCommands quiet the remote shell down before we start scraping it.
// `stty -echo` is the clean fix for command echo on a PTY; PROMPT_COMMAND can emit
// arbitrary text between the output and the prompt.
var DefaultInitCommands = []string{"stty -echo", "unset PROMPT_COMMAND"}

// Load reads path and returns both halves of the configuration.
func Load(path string) (*Config, error) {
	lib, err := libconfig.Load(path)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var env fileEnvelope
	if err := yaml.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("failed to parse ssh config: %w", err)
	}

	cfg := &Config{Lib: lib, SSH: env.SSH}
	if err := cfg.SSH.applyDefaults(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// UseSentinelValue reports whether per-command sentinel markers are enabled.
func (c *SSHConfig) UseSentinelValue() bool {
	return c.UseSentinel == nil || *c.UseSentinel
}

// RequireConfirmationValue reports whether every execution needs an MRTR confirmation.
func (c *SSHConfig) RequireConfirmationValue() bool {
	return c.RequireConfirmation == nil || *c.RequireConfirmation
}

func (c *SSHConfig) applyDefaults() error {
	if c.Command == "" {
		return fmt.Errorf("ssh.command is required")
	}
	if c.PromptRegex == "" {
		return fmt.Errorf("ssh.prompt_regex is required")
	}
	if _, err := regexp.Compile(c.PromptRegex); err != nil {
		return fmt.Errorf("ssh.prompt_regex does not compile: %w", err)
	}

	if c.LoginTimeout <= 0 {
		c.LoginTimeout = DefaultLoginTimeout
	}
	if c.CommandTimeout <= 0 {
		c.CommandTimeout = DefaultCommandTimeout
	}
	if c.RecoveryTimeout <= 0 {
		c.RecoveryTimeout = DefaultRecoveryTimeout
	}
	if c.Term == "" {
		c.Term = DefaultTerm
	}
	if c.Cols == 0 {
		c.Cols = DefaultCols
	}
	if c.Rows == 0 {
		c.Rows = DefaultRows
	}
	if c.InitCommands == nil {
		c.InitCommands = append([]string(nil), DefaultInitCommands...)
	}
	if c.MaxOutputBytes <= 0 {
		c.MaxOutputBytes = DefaultMaxOutputBytes
	}
	if c.TranscriptBytes < 0 {
		c.TranscriptBytes = 0
	} else if c.TranscriptBytes == 0 {
		c.TranscriptBytes = DefaultTranscriptBytes
	}
	return nil
}
