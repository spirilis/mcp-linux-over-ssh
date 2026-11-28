package authn

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
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/lestrrat-go/jwx/jwk"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protowire"
)

const testAud = "http://mcp-linux-over-ssh.kagent.svc.cluster.local"

// fakeIdP is an HTTPS OIDC issuer serving discovery and a one-key ES256 JWKS. Its TLS
// certificate is self-signed, so only its own CA file trusts it: that is the local
// servicedns CA bundle in miniature.
type fakeIdP struct {
	srv    *httptest.Server
	key    *ecdsa.PrivateKey
	caFile string
	kid    string
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIdP{key: key, kid: "k1"}
	mux := http.NewServeMux()
	f.srv = httptest.NewUnstartedServer(mux)
	// A certificate of its own: httptest's built-in one is shared by every test server, so
	// with it any fake's CA file would trust every other fake.
	cert, certDER := selfSigned(t)
	f.srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	f.srv.StartTLS()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": f.srv.URL, "jwks_uri": f.srv.URL + "/openid/v1/jwks"})
	})
	mux.HandleFunc("/openid/v1/jwks", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(f.jwks())
	})
	t.Cleanup(f.srv.Close)
	f.caFile = filepath.Join(t.TempDir(), "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	if err := os.WriteFile(f.caFile, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

// selfSigned makes a fresh self-signed serving certificate for 127.0.0.1.
func selfSigned(t *testing.T) (tls.Certificate, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "idp.ate-system.svc"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, der
}

func (f *fakeIdP) jwks() []byte {
	k, _ := jwk.New(&f.key.PublicKey)
	_ = k.Set(jwk.KeyIDKey, f.kid)
	_ = k.Set(jwk.AlgorithmKey, "ES256")
	_ = k.Set(jwk.KeyUsageKey, "sig")
	set := jwk.NewSet()
	set.Add(k)
	b, _ := json.Marshal(set)
	return b
}

type tokOpts struct {
	sub, atespace, name string
	aud                 []string
	exp                 time.Duration
	kid                 string
}

func (f *fakeIdP) token(t *testing.T, o tokOpts) string {
	t.Helper()
	if o.atespace == "" {
		o.atespace = "kagent"
	}
	if o.name == "" {
		o.name = "session-2b1e"
	}
	if o.sub == "" {
		o.sub = "actor/" + o.atespace + "/" + o.name
	}
	if o.aud == nil {
		o.aud = []string{testAud, agentInfoAudience}
	}
	if o.exp == 0 {
		o.exp = 15 * time.Minute
	}
	if o.kid == "" {
		o.kid = f.kid
	}
	now := time.Now()
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{
		"iss": f.srv.URL, "sub": o.sub, "aud": o.aud,
		"iat": now.Unix(), "nbf": now.Add(-5 * time.Minute).Unix(), "exp": now.Add(o.exp).Unix(),
		"actor.ate.dev": map[string]string{"atespace": o.atespace, "actorName": o.name, "actorUID": "uid-1"},
	})
	tok.Header["kid"] = o.kid
	s, err := tok.SignedString(f.key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type fakeResolver struct {
	id    AgentIdentity
	err   error
	calls atomic.Int32
}

func (r *fakeResolver) Resolve(context.Context, string) (AgentIdentity, error) {
	r.calls.Add(1)
	return r.id, r.err
}

func newVerifier(t *testing.T, idp *fakeIdP, log *slog.Logger) *ActorVerifier {
	t.Helper()
	v, err := NewActorVerifier(t.Context(), ActorJWTConfig{Issuer: idp.srv.URL, Audience: testAud, CAFile: idp.caFile}, log)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func newTestAuth(t *testing.T, idp *fakeIdP, res AgentResolver, allowed ...string) *Authenticator {
	t.Helper()
	log, _ := quietLog()
	a, err := New(Options{Verifier: newVerifier(t, idp, log), Resolver: res, AllowedAgents: allowed, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func call(a *Authenticator, token string) (int, Principal) {
	var got Principal
	h := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = PrincipalFrom(r.Context())
	}))
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, got
}

func quietLog() (*slog.Logger, *bytes.Buffer) {
	b := &bytes.Buffer{}
	return slog.New(slog.NewTextHandler(b, &slog.HandlerOptions{Level: slog.LevelDebug})), b
}

func TestActorJWT(t *testing.T) {
	idp := newFakeIdP(t)
	agent := AgentIdentity{Namespace: "kagent", Name: "k8s-helper", SessionID: "2b1e"}

	t.Run("allowed agent passes and is cached", func(t *testing.T) {
		res := &fakeResolver{id: agent}
		a := newTestAuth(t, idp, res, "kagent/k8s-helper")
		tok := idp.token(t, tokOpts{})
		code, p := call(a, tok)
		if code != http.StatusOK || p.ID != "agent:kagent/k8s-helper" || p.SessionID != "2b1e" {
			t.Fatalf("code=%d principal=%+v", code, p)
		}
		// The audit log names the Substrate actor (the session's sandbox) beside the Agent.
		if p.ActorUID != "uid-1" || !strings.Contains(p.Actor, "/") || strings.HasPrefix(p.Actor, "/") || strings.HasSuffix(p.Actor, "/") {
			t.Fatalf("actor = %q uid = %q", p.Actor, p.ActorUID)
		}
		if id, login, ok := a.UserFromContext(WithPrincipal(context.Background(), p)); !ok || id != p.ID || login != "kagent/k8s-helper" {
			t.Fatalf("UserFromContext = %q %q %v", id, login, ok)
		}
		call(a, tok)
		if n := res.calls.Load(); n != 1 {
			t.Fatalf("resolver calls = %d, want 1 (cached)", n)
		}
	})
	t.Run("globs match each half and never cross the slash", func(t *testing.T) {
		for pattern, want := range map[string]int{
			"kagent/*": http.StatusOK, "*/k8s-helper": http.StatusOK, "kag*/k8s-*": http.StatusOK,
			"kagent/k8s-helpe?": http.StatusOK, "*/*": http.StatusOK,
			"kagent/k8s": http.StatusForbidden, "k?/k8s-helper": http.StatusForbidden,
		} {
			a := newTestAuth(t, idp, &fakeResolver{id: agent}, pattern)
			if code, _ := call(a, idp.token(t, tokOpts{})); code != want {
				t.Errorf("pattern %q: code=%d, want %d", pattern, code, want)
			}
		}
	})
	t.Run("empty allowlist refuses every agent", func(t *testing.T) {
		a := newTestAuth(t, idp, &fakeResolver{id: agent})
		if code, _ := call(a, idp.token(t, tokOpts{})); code != http.StatusForbidden {
			t.Fatalf("code=%d", code)
		}
	})
	t.Run("agent not on allowlist is 403 naming it", func(t *testing.T) {
		a := newTestAuth(t, idp, &fakeResolver{id: agent}, "kagent/other")
		h := a.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		req.Header.Set("Authorization", "Bearer "+idp.token(t, tokOpts{}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "kagent/k8s-helper") {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
		}
	})
	t.Run("resolver failure fails closed", func(t *testing.T) {
		a := newTestAuth(t, idp, &fakeResolver{err: errors.New("down")}, "kagent/k8s-helper")
		if code, _ := call(a, idp.token(t, tokOpts{})); code != http.StatusUnauthorized {
			t.Fatalf("code=%d", code)
		}
	})
	bad := map[string]tokOpts{
		"wrong audience":       {aud: []string{"https://elsewhere"}},
		"runtime audience":     {aud: []string{testAud, reservedRuntimeAudience}},
		"worker-for-actor sub": {sub: "worker-for-actor/kagent/session-2b1e"},
		"sub mismatches claim": {sub: "actor/kagent/session-other"},
		"expired":              {exp: -time.Hour},
		"unknown kid":          {kid: "nope"},
		"ten-year lifetime":    {exp: 10 * 365 * 24 * time.Hour},
	}
	for name, o := range bad {
		t.Run(name, func(t *testing.T) {
			res := &fakeResolver{id: agent}
			a := newTestAuth(t, idp, res, "kagent/k8s-helper")
			if code, _ := call(a, idp.token(t, o)); code != http.StatusUnauthorized {
				t.Fatalf("code=%d", code)
			}
			if res.calls.Load() != 0 {
				t.Fatal("an unverified token reached the kagent resolver")
			}
		})
	}
	t.Run("token signed by another key is refused", func(t *testing.T) {
		other := newFakeIdP(t)
		other.srv.URL = idp.srv.URL // same iss and kid, different signing key
		a := newTestAuth(t, idp, &fakeResolver{id: agent}, "*/*")
		if code, _ := call(a, other.token(t, tokOpts{})); code != http.StatusUnauthorized {
			t.Fatalf("code=%d", code)
		}
	})
	t.Run("no token", func(t *testing.T) {
		a := newTestAuth(t, idp, &fakeResolver{id: agent})
		if code, _ := call(a, ""); code != http.StatusUnauthorized {
			t.Fatalf("code=%d", code)
		}
	})
}

// TestIssuerTLSIsPinnedToTheLocalCA: the issuer's certificate must chain to the configured CA
// bundle. A bundle that does not hold it (here another fake's) means no key is ever loaded.
func TestIssuerTLSIsPinnedToTheLocalCA(t *testing.T) {
	idp, stranger := newFakeIdP(t), newFakeIdP(t)
	log, logs := quietLog()
	v, err := NewActorVerifier(t.Context(), ActorJWTConfig{Issuer: idp.srv.URL, Audience: testAud, CAFile: stranger.caFile}, log)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := v.Verify(context.Background(), idp.token(t, tokOpts{})); err == nil {
		t.Fatal("a token was verified with keys fetched over an untrusted TLS connection")
	}
	if !strings.Contains(logs.String(), "certificate") {
		t.Errorf("discovery failure should name the TLS problem; log: %s", logs)
	}
}

func TestVerifierConfigRefusals(t *testing.T) {
	idp := newFakeIdP(t)
	empty := filepath.Join(t.TempDir(), "empty.pem")
	_ = os.WriteFile(empty, []byte("not a cert"), 0o600)
	for name, cfg := range map[string]ActorJWTConfig{
		"runtime audience":    {Issuer: idp.srv.URL, Audience: reservedRuntimeAudience, CAFile: idp.caFile},
		"agent-info audience": {Issuer: idp.srv.URL, Audience: agentInfoAudience, CAFile: idp.caFile},
		"no audience":         {Issuer: idp.srv.URL, CAFile: idp.caFile},
		"http issuer":         {Issuer: "http://idp.ate-system.svc", Audience: testAud, CAFile: idp.caFile},
		"no CA bundle":        {Issuer: idp.srv.URL, Audience: testAud},
		"CA bundle not PEM":   {Issuer: idp.srv.URL, Audience: testAud, CAFile: empty},
		"CA bundle missing":   {Issuer: idp.srv.URL, Audience: testAud, CAFile: "/nonexistent/ca.pem"},
	} {
		if _, err := NewActorVerifier(t.Context(), cfg, nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestAllowlistPatternsRefused(t *testing.T) {
	idp := newFakeIdP(t)
	for _, p := range []string{"kagent", "a/b/c", "a/[b", "/name", "ns/"} {
		_, err := New(Options{Verifier: newVerifier(t, idp, nil), Resolver: &fakeResolver{}, AllowedAgents: []string{p}})
		if err == nil {
			t.Errorf("pattern %q accepted", p)
		}
	}
}

func TestNewNeedsSomeAuthentication(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("an authenticator with nothing configured was built")
	}
	idp := newFakeIdP(t)
	if _, err := New(Options{Verifier: newVerifier(t, idp, nil)}); err == nil {
		t.Fatal("a verifier without a resolver was accepted")
	}
	if _, err := New(Options{Resolver: &fakeResolver{}, StaticTokens: []StaticToken{{Name: "ci", Value: staticTok}}}); err == nil {
		t.Fatal("a resolver without a verifier was accepted")
	}
}

func TestResolverNamesAreValidated(t *testing.T) {
	idp := newFakeIdP(t)
	for _, id := range []AgentIdentity{
		{Namespace: "a/b", Name: "c"},
		{Namespace: "a", Name: "b/c"},
		{Namespace: "a", Name: "b\nfake"},
		{Namespace: "A", Name: "b"},
		{Namespace: "a", Name: "*"},
	} {
		a := newTestAuth(t, idp, &fakeResolver{id: id}, "*/*")
		if code, p := call(a, idp.token(t, tokOpts{})); code != 401 {
			t.Errorf("ns=%q name=%q: status %d principal %q, want 401", id.Namespace, id.Name, code, p.ID)
		}
	}
	a := newTestAuth(t, idp, &fakeResolver{id: AgentIdentity{Namespace: "team-1", Name: "k8s.helper-2"}}, "*/*")
	if code, _ := call(a, idp.token(t, tokOpts{})); code != 200 {
		t.Errorf("a valid Kubernetes name got %d", code)
	}
}

func TestCacheIsCapped(t *testing.T) {
	idp := newFakeIdP(t)
	res := &fakeResolver{id: AgentIdentity{Namespace: "kagent", Name: "helper"}}
	a := newTestAuth(t, idp, res, "kagent/helper")
	tok := idp.token(t, tokOpts{exp: 59 * time.Minute})
	if code, _ := call(a, tok); code != 200 {
		t.Fatalf("a 59-minute token got %d", code)
	}
	res.err = fmt.Errorf("agent deleted")
	a.now = func() time.Time { return time.Now().Add(agentCacheTTL + time.Second) }
	if code, _ := call(a, tok); code != 401 {
		t.Fatalf("after the cache window the deleted agent still got %d", code)
	}
}

// forged is what anyone can build: a JWT-shaped bearer naming the issuer, signed by a throwaway key.
func forged(t *testing.T, iss, kid string) string {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{"iss": iss, "sub": "x", "aud": testAud, "exp": time.Now().Add(time.Hour).Unix()})
	tok.Header["kid"] = kid
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// A caller who hangs up must not cancel discovery for everyone.
func TestDiscoverySurvivesCallerHangUp(t *testing.T) {
	idp := newFakeIdP(t)
	var discHits, jwksHits atomic.Int32
	inner := idp.srv.Config.Handler
	idp.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/openid/v1/jwks" {
			jwksHits.Add(1)
		} else {
			discHits.Add(1)
		}
		time.Sleep(60 * time.Millisecond)
		inner.ServeHTTP(w, r)
	})
	a := newTestAuth(t, idp, &fakeResolver{id: AgentIdentity{Namespace: "kagent", Name: "helper"}}, "kagent/helper")
	srv := httptest.NewServer(a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)

	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(conn, "POST /mcp HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nContent-Length: 0\r\n\r\n", u.Host, forged(t, idp.srv.URL, "anything"))
	for deadline := time.Now().Add(2 * time.Second); discHits.Load() == 0 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	conn.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+idp.token(t, tokOpts{}))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("genuine agent got %d after another caller hung up", resp.StatusCode)
	}
	if d, j := discHits.Load(), jwksHits.Load(); d != 1 || j != 1 {
		t.Fatalf("discovery ran %d times and keys were loaded %d times; want 1 and 1", d, j)
	}
}

func TestFailedKeyLoadLeavesOneFetcher(t *testing.T) {
	idp := newFakeIdP(t)
	var failing atomic.Bool
	failing.Store(true)
	inner := idp.srv.Config.Handler
	idp.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/openid/v1/jwks" && failing.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		inner.ServeHTTP(w, r)
	})
	log, _ := quietLog()
	v := newVerifier(t, idp, log)
	tok := idp.token(t, tokOpts{})
	attempt := func() error {
		v.mu.Lock()
		v.lastDone = time.Time{} // stands in for the back-off elapsing
		v.mu.Unlock()
		_, _, err := v.Verify(context.Background(), tok)
		return err
	}
	for i := 0; i < 21; i++ {
		if attempt() == nil {
			t.Fatal("expected failure while the JWKS is down")
		}
	}
	buf := make([]byte, 4<<20)
	if n := strings.Count(string(buf[:runtime.Stack(buf, true)]), "jwk.(*AutoRefresh).refreshLoop"); n != 1 {
		t.Fatalf("21 failed key loads left %d refreshers running, want 1", n)
	}
	failing.Store(false)
	if err := attempt(); err != nil {
		t.Fatalf("keys must load once the JWKS is back: %v", err)
	}
}

