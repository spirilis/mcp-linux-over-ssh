package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/lestrrat-go/jwx/jwk"
	"github.com/spirilis/generic-go-mcp/mcp"

	"github.com/spirilis/mcp-linux-over-ssh/authn"
	"github.com/spirilis/mcp-linux-over-ssh/config"
	"github.com/spirilis/mcp-linux-over-ssh/session"
)

// These tests stand up what `server.mode: http` actually serves — assemble's stack behind
// newHTTPHandler, real HTTP, real bash sessions — with only the two things outside this
// process faked: the Substrate issuer that signs actor tokens, and the kagent controller that
// says which Agent a token belongs to.

const testAudience = "http://mcp-linux-over-ssh.kagent.svc.cluster.local"

// issuer is a one-key HTTPS OIDC issuer. authn's own tests have a fuller one; it lives in a
// _test file of another package and cannot be shared, and an exported test-support package
// would have to avoid importing authn to stay usable from authn's in-package tests.
type issuer struct {
	srv    *httptest.Server
	key    *ecdsa.PrivateKey
	caFile string
}

func newIssuer(t *testing.T) *issuer {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "idp"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}

	i := &issuer{key: key}
	mux := http.NewServeMux()
	i.srv = httptest.NewUnstartedServer(mux)
	i.srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	i.srv.StartTLS()
	t.Cleanup(i.srv.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": i.srv.URL, "jwks_uri": i.srv.URL + "/jwks"})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		k, _ := jwk.New(&key.PublicKey)
		_ = k.Set(jwk.KeyIDKey, "k1")
		_ = k.Set(jwk.AlgorithmKey, "ES256")
		set := jwk.NewSet()
		set.Add(k)
		_ = json.NewEncoder(w).Encode(set)
	})
	i.caFile = filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(i.caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return i
}

