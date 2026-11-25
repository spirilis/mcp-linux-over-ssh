// Package listener owns mcp-linux-over-ssh's HTTP(S) server in http mode. generic-go-mcp's
// HTTPTransport serves plain HTTP only, so this server mounts the transport's /mcp handler
// (HTTPTransport.Handler) beside the probe and OAuth routes, and adds TLS with a certificate
// that is re-read from disk when it changes: cert-manager renews a Secret in place, and the
// kubelet swaps the mounted files without restarting the pod.
//
// One code path serves both: TLS is only whether a CertReloader is supplied.
package listener

import (
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"
)

// CertReloader serves a key pair from two files, re-reading them when either changes. A
// pair that fails to load (a half-written rotation, a key that does not match the cert)
// leaves the previous pair in service.
type CertReloader struct {
	certFile, keyFile string
	// checkEvery bounds how often a handshake re-reads the files.
	checkEvery time.Duration
	onError    func(error)

	mu        sync.Mutex
	cert      *tls.Certificate
	certPEM   []byte
	keyPEM    []byte
	lastCheck time.Time
	now       func() time.Time
}

// NewCertReloader loads the pair once; failing here is a startup error.
func NewCertReloader(certFile, keyFile string, onError func(error)) (*CertReloader, error) {
	r := &CertReloader{certFile: certFile, keyFile: keyFile, checkEvery: 10 * time.Second, onError: onError, now: time.Now}
	if r.onError == nil {
		r.onError = func(error) {}
	}
	if _, err := r.reload(); err != nil {
		return nil, err
	}
	return r, nil
}

// reload reads both files and swaps the pair in if their bytes changed. It reports whether
// a new pair was installed.
func (r *CertReloader) reload() (bool, error) {
	certPEM, err := os.ReadFile(r.certFile)
	if err != nil {
		return false, fmt.Errorf("read TLS certificate: %w", err)
	}
	keyPEM, err := os.ReadFile(r.keyFile)
	if err != nil {
		return false, fmt.Errorf("read TLS key: %w", err)
	}
	if r.cert != nil && bytes.Equal(certPEM, r.certPEM) && bytes.Equal(keyPEM, r.keyPEM) {
		return false, nil
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return false, fmt.Errorf("load TLS key pair %s / %s: %w", r.certFile, r.keyFile, err)
	}
	r.cert, r.certPEM, r.keyPEM = &pair, certPEM, keyPEM
	return true, nil
}

// GetCertificate implements tls.Config.GetCertificate.
func (r *CertReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if now := r.now(); now.Sub(r.lastCheck) >= r.checkEvery {
		r.lastCheck = now
		if _, err := r.reload(); err != nil {
			r.onError(fmt.Errorf("%w; still serving the previous certificate", err))
		}
	}
	return r.cert, nil
}

// Server is the http-mode listener.
type Server struct {
	srv *http.Server
	// tls is recorded rather than read back from srv.TLSConfig, which net/http fills in
	// for plain servers too.
	tls  bool
	ln   net.Listener
	done chan error
}

// New builds a server for addr. A nil certs serves plain HTTP. The timeouts match
// generic-go-mcp's own transport: bounded header and body reads against slow clients, and
// no WriteTimeout, which would sever subscriptions/listen and legacy SSE streams.
func New(addr string, handler http.Handler, certs *CertReloader) *Server {
	s := &Server{
		srv: &http.Server{
			Addr:              addr,
			Handler:           handler,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       30 * time.Second,
			IdleTimeout:       120 * time.Second,
		},
		done: make(chan error, 1),
	}
	if certs != nil {
		s.tls = true
		s.srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: certs.GetCertificate}
	}
	return s
}

// Start binds the address and serves in the background. A bind failure is returned here
// rather than logged later, so a port clash stops startup.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.srv.Addr)
	if err != nil {
		return err
	}
	s.ln = ln
	go func() {
		var err error
		if s.tls {
			err = s.srv.ServeTLS(ln, "", "")
		} else {
			err = s.srv.Serve(ln)
		}
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		s.done <- err
	}()
	return nil
}

// Addr is the bound address (useful with port 0).
func (s *Server) Addr() net.Addr { return s.ln.Addr() }

// TLS reports whether the server serves HTTPS.
func (s *Server) TLS() bool { return s.tls }

// Stop closes the listener and every connection, including open SSE streams, and returns
// the serve loop's error, if any.
func (s *Server) Stop() error {
	if err := s.srv.Close(); err != nil {
		return err
	}
	return <-s.done
}
