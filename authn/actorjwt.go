// Adapted from mcp-kube-baker internal/authn (local issuer only).

package authn

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/lestrrat-go/jwx/jwk"
)

// reservedRuntimeAudience is kagent's own runtime audience. A token that also carries it would
// grant runtime authority wherever it leaks, so we refuse it outright (kagent does the same).
const reservedRuntimeAudience = "kagent-runtime"

const clockSkew = 30 * time.Second

// maxTokenLifetime is the longest exp-iat accepted. kagent mints 5 to 60 minutes; a longer token
// is not one of its, and would sit in caches and logs for as long as it claims.
const maxTokenLifetime = time.Hour

// Discovery runs in the background, never on a caller's request: a caller who hangs up must
// not be able to cancel it for everyone else.
const (
	discoveryTimeout = 15 * time.Second
	discoveryBackoff = 10 * time.Second
)

// ActorIdentity is Substrate's private "actor.ate.dev" claim.
type ActorIdentity struct {
	Atespace  string `json:"atespace"`
	ActorName string `json:"actorName"`
	ActorUID  string `json:"actorUID"`
}

type actorClaims struct {
	jwt.RegisteredClaims
	Actor ActorIdentity `json:"actor.ate.dev"`
}

// ActorJWTConfig configures ActorVerifier.
type ActorJWTConfig struct {
	// Issuer is this cluster's Substrate actor JWT issuer, e.g. https://idp.ate-system.svc.
	Issuer string
	// Audience is the value this server requires in "aud" (RemoteMCPServer.spec.actorJwt.audiences).
	Audience string
	// CAFile is the PEM bundle the issuer's TLS certificate must chain to: this cluster's
	// Substrate servicedns signer, normally a projected ClusterTrustBundle. Required, and the
	// only roots trusted for the issuer: the system roots are never consulted, so a token can
	// only be vouched for by the issuer inside this cluster.
	CAFile string
}

// errUnknownKey marks a token whose kid is not among the issuer's signing keys.
var errUnknownKey = errors.New("signing key is not trusted")

// ActorVerifier verifies the actor JWTs of this cluster's Substrate install, with keys from
// live OIDC discovery. Discovery is lazy and retried, so the server can start before the idp
// is reachable; until it succeeds every actor token is rejected.
//
// There is deliberately no way to trust a second issuer, or to pin keys exported from one.
type ActorVerifier struct {
	cfg    ActorJWTConfig
	client *http.Client
	log    *slog.Logger

	ctx context.Context // the server's lifetime

	mu       sync.Mutex
	auto     *jwk.AutoRefresh // created once, reused by every attempt
	jwksURL  string           // set once discovery and the first key load have succeeded
	inflight chan struct{}    // closed when the running discovery attempt ends
	lastDone time.Time        // when the last failed attempt ended
	lastErr  error
}

