package authn

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spirilis/generic-go-mcp/compat"
	"github.com/spirilis/generic-go-mcp/mcp"
	"github.com/spirilis/generic-go-mcp/transport"
)

const (
	staticTok   = "full-scope-0123456789abcdef0123456789abcdef"
	discoverTok = "discover-only-0123456789abcdef0123456789"
)

func newStaticAuth(t *testing.T, extra Options) *Authenticator {
	t.Helper()
	log, _ := quietLog()
	extra.Logger = log
	extra.StaticTokens = append(extra.StaticTokens,
		StaticToken{Name: "ci", Value: staticTok},
		StaticToken{Name: "kagent-ui", Value: discoverTok, DiscoverOnly: true})
	a, err := New(extra)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// Static tokens are checked first, beside actor JWTs; a token that is neither is a 401.
func TestStaticTokens(t *testing.T) {
	idp := newFakeIdP(t)
	log, _ := quietLog()
	res := &fakeResolver{id: AgentIdentity{Namespace: "kagent", Name: "helper", SessionID: "c1"}}
	a := newStaticAuth(t, Options{Verifier: newVerifier(t, idp, log), Resolver: res, AllowedAgents: []string{"kagent/helper"}})

	if code, p := call(a, staticTok); code != 200 || p != (Principal{ID: "static:ci", Kind: KindStatic, Login: "ci", Scope: ScopeFull}) {
		t.Errorf("full token: %d %+v", code, p)
	}
	if res.calls.Load() != 0 {
		t.Error("a static token was sent to the kagent controller")
	}
	if code, p := call(a, idp.token(t, tokOpts{})); code != 200 || p.ID != "agent:kagent/helper" || p.Kind != KindAgent || p.Scope != ScopeFull {
		t.Errorf("actor token beside static tokens: %d %+v", code, p)
	}
	for _, tok := range []string{staticTok[:len(staticTok)-1], staticTok + "x", strings.ToUpper(staticTok)} {
		if code, p := call(a, tok); code != 401 || p.ID != "" {
			t.Errorf("near-miss token: %d %+v", code, p)
		}
	}
	if id, login, ok := a.UserFromContext(WithPrincipal(context.Background(), Principal{ID: "static:ci", Login: "ci"})); !ok || id != "static:ci" || login != "ci" {
		t.Errorf("UserFromContext = %q %q %v", id, login, ok)
	}

	// Static tokens alone are enough authentication, and need no verifier.
	b, err := New(Options{StaticTokens: []StaticToken{{Name: "ci", Value: staticTok}}})
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := call(b, ""); code != 401 {
		t.Errorf("no token: %d", code)
	}
	for _, tok := range []string{"nope", idp.token(t, tokOpts{})} {
		if code, p := call(b, tok); code != 401 || p.ID != "" {
			t.Errorf("static-only, token %.12q…: %d %+v", tok, code, p)
		}
	}
	if code, p := call(b, staticTok); code != 200 || p.ID != "static:ci" {
		t.Errorf("static-only, full token: %d %+v", code, p)
	}
}

// A static token is one caller: every holder shares its sessions, two tokens never do, and
// it never collides with an agent.
func TestStaticOwner(t *testing.T) {
	ci := Principal{ID: "static:ci", Kind: KindStatic, Login: "ci", Scope: ScopeFull}
	ui := Principal{ID: "static:ui", Kind: KindStatic, Login: "ui", Scope: ScopeDiscover}
	agent := Principal{ID: "agent:kagent/ci", Kind: KindAgent, SessionID: "c1"}
	if ci.Owner() != "static:ci" || ui.Owner() == ci.Owner() || agent.Owner() == ci.Owner() {
		t.Errorf("owners: %q %q %q", ci.Owner(), ui.Owner(), agent.Owner())
	}
}

func TestStaticTokenRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		tokens []StaticToken
		want   string
	}{
		"short":             {[]StaticToken{{Name: "ci", Value: "short"}}, "at least 32"},
		"JWT-shaped":        {[]StaticToken{{Name: "ci", Value: "aaaaaaaaaaaaaa.bbbbbbbbbbbbbbbbbbb.cccccccccc"}}, "looks like a JWT"},
		"bad name":          {[]StaticToken{{Name: "CI Token", Value: staticTok}}, "must be lowercase"},
		"empty name":        {[]StaticToken{{Name: "", Value: staticTok}}, "must be lowercase"},
		"name twice":        {[]StaticToken{{Name: "ci", Value: staticTok}, {Name: "ci", Value: discoverTok}}, "listed twice"},
		"value twice":       {[]StaticToken{{Name: "a", Value: staticTok}, {Name: "b", Value: staticTok, DiscoverOnly: true}}, "same value"},
		"no auth at all":    {nil, "no authentication configured"},
		"agents but no JWT": {nil, "actor JWT auth is off"},
	} {
		t.Run(name, func(t *testing.T) {
			opts := Options{StaticTokens: tc.tokens}
			if name == "agents but no JWT" {
				opts = Options{StaticTokens: []StaticToken{{Name: "ci", Value: staticTok}}, AllowedAgents: []string{"kagent/x"}}
			}
			_, err := New(opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one naming %q", err, tc.want)
			}
		})
	}
}

