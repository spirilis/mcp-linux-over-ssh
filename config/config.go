// Package config loads mcp-linux-over-ssh's configuration.
//
// The same YAML file feeds two parsers: generic-go-mcp's own config.Load reads the
// server: and logging: blocks, and this package reads the ssh: block, the actor_jwt:,
// static_tokens: and tls: blocks that only matter in http mode (see http.go), plus a couple of fields the
// library's logging: block doesn't have room for (see LoggingConfig). The
// library's config.Config has no extension point, so a second unmarshal of the same file
// is the least surprising way to carry our own settings.
package config

import (
	"fmt"
	"os"
	"regexp"
	"time"

	libconfig "github.com/spirilis/generic-go-mcp/config"
	"gopkg.in/yaml.v3"
)

// SSHConfig describes the shell sessions this server drives. One session is one SSH
// command, spawned on demand by start_session and run exactly once; several may be live at
// a time, each independent of the others.
type SSHConfig struct {
	// Command is the shell command line that opens a session, run via `sh -c`. It is a Go
	// text/template rendered per session against the values start_session was called with;
	// with no ssh.parameters declared it is simply a constant. Each rendered command is
	// executed exactly once, for the life of the session it opened.
	//
	// SECURITY: this string reaches `sh -c` on the host running this server, so what a
	// parameter is permitted to interpolate into it is the difference between choosing a
	// target and choosing a command. See ParameterConfig.Pattern.
	Command string `yaml:"command"`

	// PromptRegex matches the remote shell's prompt. It frames the initial login and
	// backstops recovery; per-command framing uses the sentinel when available. Also a
	// template, so a prompt that embeds the target's own hostname or username can be
	// matched exactly.
	PromptRegex string `yaml:"prompt_regex"`

	// Parameters declares the values start_session accepts and interpolates into Command
	// and PromptRegex. Empty means the two templates are constants and start_session takes
	// no arguments.
	Parameters []ParameterConfig `yaml:"parameters"`

	// MaxSessions caps how many live sessions may exist at once. Sessions that have died
	// do not count, so a failing target cannot wedge the server by filling the cap.
	MaxSessions int `yaml:"max_sessions"`

	// IdleTimeout closes sessions that have gone this long without running a command.
	// Zero (the default) never reaps: on a PAM/PSM bastion, tearing down a privileged
	// session someone is still reasoning about is worse than keeping it, and a stdio
	// server's sessions are already bounded by the life of its client. That argument does
	// not survive server.mode: http, where no client lifetime bounds anything — there a
	// positive value is required (see validateTransport).
	IdleTimeout time.Duration `yaml:"idle_timeout"`

	// DisableFlagFile is an administrative kill switch: while a file exists at this path,
	// start_session and run_bash refuse with an error saying so, and sessions already open
	// are terminated as soon as they are not mid-command. Removing the file restores normal
	// service with no restart. Empty (the default) means no such check exists at all.
	//
	// Only the file's existence matters; its contents are never read. A relative path is
	// resolved against the working directory at startup — see session.NewFlagFileGate for
	// why that is done once rather than per check.
	DisableFlagFile string `yaml:"disable_flag_file"`

	// DisableFlagPollInterval is how often the flag file is re-checked for the purpose of
	// terminating sessions that are already open. It does not delay the refusal of new work:
	// start_session and run_bash stat the file themselves, so they can never act on a stale
	// answer. This bounds only how long an open, quiet session outlives the flag appearing.
	DisableFlagPollInterval time.Duration `yaml:"disable_flag_poll_interval"`

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
	// an explicit `false` is distinguishable from an omitted key. Default is true unless
	// legacy MCP compatibility is on, in which case it is false — see
	// RequireConfirmationValue.
	RequireConfirmation *bool `yaml:"require_confirmation"`
}

// LoggingConfig carries the logging: fields the library's own LoggingConfig has no room
// for. It is unmarshalled from the same logging: block as the library's level/format —
// yaml.Unmarshal ignores keys a struct doesn't declare, so the two parsers coexist without
// either seeing an "unknown field" error.
type LoggingConfig struct {
	// File redirects logging from stderr (the default) to this path, opened for append
	// and created if missing. Empty means stderr, unchanged from before this field
	// existed.
	File string `yaml:"file"`
}

// Config is the whole file: the library's half plus ours.
type Config struct {
	Lib     *libconfig.Config
	SSH     SSHConfig
	Logging LoggingConfig

	// ActorJWT admits kagent agents by their Substrate actor JWT. StaticTokens admit callers
	// by a fixed bearer token. http mode needs at least one of the two; stdio rejects both.
	ActorJWT     *ActorJWTConfig
	StaticTokens []StaticTokenConfig
	// TLS makes the HTTP listener serve HTTPS itself. http mode only.
	TLS *TLSConfig

	// Renderer turns start_session's arguments into one session's command and prompt
	// regex. Built by Load so a template that references an undeclared parameter is a
	// startup failure rather than a first-call one.
	Renderer *Renderer
}