// token mints an actor JWT for one sandbox. The sandbox name is what the fake controller
// keys its answer on, as the real one keys on the token.
func (i *issuer) token(t *testing.T, actor string) string {
	t.Helper()
	now := time.Now()
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{
		"iss": i.srv.URL, "sub": "actor/kagent/" + actor, "aud": []string{testAudience, "kagent-agent-info"},
		"iat": now.Unix(), "nbf": now.Add(-time.Minute).Unix(), "exp": now.Add(10 * time.Minute).Unix(),
		"actor.ate.dev": map[string]string{"atespace": "kagent", "actorName": actor, "actorUID": "uid-" + actor},
	})
	tok.Header["kid"] = "k1"
	s, err := tok.SignedString(i.key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// controller stands in for kagent: it knows which Agent and conversation each token is.
type controller struct {
	mu  sync.Mutex
	ids map[string]authn.AgentIdentity
}

func (c *controller) Resolve(_ context.Context, raw string) (authn.AgentIdentity, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id, ok := c.ids[raw]
	if !ok {
		return authn.AgentIdentity{}, errors.New("no agent for this token")
	}
	return id, nil
}

type httpServer struct {
	t     *testing.T
	url   string
	idp   *issuer
	agent *controller
}

// newHTTPServer assembles the http-mode stack and serves it. allowed is the agent allowlist.
func newHTTPServer(t *testing.T, allowed ...string) *httpServer {
	t.Helper()
	idp := newIssuer(t)

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	yaml := `server:
  mode: http
  http: {host: 127.0.0.1, port: 0}
actor_jwt:
  enabled: true
  issuer: ` + idp.srv.URL + `
  audience: ` + testAudience + `
  ca_file: ` + idp.caFile + `
ssh:
  command: "bash --norc --noprofile -i"
  prompt_regex: '\$ $'
  idle_timeout: 10m
  require_confirmation: false
`
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	verifier, err := authn.NewActorVerifier(t.Context(), authn.ActorJWTConfig{
		Issuer: cfg.ActorJWT.Issuer, Audience: cfg.ActorJWT.Audience, CAFile: cfg.ActorJWT.CAFile}, log)
	if err != nil {
		t.Fatal(err)
	}
	ctl := &controller{ids: map[string]authn.AgentIdentity{}}
	auth, err := authn.New(authn.Options{Verifier: verifier, Resolver: ctl, AllowedAgents: allowed, Logger: log})
	if err != nil {
		t.Fatal(err)
	}

	mgr := session.NewManager(cfg.SSH.MaxSessions, 0)
	t.Cleanup(mgr.CloseAll)
	handler, legacy, err := assemble(cfg, mgr, authenticatedOwner, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	root, trans, err := newHTTPHandler(auth, handler, legacy, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(root)
	t.Cleanup(func() { _ = trans.Stop(); srv.Close() })
	return &httpServer{t: t, url: srv.URL, idp: idp, agent: ctl}
}

// caller is one conversation: an Agent, a kagent session, and the token its sandbox carries.
type caller struct {
	s     *httpServer
	token string
	// legacy is the Mcp-Session-Id of a pre-2026-07-28 handshake, when this caller is one.
	legacy string
}

func (s *httpServer) caller(agent, conversation string) *caller {
	s.t.Helper()
	tok := s.idp.token(s.t, "sandbox-"+conversation)
	ns, name, _ := strings.Cut(agent, "/")
	s.agent.mu.Lock()
	s.agent.ids[tok] = authn.AgentIdentity{Namespace: ns, Name: name, SessionID: conversation}
	s.agent.mu.Unlock()
	return &caller{s: s, token: tok}
}

type rpcReply struct {
	status int
	header http.Header
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *caller) post(method string, params map[string]any) rpcReply {
	c.s.t.Helper()
	if params == nil {
		params = map[string]any{}
	}
	req, _ := http.NewRequest(http.MethodPost, c.s.url+"/mcp", nil)
	if c.legacy == "" && method != "initialize" && method != "notifications/initialized" {
		params["_meta"] = map[string]any{
			"io.modelcontextprotocol/protocolVersion":    mcp.ProtocolVersion,
			"io.modelcontextprotocol/clientCapabilities": map[string]any{},
		}
		req.Header.Set("MCP-Protocol-Version", mcp.ProtocolVersion)
		req.Header.Set("Mcp-Method", method)
		if n, _ := params["name"].(string); n != "" {
			req.Header.Set("Mcp-Name", n)
		} else if u, _ := params["uri"].(string); u != "" {
			req.Header.Set("Mcp-Name", u)
		}
	}
	msg := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
	if !strings.HasPrefix(method, "notifications/") {
		msg["id"] = 1
	}
	body, _ := json.Marshal(msg)
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.legacy != "" {
		req.Header.Set("Mcp-Session-Id", c.legacy)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.s.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	// A streamed reply is SSE; the response is its last data line.
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		for _, line := range strings.Split(string(raw), "\n") {
			if d, ok := strings.CutPrefix(line, "data:"); ok {
				raw = []byte(strings.TrimSpace(d))
			}
		}
	}
	out := rpcReply{status: resp.StatusCode, header: resp.Header}
	_ = json.Unmarshal(raw, &out)
	return out
}

// handshake turns this caller into a pre-2026-07-28 client: initialize, keep the session id,
// send no _meta afterwards.
func (c *caller) handshake() {
	c.s.t.Helper()
	r := c.post("initialize", map[string]any{
		"protocolVersion": "2025-11-25", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "old", "version": "0"},
	})
	if r.status != http.StatusOK || r.header.Get("Mcp-Session-Id") == "" {
		c.s.t.Fatalf("legacy initialize: status %d, no session id (%s)", r.status, r.Result)
	}
	c.legacy = r.header.Get("Mcp-Session-Id")
	c.post("notifications/initialized", nil)
}

type toolReply struct {
	IsError bool `json:"isError"`
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
	Structured map[string]any `json:"structuredContent"`
}

func (c *caller) tool(name string, args map[string]any) toolReply {
	c.s.t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	r := c.post("tools/call", map[string]any{"name": name, "arguments": args})
	if r.status != http.StatusOK || r.Error != nil {
		c.s.t.Fatalf("tools/call %s: status %d, error %+v", name, r.status, r.Error)
	}
	var out toolReply
	if err := json.Unmarshal(r.Result, &out); err != nil {
		c.s.t.Fatalf("tools/call %s: %v in %s", name, err, r.Result)
	}
	return out
}

func (r toolReply) text() string {
	var b strings.Builder
	for _, c := range r.Content {
		b.WriteString(c.Text)
	}
	return b.String()
}

// "Anonymous HTTP is hard-denied" rests on this. The library serves /mcp to anyone when no
// auth provider is configured, so the property is one of this program's wiring and has to be
// observed on the handler it serves, not inferred from the middleware's own tests.
func TestHTTPRefusesAnonymousCallers(t *testing.T) {
	s := newHTTPServer(t, "kagent/allowed")

	if _, _, err := newHTTPHandler(nil, nil, nil, nil); err == nil {
		t.Fatal("an HTTP handler was built without an authenticator")
	}

	for name, c := range map[string]*caller{
		"no credential":   {s: s},
		"garbage bearer":  {s: s, token: "not-a-token"},
		"forged jwt":      {s: s, token: "eyJhbGciOiJub25lIn0.eyJzdWIiOiJhY3Rvci9rL3gifQ."},
		"unknown sandbox": {s: s, token: s.idp.token(t, "nobody-vouches-for-this")},
	} {
		for _, method := range []string{"server/discover", "tools/list", "tools/call", "initialize"} {
			r := c.post(method, map[string]any{"name": "list_sessions"})
			if r.status != http.StatusUnauthorized {
				t.Errorf("%s, %s: HTTP %d, want 401", name, method, r.status)
			}
			if r.Result != nil {
				t.Errorf("%s, %s: got a result: %s", name, method, r.Result)
			}
		}
	}

	// A real agent that is not on the allowlist is a different answer, and names the agent
	// so whoever reads the failure knows which entry is missing.
	if r := s.caller("kagent/stranger", "c1").post("tools/list", nil); r.status != http.StatusForbidden {
		t.Errorf("an agent off the allowlist: HTTP %d, want 403", r.status)
	}
	if r := s.caller("kagent/allowed", "c2").post("tools/list", nil); r.status != http.StatusOK || r.Result == nil {
		t.Errorf("an allowed agent: HTTP %d, error %+v", r.status, r.Error)
	}

	// Probes carry no token, and get no further than the probe.
	for _, p := range []string{"/healthz", "/readyz"} {
		resp, err := http.Get(s.url + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s without a token: HTTP %d", p, resp.StatusCode)
		}
	}
	// And the legacy GET stream is behind the same door.
	resp, err := http.Get(s.url + "/mcp")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /mcp without a token: HTTP %d, want 401", resp.StatusCode)
	}
}

// One process, several conversations. A session is a privileged shell and its transcript is
// whatever was printed in it, so a caller must not be able to list, use, read or end a
// session it did not open — not another Agent's, and not another conversation's with the
// same Agent. Checked through both protocol eras, because the legacy overlay rejoins the same
// tools and a gate that only held on the modern path would be no gate.
func TestHTTPSessionsBelongToTheirCaller(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a shell")
	}
	s := newHTTPServer(t, "kagent/*")
	owner := s.caller("kagent/helper", "conversation-1")

	started := owner.tool("start_session", nil)
	if started.IsError {
		t.Fatalf("start_session: %s", started.text())
	}
	id, _ := started.Structured["session_id"].(string)
	if id == "" {
		t.Fatalf("start_session returned no id: %+v", started.Structured)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		l := owner.tool("list_sessions", nil)
		if rows, _ := l.Structured["sessions"].([]any); len(rows) == 1 && rows[0].(map[string]any)["state"] == "ready" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("session never became ready: %s", l.text())
		}
		time.Sleep(50 * time.Millisecond)
	}
	const secret = "s3cret-only-the-owner-should-see"
	if r := owner.tool("run_bash", map[string]any{"session_id": id, "cmd": "echo " + secret}); r.IsError || !strings.Contains(r.text(), secret) {
		t.Fatalf("the owner cannot use its own session: %s", r.text())
	}

	sameAgentLegacy := s.caller("kagent/helper", "conversation-3")
	sameAgentLegacy.handshake()
	others := map[string]*caller{
		"another agent":                           s.caller("kagent/other", "conversation-9"),
		"the same agent, another conversation":    s.caller("kagent/helper", "conversation-2"),
		"the same agent, another one, legacy era": sameAgentLegacy,
	}
	for who, c := range others {
		if l := c.tool("list_sessions", nil); l.Structured["count"] != float64(0) || strings.Contains(l.text(), id) {
			t.Errorf("%s: list_sessions shows a session it did not open: %s", who, l.text())
		}
		for _, call := range []struct {
			tool string
			args map[string]any
		}{
			{"run_bash", map[string]any{"session_id": id, "cmd": "id"}},
			{"tune_prompt_regex", map[string]any{"session_id": id, "regex": `\$ $`, "apply": true}},
			{"end_session", map[string]any{"session_id": id}},
		} {
			r := c.tool(call.tool, call.args)
			if !r.IsError || !strings.Contains(r.text(), "not an open session") {
				t.Errorf("%s: %s on someone else's session: isError=%v %q", who, call.tool, r.IsError, r.text())
			}
		}
		for _, uri := range []string{"mcp+stdio:///session/" + id + "/transcript", "mcp+stdio:///session/" + id + "/status"} {
			r := c.post("resources/read", map[string]any{"uri": uri})
			if r.Error == nil || strings.Contains(string(r.Result), secret) {
				t.Errorf("%s: read %s: error %+v, result %s", who, uri, r.Error, r.Result)
			}
			// The same answer an id that never existed gets, so a leaked id confirms nothing.
			never := c.post("resources/read", map[string]any{"uri": strings.Replace(uri, id, "s-00000000", 1)})
			if r.Error != nil && never.Error != nil && r.Error.Code != never.Error.Code {
				t.Errorf("%s: someone else's session answers %d, a nonexistent one %d", who, r.Error.Code, never.Error.Code)
			}
		}
		if r := c.post("resources/read", map[string]any{"uri": "mcp+stdio:///sessions"}); strings.Contains(string(r.Result), id) {
			t.Errorf("%s: the session index lists someone else's session: %s", who, r.Result)
		}
	}

	// None of that touched it.
	if r := owner.post("resources/read", map[string]any{"uri": "mcp+stdio:///session/" + id + "/transcript"}); !strings.Contains(string(r.Result), secret) {
		t.Errorf("the owner cannot read its own transcript: %+v %s", r.Error, r.Result)
	}
	if r := owner.tool("run_bash", map[string]any{"session_id": id, "cmd": "echo still-here"}); r.IsError {
		t.Errorf("the owner's session did not survive the others: %s", r.text())
	}
	if r := owner.tool("end_session", map[string]any{"session_id": id}); r.IsError {
		t.Errorf("the owner cannot end its own session: %s", r.text())
	}
}

