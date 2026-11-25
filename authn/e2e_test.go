package authn

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spirilis/generic-go-mcp/mcp"
	"github.com/spirilis/generic-go-mcp/transport"

	"github.com/spirilis/mcp-linux-over-ssh/listener"
)

// TestActorTokenOverTLSReachesToolsList assembles the http-mode stack the way main does —
// authenticator, generic-go-mcp's /mcp handler, the TLS listener — and checks that an
// allowed agent's token lists tools with its principal attached, while a refused agent and
// an anonymous caller never reach the server.
func TestActorTokenOverTLSReachesToolsList(t *testing.T) {
	idp := newFakeIdP(t)
	res := &fakeResolver{id: AgentIdentity{Namespace: "kagent", Name: "k8s-helper"}}
	a := newTestAuth(t, idp, res, "kagent/k8s-helper")

	var calls atomic.Int32
	tools := mcp.NewToolRegistry()
	tools.Register(mcp.Tool{Name: "whoami", Description: "test", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(ctx context.Context, _ *mcp.ToolRequest) (mcp.Result, error) {
			calls.Add(1)
			p, _ := PrincipalFrom(ctx)
			return &mcp.ToolCallResult{Content: []mcp.Content{mcp.Text("principal=" + p.ID)}}, nil
		})
	server := mcp.NewServer(tools, mcp.NewResourceRegistry(), &mcp.ServerConfig{
		Name: "t", Version: "t", DefaultCacheScope: "private",
		PrincipalFromContext: func(ctx context.Context) string {
			p, _ := PrincipalFrom(ctx)
			return p.ID
		},
	})
	trans := transport.NewHTTPTransport(transport.HTTPTransportConfig{AuthService: a})
	mux := http.NewServeMux()
	mux.Handle("/mcp", trans.Handler(server))

	dir := t.TempDir()
	cert, der := selfSigned(t)
	certFile, keyFile := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	writeCert(t, certFile, keyFile, cert, der)
	certs, err := listener.NewCertReloader(certFile, keyFile, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := listener.New("127.0.0.1:0", mux, certs)
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // self-signed test cert
	const meta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`
	rpc := func(token, method, name, params string) (int, string) {
		body := `{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":{` + params + meta + `}}`
		req, _ := http.NewRequest(http.MethodPost, "https://"+srv.Addr().String()+"/mcp", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("MCP-Protocol-Version", "2026-07-28")
		req.Header.Set("Mcp-Method", method)
		if name != "" {
			req.Header.Set("Mcp-Name", name)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	tok := idp.token(t, tokOpts{})
	code, body := rpc(tok, "tools/list", "", "")
	if code != 200 || !strings.Contains(body, `"whoami"`) || !strings.Contains(body, `"cacheScope":"private"`) {
		t.Fatalf("allowed agent tools/list: %d %s", code, body)
	}
	call := func(token string) (int, string) {
		return rpc(token, "tools/call", "whoami", `"name":"whoami","arguments":{},`)
	}
	if code, body := call(tok); code != 200 || !strings.Contains(body, "principal=agent:kagent/k8s-helper") {
		t.Fatalf("allowed agent tools/call: %d %s", code, body)
	}
	if code, _ := call(""); code != 401 {
		t.Fatalf("anonymous: %d", code)
	}
	res.id = AgentIdentity{Namespace: "kagent", Name: "intruder"}
	if code, body := call(idp.token(t, tokOpts{name: "session-other"})); code != 403 || !strings.Contains(body, "kagent/intruder") {
		t.Fatalf("refused agent: %d %s", code, body)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("tool ran %d times; refused callers must never reach it", n)
	}
}

func writeCert(t *testing.T, certFile, keyFile string, cert tls.Certificate, der []byte) {
	t.Helper()
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		t.Fatal(err)
	}
}
