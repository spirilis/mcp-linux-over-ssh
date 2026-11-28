package authn

// Ported from mcp-kube-baker/internal/authn/scope.go (itself from genome-vcf-mcp, without its
// NoStreams switch). skills/list is among the listing methods: this server serves skills.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/spirilis/generic-go-mcp/compat"
	"github.com/spirilis/generic-go-mcp/transport"
)

// Scope is what an authenticated principal may do. The zero value grants nothing, so a code
// path that forgets to set it fails closed.
type Scope int

// Scopes.
const (
	ScopeNone     Scope = iota
	ScopeDiscover       // may list the catalog; may not call or read anything
	ScopeFull
)

func (s Scope) String() string {
	switch s {
	case ScopeDiscover:
		return "discover"
	case ScopeFull:
		return "full"
	}
	return "none"
}

// discoverOnlyCode is the JSON-RPC error a discover-only credential gets for anything but
// listing. It is implementation-defined, outside MCP's own -32020..-32099 range, and is not
// -32601: the method exists, this credential may not use it.
const (
	discoverOnlyCode    = -32003
	discoverOnlyMessage = "this credential may only list tools and resources"
)

// Methods a discover-only principal may call. Anything not listed is refused, so a method
// added later is denied until someone decides otherwise.
var discoverMethods = map[string]bool{
	"server/discover":          true,
	"tools/list":               true,
	"resources/list":           true,
	"resources/templates/list": true,
	"prompts/list":             true,
	"skills/list":              true,
}

// A legacy (2025-11-25 and earlier) client cannot reach tools/list without these.
var legacyHandshake = map[string]bool{
	"initialize":                true,
	"notifications/initialized": true,
	"ping":                      true,
}

func discoverAllowed(req transport.JSONRPCRequest) bool {
	return discoverMethods[req.Method] || (legacyHandshake[req.Method] && compat.Claims(req.Method, req.Params))
}

// A list request is a few hundred bytes; nothing a discover-only credential may send is larger.
const maxDiscoverBody = 1 << 20

// gate enforces the principal's scope on every request to /mcp, for both protocol eras. Full
// principals pass untouched. A discover-only principal's POST body is decoded exactly as the
// transport will decode it (same type, same decoder), and the method found there must be on the
// list above; the Mcp-Method header is the client's claim and is not consulted.
func (a *Authenticator) gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := PrincipalFrom(r.Context())
		switch p.Scope {
		case ScopeFull:
			next.ServeHTTP(w, r)
		case ScopeDiscover:
			// GET and DELETE are the legacy session's change-notification stream and its
			// teardown: they carry catalog changes only, which this principal may see.
			if r.Method == http.MethodPost {
				body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxDiscoverBody))
				var req transport.JSONRPCRequest
				if err != nil || json.Unmarshal(body, &req) != nil || !discoverAllowed(req) {
					a.log.Warn("discover-only credential refused", "principal", p.ID, "mcp_method", clip(req.Method))
					writeRPCRefusal(w, req.ID)
					return
				}
				r.Body = io.NopCloser(bytes.NewReader(body))
			}
			next.ServeHTTP(w, r)
		default:
			a.log.Error("request reached the scope gate without a scope; refusing", "principal", p.ID)
			a.forbidden(w, "principal has no scope")
		}
	})
}

// writeRPCRefusal answers 403 with a JSON-RPC error, never a tool result: isError would say the
// tool ran and failed, and invite a model to retry.
func writeRPCRefusal(w http.ResponseWriter, id json.RawMessage) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write(transport.NewErrorResponse(id, &transport.RPCError{Code: discoverOnlyCode, Message: discoverOnlyMessage}))
}
