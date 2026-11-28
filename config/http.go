package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// ActorJWTConfig configures the credential kagent agents present: a Substrate actor JWT,
// verified against THIS cluster's issuer. There is deliberately no way to trust another
// cluster's issuer.
type ActorJWTConfig struct {
	Enabled bool `yaml:"enabled"`
	// Issuer is the local Substrate issuer; default https://idp.ate-system.svc.
	Issuer string `yaml:"issuer"`
	// Audience must equal an entry of the RemoteMCPServer's spec.actorJwt.audiences.
	Audience string `yaml:"audience"`
	// CAFile is the local servicedns CA bundle the issuer's TLS certificate must chain to
	// (normally a projected ClusterTrustBundle). Required; the system roots are never used.
	CAFile string `yaml:"ca_file"`
	// AllowedAgents are "namespace/name" patterns; either half may be a glob. Empty refuses
	// every agent — the server starts, and answers 403 to all of them.
	AllowedAgents    []string               `yaml:"allowed_agents"`
	KagentController KagentControllerConfig `yaml:"kagent_controller"`
}

// StaticTokenConfig names one static bearer token. The token itself lives in TokenFile (a
// mounted Secret key), never in this file.
type StaticTokenConfig struct {
	// Name is the caller: principal static:<name>, which owns the sessions the token opens.
	Name string `yaml:"name"`
	// TokenFile holds the token; surrounding whitespace is trimmed. Read at start.
	TokenFile string `yaml:"token_file"`
	// DiscoverOnly lets the token list tools, resources and skills, and nothing else.
	DiscoverOnly bool `yaml:"discover_only"`
}

// UnmarshalYAML refuses keys a static_tokens entry does not have. The rest of the file is
// read leniently, but here a typo fails open: `discoverOnly: true` (the chart's spelling)
// would otherwise load a catalog UI's token with full access to every shell.
func (t *StaticTokenConfig) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: a static_tokens entry must be a map (name, token_file, discover_only)", n.Line)
	}
	for i := 0; i < len(n.Content); i += 2 {
		switch k := n.Content[i].Value; k {
		case "name", "token_file", "discover_only":
		default:
			return fmt.Errorf("line %d: static_tokens: unknown key %q (name, token_file, discover_only)", n.Content[i].Line, k)
		}
	}
	type plain StaticTokenConfig
	return n.Decode((*plain)(t))
}

// StaticToken is one static token as read from its file.
type StaticToken struct {
	Name         string
	Value        string
	DiscoverOnly bool
}

// ReadStaticTokens reads each static token's file. A missing or empty file is an error: a
// Secret key rendered empty must not leave the server starting without the credential the
// config names.
func (c *Config) ReadStaticTokens() ([]StaticToken, error) {
	out := make([]StaticToken, 0, len(c.StaticTokens))
	for _, t := range c.StaticTokens {
		b, err := os.ReadFile(t.TokenFile)
		if err != nil {
			return nil, fmt.Errorf("static token %q: %w", t.Name, err)
		}
		v := strings.TrimSpace(string(b))
		if v == "" {
			return nil, fmt.Errorf("static token %q: %s is empty", t.Name, t.TokenFile)
		}
		out = append(out, StaticToken{Name: t.Name, Value: v, DiscoverOnly: t.DiscoverOnly})
	}
	return out, nil
}

// ActorJWTEnabled reports whether kagent agents are admitted by actor JWT.
func (c *Config) ActorJWTEnabled() bool { return c.ActorJWT != nil && c.ActorJWT.Enabled }

// KagentControllerConfig locates the local kagent controller's gRPC listener, which resolves
// an actor (one conversation's sandbox) to the Agent it belongs to.
type KagentControllerConfig struct {
	// Address defaults to kagent-controller.kagent:8083.
	Address string `yaml:"address"`
	TLS     bool   `yaml:"tls"`
	CAFile  string `yaml:"ca_file"`
}

