package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A static token is one caller. Everyone holding it shares its sessions, through either
// protocol era; another token, or an agent whose name happens to match, sees none of them.
func TestHTTPStaticTokenSessions(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a shell")
	}
	s := newHTTPServer(t, "kagent/*")
	ci := s.static("ci")
	id := ci.startReadySession()
	const secret = "s3cret-only-the-token-should-see"
	if r := ci.tool("run_bash", map[string]any{"session_id": id, "cmd": "echo " + secret}); r.IsError || !strings.Contains(r.text(), secret) {
		t.Fatalf("the token cannot use its own session: %s", r.text())
	}

	// Another holder of the same token, legacy era: the same caller.
	again := s.static("ci")
	again.handshake()
	if l := again.tool("list_sessions", nil); !strings.Contains(l.text(), id) {
		t.Errorf("another holder of the token does not see its session: %s", l.text())
	}
	if r := again.post("resources/read", map[string]any{"uri": "mcp+stdio:///session/" + id + "/transcript"}); !strings.Contains(string(r.Result), secret) {
		t.Errorf("another holder of the token cannot read its transcript: %+v", r.Error)
	}

	for who, c := range map[string]*caller{
		"another token":                 s.static("ci-2"),
		"an agent named like the token": s.caller("kagent/ci", "conversation-1"),
	} {
		if l := c.tool("list_sessions", nil); l.Structured["count"] != float64(0) {
			t.Errorf("%s: list_sessions shows the token's session: %s", who, l.text())
		}
		if r := c.tool("run_bash", map[string]any{"session_id": id, "cmd": "id"}); !r.IsError || !strings.Contains(r.text(), "not an open session") {
			t.Errorf("%s: run_bash on the token's session: %q", who, r.text())
		}
		if r := c.post("resources/read", map[string]any{"uri": "mcp+stdio:///session/" + id + "/transcript"}); r.Error == nil || strings.Contains(string(r.Result), secret) {
			t.Errorf("%s: read the token's transcript: %s", who, r.Result)
		}
	}
	if r := ci.tool("end_session", map[string]any{"session_id": id}); r.IsError {
		t.Errorf("end_session: %s", r.text())
	}
}

// A discover-only token lists the catalog and nothing else, on the assembled stack, through
// both eras: it cannot open a session, run or read anything, or subscribe. (A legacy GET
// stream is let through, and hears nothing: TestHTTPNotificationsStayWithTheirOwner.)
func TestHTTPDiscoverOnlyToken(t *testing.T) {
	s := newHTTPServer(t, "kagent/*")
	cat := s.static("catalog")
	refused := func(r rpcReply) bool {
		return r.status == http.StatusForbidden && r.Error != nil && r.Error.Code == -32003
	}

	for _, m := range []string{"server/discover", "tools/list", "resources/list", "resources/templates/list"} {
		if r := cat.post(m, nil); r.status != http.StatusOK || r.Error != nil {
			t.Errorf("%s: HTTP %d, error %+v", m, r.status, r.Error)
		}
	}
	if r := cat.post("tools/list", nil); !strings.Contains(string(r.Result), `"start_session"`) || !strings.Contains(string(r.Result), `"run_bash"`) {
		t.Errorf("tools/list does not show the catalog: %s", r.Result)
	}

	for _, call := range []struct {
		method string
		params map[string]any
	}{
		{"tools/call", map[string]any{"name": "start_session", "arguments": map[string]any{}}},
		{"tools/call", map[string]any{"name": "list_sessions", "arguments": map[string]any{}}},
		{"tools/call", map[string]any{"name": "run_bash", "arguments": map[string]any{"session_id": "s-00000000", "cmd": "id"}}},
		{"resources/read", map[string]any{"uri": "mcp+stdio:///sessions"}},
		{"subscriptions/listen", map[string]any{"notifications": map[string]any{"resourcesListChanged": true}}},
	} {
		if r := cat.post(call.method, call.params); !refused(r) {
			t.Errorf("%s %v: HTTP %d, error %+v, want 403 / -32003", call.method, call.params["name"], r.status, r.Error)
		}
	}

	legacy := s.static("catalog")
	legacy.handshake()
	if r := legacy.post("tools/list", nil); r.status != http.StatusOK || !strings.Contains(string(r.Result), `"start_session"`) {
		t.Errorf("legacy tools/list: HTTP %d, %s", r.status, r.Result)
	}
	if r := legacy.post("tools/call", map[string]any{"name": "start_session", "arguments": map[string]any{}}); !refused(r) {
		t.Errorf("legacy start_session: HTTP %d, error %+v", r.status, r.Error)
	}
	if r := legacy.post("resources/read", map[string]any{"uri": "mcp+stdio:///sessions"}); !refused(r) {
		t.Errorf("legacy resources/read: HTTP %d, error %+v", r.status, r.Error)
	}

	if n := s.mgr.Live(); n != 0 {
		t.Errorf("a discover-only token left %d sessions open", n)
	}
	// The full-scope token beside it still works.
	if r := s.static("ci").post("tools/call", map[string]any{"name": "list_sessions", "arguments": map[string]any{}}); r.status != http.StatusOK || r.Error != nil {
		t.Errorf("full token: HTTP %d, error %+v", r.status, r.Error)
	}
}

