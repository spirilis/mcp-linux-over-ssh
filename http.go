package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/spirilis/generic-go-mcp/logging"
	"github.com/spirilis/generic-go-mcp/transport"

	"github.com/spirilis/mcp-linux-over-ssh/authn"
	"github.com/spirilis/mcp-linux-over-ssh/config"
	"github.com/spirilis/mcp-linux-over-ssh/listener"
	"github.com/spirilis/mcp-linux-over-ssh/session"
)

// errNoCaller is what a tool or resource answers when a request reached it over HTTP with no
// authenticated caller attached. The auth middleware makes that unreachable; this is the
// second lock on the same door, so that a future change which mounts the handler somewhere
// the middleware is not fails closed instead of handing out stdio's see-everything owner.
var errNoCaller = errors.New("this request carries no authenticated caller, so it cannot be attributed to anyone and is refused")

// authenticatedOwner is the tools.OwnerFunc for HTTP mode.
func authenticatedOwner(ctx context.Context) (string, error) {
	p, ok := authn.PrincipalFrom(ctx)
	if !ok || p.ID == "" {
		return "", errNoCaller
	}
	return p.Owner(), nil
}

// newHTTPHandler builds everything the listener serves: the probes, and /mcp behind
// authentication.
//
// It takes the authenticator as a concrete, required argument on purpose. The library treats
// a nil HTTPTransportConfig.AuthService as "authentication is off" and serves /mcp to anyone
// — a sensible default for a library and exactly wrong here — so the one place that field is
// assigned refuses to proceed without something to assign. TestHTTPRefusesAnonymousCallers
// pins the consequence against the handler this function returns, which is the same one the
// process serves.
func newHTTPHandler(a *authn.Authenticator, mcpHandler transport.MessageHandler, legacy transport.LegacySessions, allowedOrigins []string) (http.Handler, *transport.HTTPTransport, error) {
	if a == nil {
		return nil, nil, errors.New("refusing to serve HTTP without an authenticator")
	}
	trans := transport.NewHTTPTransport(transport.HTTPTransportConfig{
		AuthService:    a,
		AllowedOrigins: allowedOrigins,
		LegacySessions: legacy,
	})

	mux := http.NewServeMux()
	// Outside auth: a probe carries no token, and neither answer says anything about the
	// environment.
	listener.HealthRoutes(mux)
	mux.Handle("/mcp", trans.Handler(mcpHandler))
	return mux, trans, nil
}

// buildAuthenticator assembles authentication from the config: static tokens, actor JWTs, or
// both. ctx bounds the verifier's background issuer discovery and key refresh.
func buildAuthenticator(ctx context.Context, cfg *config.Config, log *slog.Logger) (*authn.Authenticator, func(), error) {
	read, err := cfg.ReadStaticTokens()
	if err != nil {
		return nil, nil, err
	}
	opts := authn.Options{Realm: serverName, Logger: log}
	for _, t := range read {
		opts.StaticTokens = append(opts.StaticTokens, authn.StaticToken{Name: t.Name, Value: t.Value, DiscoverOnly: t.DiscoverOnly})
	}

	closeAuth := func() {}
	if cfg.ActorJWTEnabled() {
		a := cfg.ActorJWT
		if opts.Verifier, err = authn.NewActorVerifier(ctx, authn.ActorJWTConfig{Issuer: a.Issuer, Audience: a.Audience, CAFile: a.CAFile}, log); err != nil {
			return nil, nil, err
		}
		resolver, err := authn.NewKagentResolver(authn.KagentResolverConfig{
			Address: a.KagentController.Address, TLS: a.KagentController.TLS, CAFile: a.KagentController.CAFile,
		})
		if err != nil {
			return nil, nil, err
		}
		opts.Resolver, opts.AllowedAgents = resolver, a.AllowedAgents
		closeAuth = func() { _ = resolver.Close() }
	}

	auth, err := authn.New(opts)
	if err != nil {
		closeAuth()
		return nil, nil, err
	}
	for _, t := range read {
		scope := "full"
		if t.DiscoverOnly {
			scope = "discover"
		}
		log.Info("static token enabled", "name", t.Name, "principal", "static:"+t.Name, "scope", scope)
	}
	if cfg.ActorJWTEnabled() {
		a := cfg.ActorJWT
		log.Info("actor JWT authentication enabled", "issuer", a.Issuer, "audience", a.Audience,
			"kagent_controller", a.KagentController.Address, "allowed_agents", len(a.AllowedAgents))
	}
	return auth, closeAuth, nil
}