func TestDiscoveryBackoffFailsFast(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln.Close() // nothing listens: connection refused, at once
	idp := newFakeIdP(t)
	log, _ := quietLog()
	v, err := NewActorVerifier(t.Context(), ActorJWTConfig{Issuer: "https://" + ln.Addr().String(), Audience: testAud, CAFile: idp.caFile}, log)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := v.Verify(context.Background(), "a.b.c"); err == nil {
		t.Fatal("expected failure")
	}
	start := time.Now()
	for i := 0; i < 50; i++ {
		_, _, err := v.Verify(context.Background(), "a.b.c")
		if err == nil || !strings.Contains(err.Error(), "backing off") {
			t.Fatalf("want a back-off error, got %v", err)
		}
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("50 callers during back-off took %s", d)
	}
}

// TestTokensNeverLogged: no rejection or acceptance path writes the bearer to the log.
func TestTokensNeverLogged(t *testing.T) {
	idp := newFakeIdP(t)
	log, logs := quietLog()
	res := &fakeResolver{id: AgentIdentity{Namespace: "kagent", Name: "helper"}}
	a, err := New(Options{Verifier: newVerifier(t, idp, log), Resolver: res, AllowedAgents: []string{"kagent/helper"}, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	toks := []string{idp.token(t, tokOpts{}), idp.token(t, tokOpts{exp: -time.Hour}), forged(t, idp.srv.URL, "k1")}
	for _, tok := range toks {
		call(a, tok)
	}
	res.id = AgentIdentity{Namespace: "kagent", Name: "other"}
	other := idp.token(t, tokOpts{name: "session-other"})
	toks = append(toks, other)
	call(a, other)
	for _, tok := range toks {
		sig := tok[strings.LastIndex(tok, ".")+1:]
		if strings.Contains(logs.String(), sig) {
			t.Fatalf("a token reached the log")
		}
	}
}

// TestKagentResolver runs the real gRPC client against a fake AgentIdentityService.
func TestKagentResolver(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var gotAuth, gotMethod string
	srv := grpc.NewServer(grpc.ForceServerCodec(rawCodec{}), grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
		gotMethod, _ = grpc.MethodFromServerStream(stream)
		md, _ := metadata.FromIncomingContext(stream.Context())
		if v := md.Get("authorization"); len(v) == 1 {
			gotAuth = v[0]
		}
		var req rawMessage
		if err := stream.RecvMsg(&req); err != nil {
			return err
		}
		return stream.SendMsg(encodeTestResponse())
	}))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	r, err := NewKagentResolver(KagentResolverConfig{Address: lis.Addr().String()})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	id, err := r.Resolve(t.Context(), "a.b.c")
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != resolveMethod || gotAuth != "Bearer a.b.c" {
		t.Fatalf("method=%q auth=%q", gotMethod, gotAuth)
	}
	if id.Namespace != "support" || id.Name != "ticket-assistant" || id.UID != "u1" || id.SessionID != "s1" || id.Labels["team"] != "support" {
		t.Fatalf("decoded %+v", id)
	}
}

func encodeTestResponse() rawMessage {
	str := func(b []byte, n protowire.Number, s string) []byte {
		b = protowire.AppendTag(b, n, protowire.BytesType)
		return protowire.AppendString(b, s)
	}
	var entry []byte
	entry = str(entry, 1, "team")
	entry = str(entry, 2, "support")
	var agent []byte
	agent = str(agent, 1, "support")
	agent = str(agent, 2, "ticket-assistant")
	agent = str(agent, 3, "u1")
	agent = protowire.AppendTag(agent, 4, protowire.BytesType)
	agent = protowire.AppendBytes(agent, entry)
	var out []byte
	out = protowire.AppendTag(out, 1, protowire.BytesType)
	out = protowire.AppendBytes(out, agent)
	out = str(out, 2, "s1")
	out = str(out, 3, "rev")
	return out
}
