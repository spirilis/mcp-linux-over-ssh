package tools

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/spirilis/mcp-linux-over-ssh/session"
)

// stdio has no natural URI scheme for server-local resources, so this server invents one,
// as the library's UNIX-socket example does with mcp+unix://.
const (
	// SessionsURI indexes every live session. Unlike the per-session resources it is
	// registered for the process lifetime, which is what keeps the server advertising a
	// resources capability at all when no session has been started yet — a client that
	// caches capabilities from its first handshake would otherwise never look again.
	SessionsURI = "mcp+stdio:///sessions"

	sessionURIPrefix = "mcp+stdio:///session/"
)

// StatusURI is where a single session's state is published.
func StatusURI(id string) string { return sessionURIPrefix + id + "/status" }

// TranscriptURI is where a single session's raw shell output is published.
func TranscriptURI(id string) string { return sessionURIPrefix + id + "/transcript" }

// resolveSession looks up a session id supplied by a caller.
//
// An id that names nothing is an ordinary mistake — a session that was ended, or one from
// before a restart — so the error names the tool that can list what actually exists rather
// than leaving the model to guess.
func resolveSession(mgr *session.Manager, id string) (*session.Session, error) {
	if id == "" {
		return nil, fmt.Errorf("session_id is required; call start_session to open a session, or list_sessions to see the ones already open")
	}
	sess, ok := mgr.Get(id)
	if !ok {
		return nil, fmt.Errorf("%w: %q is not an open session; call list_sessions to see what is open, or start_session to open one", session.ErrNoSuchSession, id)
	}
	return sess, nil
}

// bindParameterValues extracts the declared parameters from a start_session call.
//
// Values are read as raw JSON rather than straight into map[string]string so that a client
// sending a port or a boolean — which the generated schema asks for as a string, but which
// clients get wrong often enough to be worth absorbing — is coerced rather than rejected.
// Nothing is loosened by this: every value still has to pass its configured pattern.
func bindParameterValues(raw json.RawMessage) (map[string]string, error) {
	if len(raw) == 0 {
		return map[string]string{}, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("arguments must be a JSON object: %w", err)
	}

	values := make(map[string]string, len(fields))
	for name, rawVal := range fields {
		v, err := scalarString(rawVal)
		if err != nil {
			return nil, fmt.Errorf("parameter %q: %w", name, err)
		}
		values[name] = v
	}
	return values, nil
}

// scalarString renders one JSON scalar as the string a template will interpolate.
func scalarString(raw json.RawMessage) (string, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", err
	}
	switch t := v.(type) {
	case string:
		return t, nil
	case float64:
		// Formatted as the shortest representation that round-trips, so 8022 stays 8022
		// rather than becoming 8022.000000.
		return strconv.FormatFloat(t, 'f', -1, 64), nil
	case bool:
		return strconv.FormatBool(t), nil
	default:
		return "", fmt.Errorf("must be a string, not %T", v)
	}
}