// The second lock on the door: should a request ever reach a tool over HTTP with nothing
// attested, it must be refused, not treated as stdio's single implicit caller — which owns
// the empty string and would be handed nothing here, but must not be handed even that.
func TestAuthenticatedOwnerNeedsAPrincipal(t *testing.T) {
	if who, err := authenticatedOwner(context.Background()); err == nil {
		t.Errorf("a context with no principal resolved to owner %q", who)
	}
	if who, err := authenticatedOwner(authn.WithPrincipal(context.Background(), authn.Principal{})); err == nil {
		t.Errorf("an empty principal resolved to owner %q", who)
	}

	a := authn.Principal{ID: "agent:kagent/helper", SessionID: "c1"}
	b := authn.Principal{ID: "agent:kagent/helper", SessionID: "c2"}
	wa, err := authenticatedOwner(authn.WithPrincipal(context.Background(), a))
	if err != nil {
		t.Fatal(err)
	}
	wb, _ := authenticatedOwner(authn.WithPrincipal(context.Background(), b))
	if wa == wb {
		t.Errorf("two conversations with one agent share the owner %q", wa)
	}

	// A controller that reports no session id must not widen ownership to the whole Agent:
	// the sandbox the token names still tells two conversations apart.
	c := authn.Principal{ID: "agent:kagent/helper", Actor: "kagent/sandbox-1"}
	d := authn.Principal{ID: "agent:kagent/helper", Actor: "kagent/sandbox-2"}
	wc, _ := authenticatedOwner(authn.WithPrincipal(context.Background(), c))
	wd, _ := authenticatedOwner(authn.WithPrincipal(context.Background(), d))
	if wc == wd {
		t.Errorf("with no session id, two sandboxes of one agent share the owner %q", wc)
	}
}