// slogLogger mirrors the logging: section for the authn package, which logs through log/slog
// directly because generic-go-mcp's logger is not exported. It writes where the rest of the
// server's logging goes — logging.file if set, stderr otherwise — so a refused token and the
// request it belonged to end up in the same place. trace maps to debug.
func slogLogger(level, format string, out io.Writer) *slog.Logger {
	if out == nil {
		out = os.Stderr
	}
	var l slog.Level
	switch level {
	case "trace", "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: l}
	if format == "json" {
		return slog.New(slog.NewJSONHandler(out, opts))
	}
	return slog.New(slog.NewTextHandler(out, opts))
}

// serveHTTP runs the server in http mode until a signal arrives, and returns the process
// exit code. main calls os.Exit on the result, which skips its deferred CloseAll, so every
// path out of here closes the sessions itself: a shell left to die of PTY hangup is not good
// enough when the far end is a privileged login.
//
// Shutdown is a signal and nothing else. There is no stdin whose closing means the client is
// done, so none of the stdio path's EOF-and-drain machinery applies — and the library's
// DoneNotifier, which the stdio notes in main.go explain is a deadlock there, is not
// consulted here either: an HTTP transport has no natural end to report.
func serveHTTP(cfg *config.Config, mgr *session.Manager, handler transport.MessageHandler, legacy transport.LegacySessions, level string, logOut io.Writer) int {
	defer mgr.CloseAll()

	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	auth, closeAuth, err := buildAuthenticator(ctx, cfg, slogLogger(level, cfg.Lib.Logging.Format, logOut))
	if err != nil {
		logging.Error("error initializing authentication", "error", err)
		return 1
	}
	defer closeAuth()

	httpCfg := cfg.Lib.Server.HTTP
	root, trans, err := newHTTPHandler(auth, handler, legacy, httpCfg.AllowedOrigins)
	if err != nil {
		logging.Error("error building the HTTP handler", "error", err)
		return 1
	}

	var certs *listener.CertReloader
	if cfg.TLSEnabled() {
		certs, err = listener.NewCertReloader(cfg.TLS.CertFile, cfg.TLS.KeyFile, func(err error) {
			logging.Error("TLS certificate reload failed", "error", err)
		})
		if err != nil {
			logging.Error("error loading TLS certificate", "error", err)
			return 1
		}
	}

	srv := listener.New(net.JoinHostPort(httpCfg.Host, strconv.Itoa(httpCfg.Port)), root, certs)
	if err := srv.Start(); err != nil {
		logging.Error("error starting HTTP listener", "error", err)
		return 1
	}
	logging.Info("MCP server ready on HTTP",
		"addr", srv.Addr().String(),
		"tls", srv.TLS(),
		"legacy_compat", legacy != nil,
		"max_sessions", cfg.SSH.MaxSessions,
		"idle_timeout", cfg.SSH.IdleTimeout,
		"parameters", len(cfg.Renderer.Parameters()),
		"actor_jwt", cfg.ActorJWTEnabled(),
		"static_tokens", len(cfg.StaticTokens),
		"probes", "/healthz /readyz")

	<-ctx.Done()
	logging.Info("shutting down", "reason", "signal")

	code := 0
	// The transport first, which ends its legacy SSE keep-alives; then the listener, which
	// drops every connection and with them any command still in flight.
	if err := trans.Stop(); err != nil {
		logging.Error("error stopping transport", "error", err)
		code = 1
	}
	if err := srv.Stop(); err != nil {
		logging.Error("error stopping HTTP listener", "error", err)
		code = 1
	}
	// Explicitly, before the line that claims it; the defer covers the early returns.
	mgr.CloseAll()
	logging.Info("shutdown complete")
	return code
}