// realStack serves the authenticator in front of the real transport, legacy overlay and server,
// with one tool whose handler counts its calls, and a skills registry.
func realStack(t *testing.T, a *Authenticator) (url string, toolCalls *atomic.Int32) {
	t.Helper()
	toolCalls = &atomic.Int32{}
	reg := mcp.NewToolRegistry()
	reg.Register(mcp.Tool{Name: "probe", Description: "counts calls", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, *mcp.ToolRequest) (mcp.Result, error) {
			toolCalls.Add(1)
			return &mcp.ToolCallResult{Content: []mcp.Content{mcp.Text("ran")}}, nil
		})
	resources := mcp.NewResourceRegistry()
	server := mcp.NewServer(reg, resources, &mcp.ServerConfig{Name: "t", Version: "0",
		Skills:             mcp.NewSkillRegistry(resources),
		AdvertisedVersions: append([]string{mcp.ProtocolVersion}, compat.LegacyVersions...)})
	overlay := compat.New(server, compat.Config{ServerInfo: mcp.Implementation{Name: "t", Version: "0"}})
	trans := transport.NewHTTPTransport(transport.HTTPTransportConfig{LegacySessions: overlay.LegacySessions(), AuthService: a})
	srv := httptest.NewServer(trans.Handler(overlay))
	t.Cleanup(srv.Close)
	return srv.URL, toolCalls
}

