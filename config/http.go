package config

import (
	"fmt"
)

// ActorJWTConfig configures the one credential the HTTP transport accepts: a kagent agent's
// Substrate actor JWT, verified against THIS cluster's issuer. There is deliberately no way
// to trust another cluster's issuer, and no other way to authenticate.
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
// shell — so http mode without actor_jwt is a startup error rather than a warning, and there
// is no key that turns that off.
func (c *Config) validateTransport() error {
	mode := c.Lib.Server.Mode
	if mode != ModeStdio && mode != ModeHTTP {
		return fmt.Errorf("server.mode %q is not supported: this server speaks %q or %q", mode, ModeStdio, ModeHTTP)
	}
	if c.Lib.Auth != nil && c.Lib.Auth.Enabled {
		// Accepting the block and ignoring it would leave an operator believing OAuth was
		// guarding a listener that it is not.
		return fmt.Errorf("auth.enabled is set, but this server does not implement OAuth: the HTTP transport authenticates kagent agents with actor_jwt only")
	}

	actorOn := c.ActorJWT != nil && c.ActorJWT.Enabled
	if mode == ModeStdio {
		if actorOn {
			return fmt.Errorf("actor_jwt needs server.mode: http (got %q): the token arrives as an HTTP Authorization header", mode)
		}
		if c.TLSEnabled() {
			return fmt.Errorf("tls needs server.mode: http (got %q)", mode)
		}
		return nil
	}

	if !actorOn {
		return fmt.Errorf("server.mode: http requires actor_jwt.enabled: true — this server does not serve HTTP anonymously, and has no setting that makes it")
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
	return nil
}