// stream is one open notification stream: a modern subscriptions/listen, or a legacy
// client's GET.
type stream struct {
	mu   sync.Mutex
	got  []map[string]any
	stop context.CancelFunc
}

// listen opens a notification stream for c and returns once the server has accepted it.
// uris are the resourceSubscriptions a modern caller asks for; a legacy caller cannot ask.
func (c *caller) listen(uris ...string) *stream {
	c.s.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	st := &stream{stop: cancel}
	c.s.t.Cleanup(cancel)

	var req *http.Request
	if c.legacy != "" {
		req, _ = http.NewRequestWithContext(ctx, http.MethodGet, c.s.url+"/mcp", nil)
		req.Header.Set("Mcp-Session-Id", c.legacy)
	} else {
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 77, "method": "subscriptions/listen", "params": map[string]any{
			"notifications": map[string]any{"resourcesListChanged": true, "toolsListChanged": true, "resourceSubscriptions": uris},
			"_meta": map[string]any{
				"io.modelcontextprotocol/protocolVersion":    mcp.ProtocolVersion,
				"io.modelcontextprotocol/clientCapabilities": map[string]any{},
			},
		}})
		req, _ = http.NewRequestWithContext(ctx, http.MethodPost, c.s.url+"/mcp", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("MCP-Protocol-Version", mcp.ProtocolVersion)
		req.Header.Set("Mcp-Method", "subscriptions/listen")
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.s.t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		c.s.t.Fatalf("opening a notification stream: HTTP %d", resp.StatusCode)
	}
	go func() {
		defer resp.Body.Close()
		buf := make([]byte, 0, 4096)
		chunk := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(chunk)
			buf = append(buf, chunk[:n]...)
			for {
				i := bytes.IndexByte(buf, '\n')
				if i < 0 {
					break
				}
				line := string(buf[:i])
				buf = buf[i+1:]
				if d, ok := strings.CutPrefix(line, "data:"); ok {
					var m map[string]any
					if json.Unmarshal([]byte(strings.TrimSpace(d)), &m) == nil {
						st.mu.Lock()
						st.got = append(st.got, m)
						st.mu.Unlock()
					}
				}
			}
			if err != nil {
				return
			}
		}
	}()
	return st
}

