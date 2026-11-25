package listener

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writePair writes a fresh self-signed cert for 127.0.0.1 with the given CN and returns
// its DER.
func writePair(t *testing.T, certFile, keyFile, cn string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:    x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		t.Fatal(err)
	}
	return der
}

// servedCN connects and reports the CN of the certificate the server presented.
func servedCN(t *testing.T, addr string) string {
	t.Helper()
	conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // inspecting the served cert
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	return conn.ConnectionState().PeerCertificates[0].Subject.CommonName
}

func TestTLSServesAndReloadsRotatedCertificates(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	writePair(t, certFile, keyFile, "first")

	var reloadErrs []error
	certs, err := NewCertReloader(certFile, keyFile, func(err error) { reloadErrs = append(reloadErrs, err) })
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Now()
	certs.now = func() time.Time { return clock }

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") })
	s := New("127.0.0.1:0", mux, certs)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	addr := s.Addr().String()

	if cn := servedCN(t, addr); cn != "first" {
		t.Fatalf("served %q, want first", cn)
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec
	resp, err := client.Get("https://" + addr + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("healthz over TLS: %d", resp.StatusCode)
	}
	if resp, err := http.Get("http://" + addr + "/healthz"); err == nil {
		resp.Body.Close()
		if resp.StatusCode == 200 {
			t.Fatal("plain HTTP was served on the TLS port")
		}
	}

	// Rotation: new files on disk, picked up once the check interval passes.
	writePair(t, certFile, keyFile, "second")
	if cn := servedCN(t, addr); cn != "first" {
		t.Fatalf("re-read before the check interval: %q", cn)
	}
	clock = clock.Add(11 * time.Second)
	if cn := servedCN(t, addr); cn != "second" {
		t.Fatalf("after rotation served %q, want second", cn)
	}

	// A half-written rotation (key does not match the cert) keeps the previous pair.
	otherKey := filepath.Join(dir, "other.key")
	writePair(t, filepath.Join(dir, "other.crt"), otherKey, "third")
	b, _ := os.ReadFile(otherKey)
	if err := os.WriteFile(keyFile, b, 0o600); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(11 * time.Second)
	if cn := servedCN(t, addr); cn != "second" {
		t.Fatalf("a mismatched pair replaced the good one: %q", cn)
	}
	if len(reloadErrs) == 0 {
		t.Error("a failed reload was not reported")
	}
}

func TestPlainHTTPAndStartupErrors(t *testing.T) {
	s := New("127.0.0.1:0", http.NotFoundHandler(), nil)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get("http://" + s.Addr().String() + "/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if s.TLS() || resp.StatusCode != 404 {
		t.Fatalf("plain server: tls=%v status=%d", s.TLS(), resp.StatusCode)
	}
	// A second server on the same port fails at Start, not later.
	if err := New(s.Addr().String(), http.NotFoundHandler(), nil).Start(); err == nil {
		t.Fatal("binding a used port succeeded")
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	dir := t.TempDir()
	if _, err := NewCertReloader(filepath.Join(dir, "none.crt"), filepath.Join(dir, "none.key"), nil); err == nil {
		t.Fatal("missing cert files accepted at startup")
	}
}