// NewActorVerifier validates configuration and reads the CA bundle; it does not contact the
// issuer.
func NewActorVerifier(ctx context.Context, cfg ActorJWTConfig, log *slog.Logger) (*ActorVerifier, error) {
	if cfg.Audience == "" || strings.TrimSpace(cfg.Audience) != cfg.Audience {
		return nil, errors.New("actor JWT audience must be set and have no surrounding whitespace")
	}
	if cfg.Audience == reservedRuntimeAudience || cfg.Audience == agentInfoAudience {
		return nil, fmt.Errorf("actor JWT audience %q is reserved by kagent", cfg.Audience)
	}
	if err := validateHTTPSURL(cfg.Issuer); err != nil {
		return nil, fmt.Errorf("actor JWT issuer: %w", err)
	}
	if cfg.CAFile == "" {
		return nil, errors.New("actor JWT ca_file is required: the issuer's certificate is verified against this cluster's Substrate CA bundle only")
	}
	if log == nil {
		log = slog.Default()
	}
	tlsCfg, err := tlsConfigFromCA(cfg.CAFile)
	if err != nil {
		return nil, err
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tlsCfg
	tr.Proxy = nil // the issuer is a cluster-internal Service; never route its keys through a proxy
	return &ActorVerifier{
		cfg: cfg,
		log: log,
		ctx: ctx,
		client: &http.Client{
			Transport: limitBody{tr, 1 << 20}, Timeout: 10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// limitBody caps response bodies: the JWKS library reads them whole.
type limitBody struct {
	rt  http.RoundTripper
	max int64
}

func (l limitBody) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := l.rt.RoundTrip(r)
	if err == nil {
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.LimitReader(resp.Body, l.max), resp.Body}
	}
	return resp, err
}

// Issuer returns the "iss" this verifier accepts.
func (v *ActorVerifier) Issuer() string { return v.cfg.Issuer }

// keySet returns the current signing keys.
func (v *ActorVerifier) keySet(ctx context.Context) (jwk.Set, error) {
	keys, jwksURL, err := v.ensureKeys(ctx)
	if err != nil {
		return nil, err
	}
	set, err := keys.Fetch(ctx, jwksURL)
	if err != nil {
		return nil, fmt.Errorf("get signing keys: %w", err)
	}
	return set, nil
}

// tlsConfigFromCA trusts exactly the certificates in caFile. Empty caFile means the system
// roots; only the kagent resolver's optional TLS uses that.
func tlsConfigFromCA(caFile string) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile == "" {
		return cfg, nil
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read CA bundle: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("CA bundle %s holds no PEM certificates", caFile)
	}
	cfg.RootCAs = pool
	return cfg, nil
}

func validateHTTPSURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%q is not an HTTPS URL without credentials, query or fragment", raw)
	}
	return nil
}

// ensureKeys returns the key source once discovery has succeeded. Until then it starts (at most
// one) background attempt, waits for it as long as the caller is willing, and reports why keys
// are not available. Unknown kids never trigger fetches, so tokens can't drive traffic.
func (v *ActorVerifier) ensureKeys(ctx context.Context) (*jwk.AutoRefresh, string, error) {
	v.mu.Lock()
	if v.jwksURL != "" {
		auto, u := v.auto, v.jwksURL
		v.mu.Unlock()
		return auto, u, nil
	}
	if v.inflight == nil {
		if wait := discoveryBackoff - time.Since(v.lastDone); !v.lastDone.IsZero() && wait > 0 {
			err := v.lastErr
			v.mu.Unlock()
			return nil, "", fmt.Errorf("actor JWT issuer discovery is backing off after: %w", err)
		}
		v.inflight = make(chan struct{})
		go v.discover(v.inflight)
	}
	done := v.inflight
	v.mu.Unlock()

	select {
	case <-done:
	case <-ctx.Done():
		return nil, "", fmt.Errorf("actor JWT issuer discovery still running: %w", ctx.Err())
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.jwksURL == "" {
		return nil, "", v.lastErr
	}
	return v.auto, v.jwksURL, nil
}

// discover makes one discovery attempt on the server's own context and records the outcome.
func (v *ActorVerifier) discover(done chan struct{}) {
	ctx, cancel := context.WithTimeout(v.ctx, discoveryTimeout)
	defer cancel()
	jwksURL, err := v.discoverOnce(ctx)

	v.mu.Lock()
	if err != nil {
		v.lastErr, v.lastDone = err, time.Now() // back off from the END of the attempt
		v.log.Warn("actor JWT issuer discovery failed", "issuer", v.cfg.Issuer, "error", err)
	} else {
		v.jwksURL = jwksURL
		v.log.Info("actor JWT issuer ready", "issuer", v.cfg.Issuer, "jwks", jwksURL)
	}
	v.inflight = nil
	v.mu.Unlock()
	close(done)
}

func (v *ActorVerifier) discoverOnce(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(v.cfg.Issuer, "/")+"/.well-known/openid-configuration", nil)
	if err != nil {
		return "", err
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch actor JWT discovery: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("actor JWT discovery returned HTTP %d", resp.StatusCode)
	}
	var disc struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&disc); err != nil {
		return "", fmt.Errorf("decode actor JWT discovery: %w", err)
	}
	if disc.Issuer != v.cfg.Issuer {
		return "", fmt.Errorf("discovery issuer %q does not match configured issuer %q", disc.Issuer, v.cfg.Issuer)
	}
	if err := validateHTTPSURL(disc.JWKSURI); err != nil {
		return "", fmt.Errorf("discovery jwks_uri: %w", err)
	}
	// The keys must come from the issuer itself: otherwise the discovery document could make
	// this server fetch any URL it likes, every minute.
	if iu, _ := url.Parse(v.cfg.Issuer); !sameHost(iu, disc.JWKSURI) {
		return "", fmt.Errorf("discovery jwks_uri %q is not on the issuer's host", disc.JWKSURI)
	}
	// One refresher for the life of the process. A failed first load leaves it registered, and
	// the next attempt reuses it instead of abandoning a goroutine that polls forever.
	v.mu.Lock()
	if v.auto == nil {
		v.auto = jwk.NewAutoRefresh(v.ctx)
		errs := make(chan jwk.AutoRefreshError, 1)
		v.auto.ErrorSink(errs)
		go func() {
			for {
				select {
				case <-v.ctx.Done():
					return
				case e := <-errs:
					v.log.Error("refresh actor JWT signing keys", "error", e.Error)
				}
			}
		}()
	}
	auto := v.auto
	v.mu.Unlock()
	auto.Configure(disc.JWKSURI, jwk.WithHTTPClient(v.client), jwk.WithRefreshInterval(time.Minute))
	if _, err := auto.Refresh(ctx, disc.JWKSURI); err != nil {
		return "", fmt.Errorf("load actor JWT signing keys: %w", err)
	}
	return disc.JWKSURI, nil
}