// heard lists what arrived, as "method" or "method uri".
func (st *stream) heard() []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	var out []string
	for _, m := range st.got {
		method, _ := m["method"].(string)
		if p, _ := m["params"].(map[string]any); p != nil {
			if uri, _ := p["uri"].(string); uri != "" {
				method += " " + uri
			}
		}
		out = append(out, method)
	}
	return out
}

func (st *stream) ack() map[string]any {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, m := range st.got {
		if m["method"] == "notifications/subscriptions/acknowledged" {
			p, _ := m["params"].(map[string]any)
			n, _ := p["notifications"].(map[string]any)
			return n
		}
	}
	return nil
}

func (st *stream) clear() {
	st.mu.Lock()
	st.got = nil
	st.mu.Unlock()
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (c *caller) startReadySession() string {
	c.s.t.Helper()
	started := c.tool("start_session", nil)
	id, _ := started.Structured["session_id"].(string)
	if started.IsError || id == "" {
		c.s.t.Fatalf("start_session: %s", started.text())
	}
	eventually(c.s.t, "the session to become ready", func() bool {
		rows, _ := c.tool("list_sessions", nil).Structured["sessions"].([]any)
		return len(rows) > 0 && rows[len(rows)-1].(map[string]any)["state"] == "ready"
	})
	return id
}

// Refusing a read is not enough if the signal still gets out. Every command makes a session
// go busy and ready, and every start and end changes the catalog — so a caller who can see
// nothing of another's session could still watch it being used, command by command, unless
// the notifications themselves stay with their owner.
//
// The broker fans out to everyone, so this is a property of callerScope sitting in front of
// it, and it is checked the only way that means anything: by holding real streams open as
// other callers while the owner works, and requiring silence.
func TestHTTPNotificationsStayWithTheirOwner(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a shell")
	}
	s := newHTTPServer(t, "kagent/*")
	owner := s.caller("kagent/helper", "conversation-1")
	id := owner.startReadySession()
	status := "mcp+stdio:///session/" + id + "/status"
	transcript := "mcp+stdio:///session/" + id + "/transcript"
	const index, never = "mcp+stdio:///sessions", "mcp+stdio:///session/s-00000000/status"

	mine := owner.listen(status, index)
	otherAgent := s.caller("kagent/other", "conversation-9")
	sameAgent := s.caller("kagent/helper", "conversation-2")
	legacy := s.caller("kagent/helper", "conversation-3")
	legacy.handshake()
	watchers := map[string]*stream{
		"another agent":                        otherAgent.listen(status, transcript, never, index),
		"the same agent, another conversation": sameAgent.listen(status, transcript, never, index),
		"a legacy client's stream":             legacy.listen(),
	}

	// What each was granted. Someone else's session and a session that never existed must
	// come back identically — both gone — or the acknowledgement is an oracle for ids.
	eventually(t, "the owner's subscription to be acknowledged", func() bool { return mine.ack() != nil })
	if got, _ := mine.ack()["resourceSubscriptions"].([]any); len(got) != 2 {
		t.Errorf("the owner was granted %v, want its own status URI and the index", got)
	}
	for who, st := range watchers {
		if who == "a legacy client's stream" {
			continue // the overlay swallows the acknowledgement; a legacy client never asked
		}
		eventually(t, who+"'s subscription to be acknowledged", func() bool { return st.ack() != nil })
		got, _ := st.ack()["resourceSubscriptions"].([]any)
		if len(got) != 1 || got[0] != index {
			t.Errorf("%s was granted %v, want only the index", who, got)
		}
	}
	// Give the legacy stream, which announces nothing, time to be attached.
	time.Sleep(300 * time.Millisecond)
	mine.clear()
	for _, st := range watchers {
		st.clear()
	}

	// The owner works: two commands, then ends the session.
	for _, cmd := range []string{"true", "echo hello"} {
		if r := owner.tool("run_bash", map[string]any{"session_id": id, "cmd": cmd}); r.IsError {
			t.Fatalf("run_bash: %s", r.text())
		}
	}
	if r := owner.tool("end_session", map[string]any{"session_id": id}); r.IsError {
		t.Fatalf("end_session: %s", r.text())
	}

	has := func(st *stream, want string) bool {
		for _, h := range st.heard() {
			if h == want {
				return true
			}
		}
		return false
	}
	// The owner hears all of it. This is also what makes the silence below meaningful: by
	// the time these have arrived, anything addressed to the others has had its chance.
	for _, want := range []string{
		"notifications/resources/updated " + status,
		"notifications/resources/updated " + index,
		"notifications/resources/list_changed",
	} {
		eventually(t, "the owner to hear "+want, func() bool { return has(mine, want) })
	}
	time.Sleep(300 * time.Millisecond)
	for who, st := range watchers {
		if h := st.heard(); len(h) != 0 {
			t.Errorf("%s heard the owner's session being used: %v", who, h)
		}
	}

	// And it is a scope, not a mute: each still hears about its own.
	mine.clear()
	theirs := otherAgent.startReadySession()
	st := watchers["another agent"]
	eventually(t, "another agent to hear its own session join", func() bool {
		return has(st, "notifications/resources/list_changed") && has(st, "notifications/resources/updated "+index)
	})
	if r := otherAgent.tool("end_session", map[string]any{"session_id": theirs}); r.IsError {
		t.Fatalf("end_session: %s", r.text())
	}
	time.Sleep(300 * time.Millisecond)
	if h := mine.heard(); len(h) != 0 {
		t.Errorf("the first owner heard another agent's session: %v", h)
	}
	if h := watchers["the same agent, another conversation"].heard(); len(h) != 0 {
		t.Errorf("a bystander heard another agent's session: %v", h)
	}
}

// The legacy stream must also carry a legacy caller's own changes, or scoping it would have
// been indistinguishable from breaking it.
func TestHTTPLegacyStreamStillHearsItsOwnSessions(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a shell")
	}
	s := newHTTPServer(t, "kagent/*")
	c := s.caller("kagent/helper", "conversation-1")
	c.handshake()
	st := c.listen()
	time.Sleep(300 * time.Millisecond)

	id := c.startReadySession()
	eventually(t, "a legacy caller to hear its own session join", func() bool {
		for _, h := range st.heard() {
			if h == "notifications/resources/list_changed" {
				return true
			}
		}
		return false
	})
	c.tool("end_session", map[string]any{"session_id": id})
}
