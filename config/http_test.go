package config

import (
	"strings"
	"testing"
	"time"
)

const httpServer = `
server:
  mode: http
`

const actorBlock = `
actor_jwt:
  enabled: true
  audience: http://mcp.kagent.svc.cluster.local
  ca_file: /etc/issuer/ca.pem
  allowed_agents: ["kagent/helper"]
`

const httpSSH = `
ssh:
  command: "ssh -tt user@bastion"
  prompt_regex: '(?m)^[^\n]*[$#]\s*$'
  idle_timeout: 30m
`

func TestLoadHTTPMode(t *testing.T) {
	cfg, err := Load(write(t, httpServer+actorBlock+httpSSH))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.HTTPMode() || cfg.TLSEnabled() {
		t.Errorf("HTTPMode=%v TLSEnabled=%v", cfg.HTTPMode(), cfg.TLSEnabled())
	}
	if cfg.ActorJWT.Issuer != DefaultActorIssuer || cfg.ActorJWT.KagentController.Address != DefaultKagentController {
		t.Errorf("defaults not applied: %+v", cfg.ActorJWT)
	}
	if cfg.SSH.IdleTimeout != 30*time.Minute {
		t.Errorf("idle_timeout = %v", cfg.SSH.IdleTimeout)
	}
	if h := cfg.Lib.Server.HTTP; h == nil || h.Port != 8080 {
		t.Errorf("library HTTP defaults missing: %+v", h)
	}
}

// Each of these is a way to end up with a listener that is less guarded than the operator
// believes, or a deployment that quietly wedges. All are startup errors; none is a warning.
func TestLoadRefusesUnsafeTransportConfigs(t *testing.T) {
	for name, tc := range map[string]struct{ yaml, want string }{
		"http with no actor_jwt at all": {httpServer + httpSSH, "does not serve HTTP anonymously"},
		"http with actor_jwt disabled": {httpServer + strings.Replace(actorBlock, "enabled: true", "enabled: false", 1) + httpSSH,
			"does not serve HTTP anonymously"},
		"http without an audience": {httpServer + strings.Replace(actorBlock, "  audience: http://mcp.kagent.svc.cluster.local\n", "", 1) + httpSSH,
			"actor_jwt.audience"},
		"http without the issuer CA": {httpServer + strings.Replace(actorBlock, "  ca_file: /etc/issuer/ca.pem\n", "", 1) + httpSSH,
			"actor_jwt.ca_file"},
		"http with sessions that never expire": {httpServer + actorBlock + strings.Replace(httpSSH, "  idle_timeout: 30m\n", "", 1),
			"ssh.idle_timeout"},
		"http with half a certificate": {httpServer + actorBlock + httpSSH + "tls:\n  enabled: true\n  cert_file: /tls/tls.crt\n",
			"tls.cert_file and tls.key_file"},
		"oauth, which is not implemented": {httpServer + actorBlock + httpSSH + "auth:\n  enabled: true\n  issuer: https://x.example\n  github: {clientId: a, clientSecret: b}\n",
			"does not implement OAuth"},
		"actor_jwt on stdio": {"server:\n  mode: stdio\n" + actorBlock + httpSSH, "needs server.mode: http"},
		"tls on stdio":       {"server:\n  mode: stdio\n" + httpSSH + "tls:\n  enabled: true\n  cert_file: a\n  key_file: b\n", "needs server.mode: http"},
		"a mode nothing here implements": {"server:\n  mode: unix\n  unix: {socket_path: /tmp/x.sock, name: x}\n" + httpSSH,
			"not supported"},
	} {
		_, err := Load(write(t, tc.yaml))
		if err == nil {
			t.Errorf("%s: loaded", name)
		} else if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q does not mention %q", name, err, tc.want)
		}
	}
}

func TestLoadHTTPWithTLS(t *testing.T) {
	cfg, err := Load(write(t, httpServer+actorBlock+httpSSH+"tls:\n  enabled: true\n  cert_file: /tls/tls.crt\n  key_file: /tls/tls.key\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.TLSEnabled() || cfg.TLS.KeyFile != "/tls/tls.key" {
		t.Errorf("tls = %+v", cfg.TLS)
	}
}