type fileEnvelope struct {
	SSH          SSHConfig           `yaml:"ssh"`
	Logging      LoggingConfig       `yaml:"logging"`
	ActorJWT     *ActorJWTConfig     `yaml:"actor_jwt"`
	StaticTokens []StaticTokenConfig `yaml:"static_tokens"`
	TLS          *TLSConfig          `yaml:"tls"`
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
	DefaultMaxSessions     = 8

	// DefaultDisableFlagPollInterval is how quickly an open, quiet session notices the
	// disable flag file appearing. Only meaningful when ssh.disable_flag_file is set. Five
	// seconds is a stat every five seconds against a promptness an operator reaching for a
	// kill switch will accept; the refusal of *new* work is immediate regardless.
	DefaultDisableFlagPollInterval = 5 * time.Second
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

	// The library defaults legacy MCP compatibility to off; this server inverts that
	// default (see LegacyCompatEnabled), so an omitted block means "on" here rather than
	// falling through to the library's off. An explicit server.legacy_compat.enabled in
	// the file, true or false, always wins over this.
	if lib.Server.LegacyCompat == nil {
		lib.Server.LegacyCompat = &libconfig.LegacyCompatConfig{Enabled: true}
	}
	if lib.Server.LegacyCompat.SessionTTL != "" {
		if _, err := time.ParseDuration(lib.Server.LegacyCompat.SessionTTL); err != nil {
			return nil, fmt.Errorf("server.legacy_compat.session_ttl does not parse: %w", err)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var env fileEnvelope
	if err := yaml.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	cfg := &Config{Lib: lib, SSH: env.SSH, Logging: env.Logging, ActorJWT: env.ActorJWT,
		StaticTokens: env.StaticTokens, TLS: env.TLS}
	if err := cfg.SSH.applyDefaults(); err != nil {
		return nil, err
	}
	if err := cfg.validateTransport(); err != nil {
		return nil, err
	}
	if cfg.Renderer, err = NewRenderer(cfg.SSH); err != nil {
		return nil, err
	}
	return cfg, nil
}

// UseSentinelValue reports whether per-command sentinel markers are enabled.
func (c *SSHConfig) UseSentinelValue() bool {
	return c.UseSentinel == nil || *c.UseSentinel
}

// RequireConfirmationValue reports whether every execution needs an MRTR confirmation. An
// explicit require_confirmation in the config always wins. Left unset, the default tracks
// legacyCompatEnabled: MRTR (mcp.ToolRequest.NeedInput) has no legacy-MCP equivalent — a
// pre-2026-07-28 client gets a visible isError refusal on every run_bash call instead of the
// confirmation round trip (see compat.Overlay's result downgrading) — so confirmation
// defaults off while compat is on, and on otherwise, same as before this field existed.
func (c *SSHConfig) RequireConfirmationValue(legacyCompatEnabled bool) bool {
	if c.RequireConfirmation != nil {
		return *c.RequireConfirmation
	}
	return !legacyCompatEnabled
}

// LegacyCompatEnabled reports whether the legacy (2025-11-25 and earlier) MCP
// compatibility overlay (compat.Overlay) is active. On by default — the inverse of the
// library's own default — since most MCP clients as of this writing (Claude Desktop, most
// IDEs) do not yet speak 2026-07-28, and this server exists to keep working with the
// clients already deployed against it. Set server.legacy_compat.enabled: false once every
// client this server serves has moved off the legacy revisions.
func (c *Config) LegacyCompatEnabled() bool {
	return c.Lib.Server.LegacyCompat.Enabled
}

// LegacyCompatSessionTTL is server.legacy_compat.session_ttl, parsed. Load has already
// validated it parses; an empty string returns zero, which lets compat.Overlay apply its
// own 30-minute default.
func (c *Config) LegacyCompatSessionTTL() time.Duration {
	if c.Lib.Server.LegacyCompat.SessionTTL == "" {
		return 0
	}
	d, _ := time.ParseDuration(c.Lib.Server.LegacyCompat.SessionTTL)
	return d
}

func (c *SSHConfig) applyDefaults() error {
	if c.Command == "" {
		return fmt.Errorf("ssh.command is required")
	}
	if c.PromptRegex == "" {
		return fmt.Errorf("ssh.prompt_regex is required")
	}
	// With no parameters the regex is its own final form, so compile it now and fail at
	// startup as this server always has. Once it is a template, the concrete pattern
	// differs per session and can only be compiled when start_session renders it — where
	// a failure is a clean tool error rather than a dead server.
	if len(c.Parameters) == 0 {
		if _, err := regexp.Compile(c.PromptRegex); err != nil {
			return fmt.Errorf("ssh.prompt_regex does not compile: %w", err)
		}
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
	if c.MaxSessions <= 0 {
		c.MaxSessions = DefaultMaxSessions
	}
	if c.IdleTimeout < 0 {
		c.IdleTimeout = 0
	}
	if c.DisableFlagPollInterval <= 0 {
		c.DisableFlagPollInterval = DefaultDisableFlagPollInterval
	}
	return nil
}
