// Package authn authenticates MCP callers on the HTTP transport. It satisfies generic-go-mcp's
// transport.AuthProvider and accepts two credentials, checked in this order:
//
//  1. a static bearer token, named in the config and read from a file (for curl, scripts,
//     CI). The caller is the token's name. A token may be discover-only: it lists the catalog
//     and nothing else.
//  2. a Substrate actor JWT, which the atenet egress gateway injects for a kagent
//     RemoteMCPServer with spec.actorJwt. The token is verified against this cluster's own
//     Substrate issuer (TLS to it pinned to the local servicedns CA bundle), resolved to its
//     kagent Agent through the local kagent controller, and checked against an allowlist.
//
// There is deliberately no anonymous mode and no OAuth: New refuses to build an
// Authenticator with no credential configured, so every admitted request is attributed to a
// token name or an Agent. What sits behind this server is a privileged shell, and "who ran
// that" has to have an answer.
//
// Adapted from mcp-kube-baker's internal/authn, itself from eric-small-mcps
// youtube-mcp/internal/authn, without OAuth, additional issuers or discover-only agents.
package authn

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Kind is how a principal authenticated.
type Kind string

// Principal kinds.
const (
	KindAgent  Kind = "agent"
	KindStatic Kind = "static"
)

// Principal is the authenticated caller.
type Principal struct {
	// ID is stable per caller: "agent:<ns>/<name>" or "static:<name>".
	ID   string
	Kind Kind
	// Login is the agent's "namespace/name" or the static token's name.
	Login string
	// Scope is what the caller may do. The zero value grants nothing.
	Scope Scope
	// SessionID is the kagent session for agent principals.
	SessionID string
	// Actor ("atespace/actorName") and ActorUID are the Substrate actor the token was issued
	// to: the one session sandbox behind an agent principal.
	Actor    string
	ActorUID string
}

// Owner identifies what a session opened by this caller belongs to: the Agent *and* the
// kagent session (one conversation) the request came from.
//
// The Agent alone is too wide. One Agent serves many conversations, usually on behalf of
// different people, and a shell's transcript holds whatever was printed in it — so two
// conversations with the same Agent must not see each other's shells. Too narrow costs a
// session that outlives its conversation and is collected by the idle reaper; too wide
// costs one person reading another's terminal.
//
// The conversation is the session id the kagent controller reported. Should a controller
// ever report none, the owner falls back to the actor — the sandbox the token was issued
// to, which is one per conversation and comes from the verified token itself — rather than
// silently widening to everything the Agent has open.
//
// A static token has no conversation to narrow it: the token is the caller, so everyone
// holding one token shares its sessions, and two tokens never do. That is the reason to give
// each script or service its own token.
func (p Principal) Owner() string {
	if p.Kind == KindStatic {
		return p.ID
	}
	if p.SessionID != "" {
		return p.ID + "#session:" + p.SessionID
	}
	return p.ID + "#actor:" + p.Actor
}

type ctxKey struct{}

// WithPrincipal returns ctx carrying p.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// PrincipalFrom returns the principal stored by Middleware.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}