// Static tokens alone are enough for http mode, built the way serveHTTP builds them; with no
// actor_jwt, an actor JWT is just a wrong token.
func TestHTTPStaticTokensOnly(t *testing.T) {
	s := newHTTPServerWith(t, serverOpts{staticOnly: true})
	if r := s.static("ci").post("tools/list", nil); r.status != http.StatusOK || r.Error != nil {
		t.Errorf("full token: HTTP %d, error %+v", r.status, r.Error)
	}
	if r := s.static("catalog").post("tools/call", map[string]any{"name": "list_sessions", "arguments": map[string]any{}}); r.status != http.StatusForbidden {
		t.Errorf("discover-only token calling a tool: HTTP %d", r.status)
	}
	for name, c := range map[string]*caller{
		"no credential": {s: s},
		"a wrong token": {s: s, token: staticValue("nobody")},
		"an actor JWT":  {s: s, token: s.idp.token(t, "sandbox-1")},
	} {
		if r := c.post("tools/list", nil); r.status != http.StatusUnauthorized || r.Result != nil {
			t.Errorf("%s: HTTP %d", name, r.status)
		}
	}
}

// generic-go-mcp v0.12.0 refuses inputResponses that arrive without the requestState of the
// request that asked for them. Before, they reached the tool unverified, so a client could
// pre-confirm run_bash and skip the user's confirmation. Pinned here because the
// confirmation is this server's, and the property is what it was for.
func TestRunBashCannotBePreConfirmed(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a shell")
	}
	s := newHTTPServerWith(t, serverOpts{allowed: []string{"kagent/*"}, confirm: true})
	c := s.static("ci")
	c.caps = map[string]any{"elicitation": map[string]any{"form": map[string]any{}}}
	id := c.startReadySession()

	marker := filepath.Join(t.TempDir(), "ran")
	args := map[string]any{"session_id": id, "cmd": "touch " + marker}
	accept := map[string]any{"confirm": map[string]any{"action": "accept", "content": map[string]any{"confirm": true}}}

	r := c.post("tools/call", map[string]any{"name": "run_bash", "arguments": args, "inputResponses": accept})
	if r.Error == nil {
		t.Errorf("a pre-confirmed run_bash was accepted: %s", r.Result)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a pre-confirmed run_bash executed its command")
	}

	// The real round trip still works, so the refusal above is not the tool being broken.
	first := c.post("tools/call", map[string]any{"name": "run_bash", "arguments": args})
	var asked struct {
		InputRequests map[string]any `json:"inputRequests"`
		RequestState  string         `json:"requestState"`
	}
	if err := json.Unmarshal(first.Result, &asked); err != nil || asked.RequestState == "" || asked.InputRequests["confirm"] == nil {
		t.Fatalf("run_bash did not ask for confirmation: %+v %s", first.Error, first.Result)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("run_bash executed before it was confirmed")
	}
	retry := c.post("tools/call", map[string]any{"name": "run_bash", "arguments": args, "inputResponses": accept, "requestState": asked.RequestState})
	if retry.Error != nil {
		t.Fatalf("the confirmed retry: %+v", retry.Error)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the confirmed command did not run: %v (%s)", err, retry.Result)
	}
}
