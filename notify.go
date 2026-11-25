package main

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/spirilis/generic-go-mcp/transport"

	"github.com/spirilis/mcp-linux-over-ssh/session"
	"github.com/spirilis/mcp-linux-over-ssh/tools"
)

// callerScope keeps change notifications inside the caller they are about.
//
// The library's notification broker is one fan-out for the whole server: anyone subscribed
// to a URI is told when it changes, and anyone subscribed to list changes is told when
// anything joins or leaves the catalog. On a transport with one caller that is exactly
// right. On HTTP it means a caller who never sees another's session could still watch it
// work — every command makes a session go busy and ready, every session start and end
// changes the catalog — and "somebody else is running commands right now, this often" is
// not this caller's to know. Reads were already refused; this closes the signal.
//
// It wraps the protocol server *inside* the legacy overlay, because the overlay opens its
// own subscriptions/listen against whatever it wraps on behalf of a legacy client's GET
// stream. Sitting inside it covers both eras with one filter; sitting outside would cover
// neither, since the overlay's synthetic request never passes back out.
//
// Three notifications are scoped, each the way its shape allows:
//
//   - updates to one session's status or transcript: decided once, when the subscription
//     is made, by dropping every per-session URI the caller does not own from the filter it
//     asked for. Ownership never changes and ids are never reused, so there is nothing to
//     re-check later.
//   - updates to the sessions index: one URI every caller legitimately watches, whose
//     content differs per caller. The broker cannot say whose change triggered a
//     notification, so each owner has a counter that is advanced just before a
//     notification is sent on its behalf, and a stream relays the notification only if its
//     own owner's counter has moved since the last one it relayed.
//   - list changes: the same counter scheme.
//
// A notification means "re-read", never "here is the new state" (see CLAUDE.md), so
// collapsing two of an owner's notifications into one is harmless and suppressing someone
// else's loses nothing: a re-read would have shown this caller no difference.
type callerScope struct {
	inner transport.MessageHandler
	owner tools.OwnerFunc
	mgr   *session.Manager

	mu      sync.Mutex
	changes map[string]*ownerChanges
}

// ownerChanges counts what has changed for one owner. The counters only ever grow.
type ownerChanges struct {
	index   uint64 // the sessions index reads differently
	catalog uint64 // a session's resources joined or left
}

func newCallerScope(inner transport.MessageHandler, owner tools.OwnerFunc, mgr *session.Manager) *callerScope {
	return &callerScope{inner: inner, owner: owner, mgr: mgr, changes: map[string]*ownerChanges{}}
}

// indexChanged records that owner's view of the sessions index is about to be announced as
// changed. Call it before the NotifyUpdated it explains, never after: a stream compares
// counters when the notification arrives. Safe on a nil receiver, which is what a transport
// with one caller has.
func (c *callerScope) indexChanged(owner string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.forLocked(owner).index++
	c.mu.Unlock()
}

// catalogChanged is indexChanged for a session joining or leaving, which changes both.
func (c *callerScope) catalogChanged(owner string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	oc := c.forLocked(owner)
	oc.index++
	oc.catalog++
	c.mu.Unlock()
}

func (c *callerScope) forLocked(owner string) *ownerChanges {
	oc, ok := c.changes[owner]
	if !ok {
		oc = &ownerChanges{}
		c.changes[owner] = oc
	}
	return oc
}

func (c *callerScope) snapshot(owner string) ownerChanges {
	c.mu.Lock()
	defer c.mu.Unlock()
	return *c.forLocked(owner)
}

func (c *callerScope) HandleMessage(ctx context.Context, data []byte, w transport.ResponseWriter) {
	if !isSubscriptionStream(data) {
		c.inner.HandleMessage(ctx, data, w)
		return
	}

	who, err := c.owner(ctx)
	if err != nil {
		// Unreachable behind the auth middleware. Refused outright rather than served
		// unfiltered: a stream with no owner has nothing it is entitled to hear.
		var req struct {
			ID json.RawMessage `json:"id"`
		}
		_ = json.Unmarshal(data, &req)
		_ = w.WriteMessage(transport.NewErrorResponse(req.ID, &transport.RPCError{Code: transport.InvalidRequest, Message: err.Error()}))
		return
	}

	c.inner.HandleMessage(ctx, c.narrow(data, who), &scopedStream{
		ResponseWriter: w,
		scope:          c,
		owner:          who,
		seen:           c.snapshot(who),
	})
}

// narrow rewrites a subscriptions/listen request so it names no per-session URI the caller
// does not own. Anything that is not a per-session URI — the index, a skill file — is left
// alone.
//
// A URI for a session that does not exist is dropped exactly as one for somebody else's
// session is. The acknowledgement echoes the filter back, so keeping one and dropping the
// other would turn a subscription into a way to ask whether an id is real.
//
// A request that cannot be taken apart is passed through untouched: the server will reject
// it, or will find no subscriptions in it, and either way nothing was granted.
func (c *callerScope) narrow(data []byte, who string) []byte {
	var req map[string]json.RawMessage
	if json.Unmarshal(data, &req) != nil {
		return data
	}
	var params map[string]json.RawMessage
	if json.Unmarshal(req["params"], &params) != nil {
		return data
	}
	var filter map[string]json.RawMessage
	if json.Unmarshal(params["notifications"], &filter) != nil {
		return data
	}
	var uris []string
	if json.Unmarshal(filter["resourceSubscriptions"], &uris) != nil || len(uris) == 0 {
		return data
	}

	kept := make([]string, 0, len(uris))
	for _, uri := range uris {
		if id, perSession := tools.SessionIDFromURI(uri); perSession && !c.mgr.Owns(who, id) {
			continue
		}
		kept = append(kept, uri)
	}
	if len(kept) == len(uris) {
		return data
	}

	filter["resourceSubscriptions"], _ = json.Marshal(kept)
	params["notifications"], _ = json.Marshal(filter)
	req["params"], _ = json.Marshal(params)
	out, err := json.Marshal(req)
	if err != nil {
		return data
	}
	return out
}

// scopedStream is the writer one subscriptions/listen stream relays through. It is used by
// the single goroutine serving that stream, so seen needs no lock of its own.
type scopedStream struct {
	transport.ResponseWriter
	scope *callerScope
	owner string
	seen  ownerChanges
}

func (s *scopedStream) WriteNotification(method string, params interface{}) error {
	switch method {
	case "notifications/resources/updated":
		if uri, _ := notifiedURI(params); uri == tools.SessionsURI {
			now := s.scope.snapshot(s.owner)
			if now.index == s.seen.index {
				return nil // someone else's change; this caller's index reads the same
			}
			s.seen.index = now.index
		}
	case "notifications/resources/list_changed":
		now := s.scope.snapshot(s.owner)
		if now.catalog == s.seen.catalog {
			return nil
		}
		s.seen.catalog = now.catalog
	}
	return s.ResponseWriter.WriteNotification(method, params)
}

// notifiedURI reads the uri out of a resources/updated notification as the broker builds it.
func notifiedURI(params interface{}) (string, bool) {
	m, ok := params.(map[string]interface{})
	if !ok {
		return "", false
	}
	uri, ok := m["uri"].(string)
	return uri, ok
}