// TLSConfig makes the HTTP listener serve HTTPS itself. Both files are re-read when they
// change — cert-manager and CSI drivers rotate them in place — so a renewal needs no restart.
// Where the files come from is not this server's business: a Secret, a CSI volume, or a
// SPIFFE helper writing an X.509 SVID to disk all look the same from here.
type TLSConfig struct {
	Enabled  bool   `yaml:"enabled"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

// Defaults for ActorJWTConfig.
const (
	DefaultActorIssuer      = "https://idp.ate-system.svc"
	DefaultKagentController = "kagent-controller.kagent:8083"
)

// Transport modes this server implements. The library also knows "unix"; nothing here does.
const (
	ModeStdio = "stdio"
	ModeHTTP  = "http"
)

// HTTPMode reports whether the server listens on HTTP rather than speaking over stdio.
func (c *Config) HTTPMode() bool { return c.Lib.Server.Mode == ModeHTTP }

// TLSEnabled reports whether the HTTP listener serves HTTPS.
func (c *Config) TLSEnabled() bool { return c.TLS != nil && c.TLS.Enabled }

// validateTransport checks the rules that tie the transport to authentication, and fills the
// actor JWT defaults.
//
// The central one: **HTTP is never served anonymously.** A stdio server is reachable only by
// the process that launched it, which is all the authentication it needs. A listening socket
// is reachable by whatever can route to it, and what answers on this one is a privileged
// shell — so http mode with neither actor_jwt nor static_tokens is a startup error rather
// than a warning, and there is no key that turns that off.
func (c *Config) validateTransport() error {
	mode := c.Lib.Server.Mode
	if mode != ModeStdio && mode != ModeHTTP {
		return fmt.Errorf("server.mode %q is not supported: this server speaks %q or %q", mode, ModeStdio, ModeHTTP)
	}
	if c.Lib.Auth != nil && c.Lib.Auth.Enabled {
		// Accepting the block and ignoring it would leave an operator believing OAuth was
		// guarding a listener that it is not.
		return fmt.Errorf("auth.enabled is set, but this server does not implement OAuth: the HTTP transport authenticates with actor_jwt and static_tokens only")
	}

	actorOn := c.ActorJWTEnabled()
	if mode == ModeStdio {
		if actorOn {
			return fmt.Errorf("actor_jwt needs server.mode: http (got %q): the token arrives as an HTTP Authorization header", mode)
		}
		if len(c.StaticTokens) > 0 {
			return fmt.Errorf("static_tokens needs server.mode: http (got %q): the token is an HTTP Authorization header", mode)
		}
		if c.TLSEnabled() {
			return fmt.Errorf("tls needs server.mode: http (got %q)", mode)
		}
		return nil
	}

	if !actorOn && len(c.StaticTokens) == 0 {
		return fmt.Errorf("server.mode: http requires actor_jwt.enabled: true or at least one static_tokens entry — this server does not serve HTTP anonymously, and has no setting that makes it")
	}
	seen := map[string]bool{}
	for i, t := range c.StaticTokens {
		switch {
		case t.Name == "":
			return fmt.Errorf("static_tokens[%d].name is required", i)
		case seen[t.Name]:
			return fmt.Errorf("static_tokens: %q is listed twice", t.Name)
		case t.TokenFile == "":
			return fmt.Errorf("static_tokens[%d] (%s).token_file is required: the token is never written in the config", i, t.Name)
		}
		seen[t.Name] = true
	}
	if c.TLSEnabled() && (c.TLS.CertFile == "" || c.TLS.KeyFile == "") {
		return fmt.Errorf("tls.cert_file and tls.key_file are both required when tls.enabled is true")
	}
	if c.SSH.IdleTimeout <= 0 {
		// On stdio the client's lifetime bounds its sessions, which is why the default is
		// "never reap". Nothing bounds them here: a caller that walks away leaves a
		// privileged login open, and because sessions belong to the caller that opened
		// them, no other caller can end it. Without a reaper, abandoned sessions fill
		// max_sessions and the server stops accepting work.
		return fmt.Errorf("ssh.idle_timeout must be set (e.g. 30m) with server.mode: http: nothing else ends a session whose caller has gone away")
	}
	if !actorOn {
		return nil
	}
	a := c.ActorJWT
	if a.Issuer == "" {
		a.Issuer = DefaultActorIssuer
	}
	if a.KagentController.Address == "" {
		a.KagentController.Address = DefaultKagentController
	}
	if a.Audience == "" {
		return fmt.Errorf("actor_jwt.audience is required: it must match an entry of the RemoteMCPServer's spec.actorJwt.audiences")
	}
	if a.CAFile == "" {
		return fmt.Errorf("actor_jwt.ca_file is required: the local Substrate servicedns CA bundle the issuer's certificate is verified against")
	}
	return nil
}