// Options configures New. At least one credential must be configured: static tokens, or
// actor JWTs (Verifier and Resolver, always together).
type Options struct {
	// StaticTokens are bearer tokens compared in constant time, checked first.
	StaticTokens []StaticToken
	// Verifier and Resolver enable actor JWTs. Neither works without the other.
	Verifier *ActorVerifier
	Resolver AgentResolver
	// AllowedAgents lists "namespace/name" patterns; either half may be a glob ("research/*",
	// "team-*/reporter"). Empty rejects every agent.
	AllowedAgents []string
	Realm         string
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// StaticToken is one named bearer token.
type StaticToken struct {
	Name  string
	Value string
	// DiscoverOnly limits the token to listing the catalog.
	DiscoverOnly bool
}

// MinStaticTokenLen is the shortest static token accepted: 32 characters of `openssl rand
// -hex 32` or `-base64 32` output and up.
const MinStaticTokenLen = 32

var staticNameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9_.]{0,61}[a-z0-9])?$`)

type staticEntry struct {
	name  string
	sum   [32]byte
	scope Scope
}

// Authenticator is the composite authenticator.
type Authenticator struct {
	static   []staticEntry
	verifier *ActorVerifier
	resolver AgentResolver
	allowed  []agentPattern
	realm    string
	log      *slog.Logger

	mu    sync.Mutex
	cache map[[32]byte]cachedAgent
	now   func() time.Time
}

type cachedAgent struct {
	p   Principal
	exp time.Time
}

// How long, and how many, resolved agent tokens are remembered.
const (
	agentCacheTTL = 5 * time.Minute
	agentCacheMax = 10000
)

// New builds an Authenticator.
func New(opts Options) (*Authenticator, error) {
	a := &Authenticator{
		verifier: opts.Verifier,
		resolver: opts.Resolver,
		realm:    opts.Realm,
		log:      opts.Logger,
		cache:    map[[32]byte]cachedAgent{},
		now:      time.Now,
	}
	if a.log == nil {
		a.log = slog.Default()
	}
	if a.realm == "" {
		a.realm = "mcp"
	}
	if (a.verifier == nil) != (a.resolver == nil) {
		return nil, errors.New("actor JWT auth needs both a verifier and an agent resolver")
	}
	var err error
	if a.static, err = parseStaticTokens(opts.StaticTokens); err != nil {
		return nil, err
	}
	if a.verifier == nil && len(a.static) == 0 {
		return nil, errors.New("no authentication configured: enable actor_jwt or static_tokens; this server does not serve HTTP anonymously")
	}
	if a.verifier == nil {
		if len(opts.AllowedAgents) > 0 {
			return nil, errors.New("allowed agents are listed but actor JWT auth is off")
		}
		return a, nil
	}
	if a.allowed, err = parseAgentPatterns(opts.AllowedAgents); err != nil {
		return nil, err
	}
	if len(a.allowed) == 0 {
		a.log.Warn("actor JWT auth is on but no agents are allowed; every agent will be refused")
	}
	return a, nil
}

// parseStaticTokens checks the static tokens and keeps only their digests.
func parseStaticTokens(tokens []StaticToken) ([]staticEntry, error) {
	var out []staticEntry
	names := map[string]bool{}
	for _, t := range tokens {
		switch {
		case !staticNameRE.MatchString(t.Name):
			return nil, fmt.Errorf("static token name %q must be lowercase letters, digits, '-', '_' or '.', at most 63", t.Name)
		case names[t.Name]:
			return nil, fmt.Errorf("static token %q is listed twice", t.Name)
		case len(t.Value) < MinStaticTokenLen:
			return nil, fmt.Errorf("static token %q must be at least %d characters (openssl rand -hex 32)", t.Name, MinStaticTokenLen)
		case strings.Count(t.Value, ".") == 2:
			// A captured actor JWT pasted here would become a credential that never expires.
			return nil, fmt.Errorf("static token %q looks like a JWT; use a random string (openssl rand -hex 32)", t.Name)
		}
		names[t.Name] = true
		e := staticEntry{name: t.Name, sum: sha256.Sum256([]byte(t.Value)), scope: ScopeFull}
		if t.DiscoverOnly {
			e.scope = ScopeDiscover
		}
		for _, other := range out {
			if other.sum == e.sum {
				// Sessions belong to the token's name; one value, one name.
				return nil, fmt.Errorf("static tokens %q and %q have the same value", other.name, t.Name)
			}
		}
		out = append(out, e)
	}
	return out, nil
}

// matchStatic compares token with every static token in constant time.
func (a *Authenticator) matchStatic(token string) (staticEntry, bool) {
	sum := sha256.Sum256([]byte(token))
	found := -1
	for i := range a.static {
		if subtle.ConstantTimeCompare(sum[:], a.static[i].sum[:]) == 1 {
			found = i
		}
	}
	if found < 0 {
		return staticEntry{}, false
	}
	return a.static[found], true
}

// Middleware admits authenticated requests, stores their Principal in the context, and
// enforces its Scope. Every other request is answered here, before any JSON-RPC parsing:
// there is no path through it that reaches next without a Principal.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	next = a.gate(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, hasBearer := bearer(r)
		if !hasBearer {
			a.unauthorized(w, "missing bearer token")
			return
		}
		if e, ok := a.matchStatic(token); ok {
			p := Principal{ID: "static:" + e.name, Kind: KindStatic, Login: e.name, Scope: e.scope}
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
			return
		}
		if a.verifier == nil {
			a.unauthorized(w, "invalid token")
			return
		}
		p, err := a.authenticateAgent(r.Context(), token)
		if err != nil {
			a.log.Warn("actor JWT rejected", "error", err)
			if errors.Is(err, errNotAllowed) {
				a.forbidden(w, err.Error())
			} else {
				a.unauthorized(w, "invalid actor token")
			}
			return
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}

var errNotAllowed = errors.New("agent is not allowed to use this server")

func (a *Authenticator) authenticateAgent(ctx context.Context, token string) (Principal, error) {
	key := sha256.Sum256([]byte(token))
	a.mu.Lock()
	if c, ok := a.cache[key]; ok && a.now().Before(c.exp) {
		a.mu.Unlock()
		return c.p, nil
	}
	a.mu.Unlock()

	// Verify first, resolve second: resolving an unverified token would make this server an
	// oracle forwarding arbitrary bearer strings to the kagent controller.
	actor, exp, err := a.verifier.Verify(ctx, token)
	if err != nil {
		return Principal{}, err
	}
	id, err := a.resolver.Resolve(ctx, token)
	if err != nil {
		return Principal{}, err
	}
	// An honest controller returns Kubernetes object names. Refuse anything else before it
	// reaches the allowlist, a principal id or a log line.
	if !validAgentName(id.Namespace, id.Name) {
		return Principal{}, fmt.Errorf("agent lookup returned an invalid name (%q, %q)", clip(id.Namespace), clip(id.Name))
	}
	agent := id.Namespace + "/" + id.Name
	if !matchAgent(a.allowed, id.Namespace, id.Name) {
		return Principal{}, fmt.Errorf("%w: %s", errNotAllowed, agent)
	}
	p := Principal{ID: "agent:" + agent, Kind: KindAgent, Login: agent, SessionID: id.SessionID,
		Actor: actor.Atespace + "/" + actor.ActorName, ActorUID: actor.ActorUID, Scope: ScopeFull}
	a.log.Debug("actor JWT accepted", "agent", agent, "session", id.SessionID, "expires", exp.Format(time.RFC3339))

	a.mu.Lock()
	now := a.now()
	for k, c := range a.cache { // tokens live ≤1h; sweeping on insert keeps the map small
		if !now.Before(c.exp) {
			delete(a.cache, k)
		}
	}
	// A cached answer is not re-checked, so keep it briefly: the agent's deletion, a session's
	// end and a key's removal are then noticed within minutes, not at the token's expiry.
	if limit := now.Add(agentCacheTTL); exp.After(limit) {
		exp = limit
	}
	if len(a.cache) < agentCacheMax {
		a.cache[key] = cachedAgent{p: p, exp: exp}
	}
	a.mu.Unlock()
	return p, nil
}

// clip bounds an untrusted string before it is logged.
func clip(s string) string {
	if len(s) > 96 {
		return s[:96] + "…"
	}
	return s
}

func bearer(r *http.Request) (string, bool) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "bearer") || strings.TrimSpace(token) == "" {
		return "", false
	}
	return strings.TrimSpace(token), true
}

func (a *Authenticator) unauthorized(w http.ResponseWriter, msg string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="`+a.realm+`"`)
	writeErr(w, http.StatusUnauthorized, "invalid_token", msg)
}

func (a *Authenticator) forbidden(w http.ResponseWriter, msg string) {
	writeErr(w, http.StatusForbidden, "insufficient_scope", msg)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": msg})
}

// RegisterRoutes and RegisterAdminRoutes implement transport.AuthProvider. This authenticator
// has no endpoints of its own: the credential arrives on the request, minted elsewhere.
func (a *Authenticator) RegisterRoutes(*http.ServeMux) {}

// RegisterAdminRoutes implements transport.AuthProvider.
func (a *Authenticator) RegisterAdminRoutes(*http.ServeMux) {}

// UserFromContext implements transport.AuthProvider.
func (a *Authenticator) UserFromContext(ctx context.Context) (id, login string, ok bool) {
	p, ok := PrincipalFrom(ctx)
	if !ok {
		return "", "", false
	}
	return p.ID, p.Login, true
}