func sameHost(issuer *url.URL, raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && issuer != nil && strings.EqualFold(u.Host, issuer.Host)
}

// Verify checks signature, issuer, audience, expiry and the Substrate identity claims. It returns
// the actor identity and the token's expiry.
func (v *ActorVerifier) Verify(ctx context.Context, raw string) (ActorIdentity, time.Time, error) {
	set, err := v.keySet(ctx)
	if err != nil {
		return ActorIdentity{}, time.Time{}, err
	}
	claims := &actorClaims{}
	_, err = jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		kid, ok := t.Header["kid"].(string)
		if !ok || kid == "" {
			return nil, errors.New("token has no kid")
		}
		key, ok := set.LookupKeyID(kid)
		if !ok || key.Algorithm() != t.Method.Alg() || (key.KeyUsage() != "" && key.KeyUsage() != "sig") {
			return nil, errUnknownKey
		}
		var pub any
		if err := key.Raw(&pub); err != nil {
			return nil, fmt.Errorf("decode signing key: %w", err)
		}
		return pub, nil
	},
		jwt.WithValidMethods([]string{"ES256", "RS256"}),
		jwt.WithIssuer(v.cfg.Issuer), jwt.WithAudience(v.cfg.Audience),
		jwt.WithExpirationRequired(), jwt.WithIssuedAt(), jwt.WithLeeway(clockSkew))
	if err != nil {
		return ActorIdentity{}, time.Time{}, fmt.Errorf("verify actor JWT: %w", err)
	}
	if slices.Contains(claims.Audience, reservedRuntimeAudience) {
		return ActorIdentity{}, time.Time{}, errors.New("actor JWT also grants kagent runtime authority")
	}
	if claims.IssuedAt == nil || claims.ExpiresAt.Sub(claims.IssuedAt.Time) > maxTokenLifetime+clockSkew {
		return ActorIdentity{}, time.Time{}, fmt.Errorf("actor JWT lifetime exceeds %s", maxTokenLifetime)
	}
	a := claims.Actor
	// The same keys sign worker-for-actor/... tokens; only actor/<atespace>/<name> is accepted.
	if a.Atespace == "" || a.ActorName == "" || a.ActorUID == "" ||
		strings.Contains(a.Atespace+a.ActorName, "/") ||
		claims.Subject != "actor/"+a.Atespace+"/"+a.ActorName ||
		claims.NotBefore == nil || claims.IssuedAt == nil {
		return ActorIdentity{}, time.Time{}, errors.New("actor JWT has incomplete or inconsistent identity claims")
	}
	return a, claims.ExpiresAt.Time, nil
}
