package config

import (
	"fmt"
	"os"
	"path/filepath"
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
		"static_tokens on stdio": {"server:\n  mode: stdio\n" + httpSSH + "static_tokens:\n  - name: ci\n    token_file: /x\n",
			"needs server.mode: http"},
		"a static token with no file":   {httpServer + httpSSH + "static_tokens:\n  - name: ci\n", "token_file is required"},
		"a static token written inline": {httpServer + httpSSH + "static_tokens:\n  - name: ci\n    token: abc\n", `unknown key "token"`},
		"discover_only misspelt": {httpServer + httpSSH + "static_tokens:\n  - name: ui\n    token_file: /x\n    discoverOnly: true\n",
			`unknown key "discoverOnly"`},
		"a static token that is not a map": {httpServer + httpSSH + "static_tokens:\n  - ci\n", "must be a map"},
		"a static token with no name":      {httpServer + httpSSH + "static_tokens:\n  - token_file: /x\n", "name is required"},
		"a static token listed twice": {httpServer + httpSSH + "static_tokens:\n  - name: ci\n    token_file: /x\n  - name: ci\n    token_file: /y\n",
			"listed twice"},
		"static tokens only, sessions that never expire": {httpServer + strings.Replace(httpSSH, "  idle_timeout: 30m\n", "", 1) +
			"static_tokens:\n  - name: ci\n    token_file: /x\n", "ssh.idle_timeout"},
		"tls on stdio": {"server:\n  mode: stdio\n" + httpSSH + "tls:\n  enabled: true\n  cert_file: a\n  key_file: b\n", "needs server.mode: http"},
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

// Static tokens alone are enough for http mode; actor JWT is then off and its defaults are not
// applied. Each token is read from its own file, trimmed, and a missing or empty file refuses
// to start.
func TestLoadHTTPWithStaticTokens(t *testing.T) {
	dir := t.TempDir()
	write1 := func(name, v string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ci := write1("ci", "  full-scope-0123456789abcdef0123456789abcdef\n")
	ui := write1("ui", "discover-only-0123456789abcdef0123456789\n")
	empty := write1("empty", "\n")

	cfg, err := Load(write(t, httpServer+httpSSH+fmt.Sprintf(`static_tokens:
  - name: ci
    token_file: %s
  - name: kagent-ui
    token_file: %s
    discover_only: true
`, ci, ui)))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ActorJWTEnabled() {
		t.Error("actor JWT is on with no actor_jwt block")
	}
	toks, err := cfg.ReadStaticTokens()
	if err != nil {
		t.Fatal(err)
	}
	if len(toks) != 2 || toks[0] != (StaticToken{Name: "ci", Value: "full-scope-0123456789abcdef0123456789abcdef"}) ||
		toks[1] != (StaticToken{Name: "kagent-ui", Value: "discover-only-0123456789abcdef0123456789", DiscoverOnly: true}) {
		t.Fatalf("tokens = %+v", toks)
	}

	// Beside actor JWT, both are kept.
	both, err := Load(write(t, httpServer+actorBlock+httpSSH+"static_tokens:\n  - name: ci\n    token_file: "+ci+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !both.ActorJWTEnabled() || len(both.StaticTokens) != 1 {
		t.Errorf("actor_jwt=%v static_tokens=%d", both.ActorJWTEnabled(), len(both.StaticTokens))
	}

	for name, file := range map[string]string{"missing": filepath.Join(dir, "nope"), "empty": empty} {
		cfg, err := Load(write(t, httpServer+httpSSH+"static_tokens:\n  - name: ci\n    token_file: "+file+"\n"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := cfg.ReadStaticTokens(); err == nil || !strings.Contains(err.Error(), `static token "ci"`) {
			t.Errorf("%s file: %v", name, err)
		}
	}
}
