package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spirilis/generic-go-mcp/mcp"

	"github.com/spirilis/mcp-linux-over-ssh/session"
)

// EndSessionArguments is the end_session input.
type EndSessionArguments struct {
	SessionID string `json:"session_id"`
}

// NewEndSession builds the end_session tool.
//
// Ending is not optional housekeeping here: every session is a privileged login the far end
// is auditing, and they are capped. A command still running on the session is cancelled
// rather than waited for — see Manager.End.
func NewEndSession(mgr *session.Manager, owner OwnerFunc) (mcp.Tool, mcp.ToolFunction) {
	return endSessionDefinition(), func(ctx context.Context, req *mcp.ToolRequest) (mcp.Result, error) {
		var args EndSessionArguments
		if err := req.BindArguments(&args); err != nil {
			return mcp.ErrorResultf("invalid arguments: %v", err), nil
		}
		id := strings.TrimSpace(args.SessionID)
		if id == "" {
			return mcp.ErrorResultf("session_id is required; call list_sessions to see which sessions are open"), nil
		}

		who, err := owner(ctx)
		if err != nil {
			return mcp.ErrorResultf("%v", err), nil
		}

		if !mgr.End(who, id) {
			// Already gone is the outcome the caller wanted, but saying so plainly beats
			// reporting success for an id that may simply be a typo.
			return mcp.ErrorResultf("%v: %q is not an open session, so there was nothing to end; call list_sessions to see what is open", session.ErrNoSuchSession, id), nil
		}

		remaining := mgr.List(who)
		var b strings.Builder
		fmt.Fprintf(&b, "Session %s has been closed and its shell terminated.\n", id)
		fmt.Fprintf(&b, "%d session(s) still open.", len(remaining))

		return &mcp.ToolCallResult{
			Content:           []mcp.Content{mcp.Text(b.String())},
			StructuredContent: map[string]any{"session_id": id, "ended": true, "remaining": len(remaining)},
		}, nil
	}
}

func endSessionDefinition() mcp.Tool {
	destructive := true
	return mcp.Tool{
		Name:  "end_session",
		Title: "Close Session",
		Description: "Closes a session opened with start_session, terminating its shell and freeing its slot. " +
			"Call this as soon as you are done with a session: each one is a real privileged login held open on the remote side, and only a limited number may exist at once. " +
			"Any command still running on that session is cancelled. The id becomes invalid immediately and the session cannot be reopened — start_session gives you a new one.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"session_id": {
					"type": "string",
					"description": "The id returned by start_session, as shown by list_sessions"
				}
			},
			"required": ["session_id"],
			"additionalProperties": false
		}`),
		OutputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"session_id": {"type": "string"},
				"ended": {"type": "boolean"},
				"remaining": {"type": "integer", "description": "How many sessions are still open"}
			},
			"required": ["session_id", "ended", "remaining"]
		}`),
		Annotations: &mcp.ToolAnnotations{
			Title:           "Close Session",
			DestructiveHint: &destructive,
			IdempotentHint:  true,
		},
	}
}