type rpcReply struct {
	status  int
	session string
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (r rpcReply) refused() bool {
	return r.status == http.StatusForbidden && r.Error != nil && r.Error.Code == discoverOnlyCode
}

func post(t *testing.T, url, token, session string, modern bool, method, name, params string) rpcReply {
	t.Helper()
	if params == "" {
		params = "{}"
	}
	if modern {
		meta := `"_meta":{"io.modelcontextprotocol/protocolVersion":"` + mcp.ProtocolVersion + `","io.modelcontextprotocol/clientCapabilities":{}}`
		if params == "{}" {
			params = "{" + meta + "}"
		} else {
			params = params[:len(params)-1] + "," + meta + "}"
		}
	}
	body := `{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":` + params + `}`
	return postRaw(t, url, token, session, modern, method, name, body)
}

func postRaw(t *testing.T, url, token, session string, modern bool, method, name, body string) rpcReply {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	if modern {
		req.Header.Set("MCP-Protocol-Version", mcp.ProtocolVersion)
		req.Header.Set("Mcp-Method", method)
		if name != "" {
			req.Header.Set("Mcp-Name", name)
		}
	}
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	// A timeout, so a request the gate wrongly lets through to a long-lived stream fails the test.
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		t.Errorf("%s: %v", method, err)
		return rpcReply{}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	out := rpcReply{status: resp.StatusCode, session: resp.Header.Get("Mcp-Session-Id")}
	if i := bytes.Index(b, []byte("data: ")); i >= 0 { // an SSE-framed reply
		b = b[i+6:]
	}
	_ = json.Unmarshal(b, &out)
	return out
}

func TestDiscoverOnlyModern(t *testing.T) {
	url, calls := realStack(t, newStaticAuth(t, Options{}))
	callParams := `{"name":"probe","arguments":{}}`

	for _, m := range []string{"server/discover", "tools/list", "resources/list", "resources/templates/list", "skills/list"} {
		if r := post(t, url, discoverTok, "", true, m, "", ""); r.status != http.StatusOK || r.Error != nil {
			t.Errorf("%s: status=%d error=%+v", m, r.status, r.Error)
		}
	}
	if r := post(t, url, discoverTok, "", true, "tools/list", "", ""); !strings.Contains(string(r.Result), `"probe"`) {
		t.Fatalf("tools/list did not list the tool: %s", r.Result)
	}
	for m, name := range map[string]string{"tools/call": "probe", "resources/read": "x://y", "prompts/get": "p",
		"skills/get": "", "subscriptions/listen": "", "completion/complete": "", "no/such-method": ""} {
		if r := post(t, url, discoverTok, "", true, m, name, callParams); !r.refused() {
			t.Errorf("%s: status=%d error=%+v, want 403 with %d", m, r.status, r.Error, discoverOnlyCode)
		}
	}
	// The header is the client's claim; the body decides.
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":` + callParams + `}`
	if r := postRaw(t, url, discoverTok, "", true, "tools/list", "", body); !r.refused() {
		t.Errorf("header says tools/list, body says tools/call: status=%d error=%+v", r.status, r.Error)
	}
	// Whichever "method" key the decoder keeps, the gate and the router keep the same one.
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/list","Method":"tools/call","params":` + callParams + `}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","method":"tools/list","params":` + callParams + `}`,
		`[{"jsonrpc":"2.0","id":1,"method":"tools/call","params":` + callParams + `}]`,
		`not json`,
	} {
		postRaw(t, url, discoverTok, "", false, "", "", body)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("tool handler ran %d times for a discover-only credential", n)
	}

	// The full-scope token still works.
	if r := post(t, url, staticTok, "", true, "tools/call", "probe", callParams); r.status != http.StatusOK || r.Error != nil {
		t.Fatalf("full token: status=%d error=%+v", r.status, r.Error)
	}
	if calls.Load() != 1 {
		t.Fatalf("full token: tool ran %d times, want 1", calls.Load())
	}
}

func TestDiscoverOnlyLegacy(t *testing.T) {
	url, calls := realStack(t, newStaticAuth(t, Options{}))
	init := `{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}`

	r := post(t, url, discoverTok, "", false, "initialize", "", init)
	if r.status != http.StatusOK || r.session == "" {
		t.Fatalf("initialize: status=%d session=%q error=%+v", r.status, r.session, r.Error)
	}
	sid := r.session
	note := `{"jsonrpc":"2.0","method":"notifications/initialized"}`
	if r := postRaw(t, url, discoverTok, sid, false, "", "", note); r.status >= 300 {
		t.Fatalf("notifications/initialized: status=%d", r.status)
	}
	if r := post(t, url, discoverTok, sid, false, "ping", "", ""); r.status != http.StatusOK {
		t.Fatalf("ping: status=%d", r.status)
	}
	if r := post(t, url, discoverTok, sid, false, "tools/list", "", ""); r.status != http.StatusOK || !strings.Contains(string(r.Result), `"probe"`) {
		t.Fatalf("legacy tools/list: status=%d result=%s", r.status, r.Result)
	}
	if r := post(t, url, discoverTok, sid, false, "tools/call", "", `{"name":"probe","arguments":{}}`); !r.refused() {
		t.Fatalf("legacy tools/call: status=%d error=%+v", r.status, r.Error)
	}
	if r := post(t, url, discoverTok, sid, false, "resources/read", "", `{"uri":"x://y"}`); !r.refused() {
		t.Fatalf("legacy resources/read: status=%d error=%+v", r.status, r.Error)
	}
	if calls.Load() != 0 {
		t.Fatalf("tool handler ran %d times", calls.Load())
	}
	// A modern request may not borrow the legacy handshake exemption.
	if r := post(t, url, discoverTok, "", true, "ping", "", ""); !r.refused() {
		t.Fatalf("modern ping: status=%d error=%+v", r.status, r.Error)
	}
	// Tearing the session down is allowed.
	req, _ := http.NewRequest(http.MethodDelete, url, nil)
	req.Header.Set("Authorization", "Bearer "+discoverTok)
	req.Header.Set("Mcp-Session-Id", sid)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE: status=%d", resp.StatusCode)
	}
}

// A principal built without a scope is refused everywhere.
func TestZeroScopeRefused(t *testing.T) {
	a := newStaticAuth(t, Options{})
	ran := false
	h := a.gate(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { ran = true }))
	for _, m := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
		req := httptest.NewRequest(m, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req.WithContext(WithPrincipal(req.Context(), Principal{ID: "static:x", Kind: KindStatic})))
		if rec.Code != http.StatusForbidden || ran {
			t.Fatalf("%s: code=%d ran=%v", m, rec.Code, ran)
		}
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req) // no principal at all
		if rec.Code != http.StatusForbidden || ran {
			t.Fatalf("%s without principal: code=%d ran=%v", m, rec.Code, ran)
		}
	}
}
