package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/spirilis/generic-go-mcp/mcp"

	"github.com/spirilis/mcp-linux-over-ssh/session"
)

// NewListSessions builds the list_sessions tool.
//
// With nothing started at boot, a session id is the only handle on a live privileged login,
// and it exists in exactly one place: the reply to the start_session call that created it.
// A model whose context no longer holds that reply would otherwise have no way back to a
// session it is still paying for. This is that way back, and it is also the readiness check
// start_session points at.
func NewListSessions(mgr *session.Manager) (mcp.Tool, mcp.ToolFunction) {
	return listSessionsDefinition(), func(ctx context.Context, req *mcp.ToolRequest) (mcp.Result, error) {
		infos := mgr.List()

		return &mcp.ToolCallResult{
			Content:           []mcp.Content{mcp.Text(renderSessions(infos, mgr.Max()))},
			StructuredContent: map[string]any{"sessions": sessionSummaries(infos), "count": len(infos), "max_sessions": mgr.Max()},
		}, nil
	}
}

// sessionSummaries flattens the manager's view into what a client sees. Session.Info nests
// the whole status snapshot; a caller of this tool wants one row per session.
func sessionSummaries(infos []session.Info) []map[string]any {
	out := make([]map[string]any, 0, len(infos))
	for _, in := range infos {
		row := map[string]any{
			"session_id":     in.ID,
			"state":          in.Status.State,
			"command":        in.Status.Command,
			"commands_run":   in.Status.CommandsRun,
			"created_at":     in.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
			"status_uri":     StatusURI(in.ID),
			"transcript_uri": TranscriptURI(in.ID),
		}
		if len(in.Parameters) > 0 {
			row["parameters"] = in.Parameters
		}
		if in.Status.LastError != "" {
			row["last_error"] = in.Status.LastError
		}
		out = append(out, row)
	}
	return out
}

func renderSessions(infos []session.Info, max int) string {
	if len(infos) == 0 {
		return "No sessions are open. Call start_session to open one."
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d session(s) open, of a maximum %d:\n\n", len(infos), max)
	for _, in := range infos {
		fmt.Fprintf(&b, "%s  [%s]\n", in.ID, in.Status.State)
		fmt.Fprintf(&b, "    command:      %s\n", in.Status.Command)
		if len(in.Parameters) > 0 {
			fmt.Fprintf(&b, "    parameters:   %s\n", formatParameters(in.Parameters))
		}
		fmt.Fprintf(&b, "    commands run: %d\n", in.Status.CommandsRun)
		fmt.Fprintf(&b, "    opened:       %s\n", in.CreatedAt.UTC().Format("2006-01-02 15:04:05 UTC"))
		if in.Status.LastError != "" {
			fmt.Fprintf(&b, "    last error:   %s\n", in.Status.LastError)
		}
		switch in.Status.State {
		case session.StateAwaitingPrompt.String():
			fmt.Fprintf(&b, "    -> the prompt regex has not matched; read %s and repair it with tune_prompt_regex\n", TranscriptURI(in.ID))
		case session.StateStarting.String():
			b.WriteString("    -> still logging in; wait for \"ready\" before running a command\n")
		case session.StateDead.String():
			b.WriteString("    -> this session is gone and cannot be revived; end_session clears it and frees its slot\n")
		case session.StateDesynced.String():
			b.WriteString("    -> the shell could not be resynchronised; end_session and start a new one\n")
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatParameters(params map[string]string) string {
	names := make([]string, 0, len(params))
	for k := range params {
		names = append(names, k)
	}
	sort.Strings(names)

	parts := make([]string, 0, len(names))
	for _, k := range names {
		parts = append(parts, fmt.Sprintf("%s=%s", k, params[k]))
	}
	return strings.Join(parts, ", ")
}

func listSessionsDefinition() mcp.Tool {
	return mcp.Tool{
		Name:  "list_sessions",
		Title: "List SSH Sessions",
		Description: "Lists every SSH session currently open on this server, with its id, state, the command that opened it, and the parameters it was opened with. " +
			"Use it to find the id of a session you have lost track of, and to check whether a session started with start_session has finished logging in — state \"ready\" means it can run commands. " +
			"An empty list means nothing is connected and you need start_session first.",
		InputSchema: json.RawMessage(`{"type": "object", "properties": {}, "additionalProperties": false}`),
		OutputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"sessions": {
					"type": "array",
					"description": "One entry per open session",
					"items": {
						"type": "object",
						"properties": {
							"session_id": {"type": "string"},
							"state": {"type": "string", "description": "starting, awaiting_prompt, ready, busy, desynced or dead"},
							"command": {"type": "string"},
							"parameters": {"type": "object", "description": "Values this session was opened with"},
							"commands_run": {"type": "integer"},
							"created_at": {"type": "string"},
							"last_error": {"type": "string"},
							"status_uri": {"type": "string"},
							"transcript_uri": {"type": "string"}
						},
						"required": ["session_id", "state", "command", "commands_run", "created_at", "status_uri", "transcript_uri"]
					}
				},
				"count": {"type": "integer"},
				"max_sessions": {"type": "integer", "description": "How many sessions may be open at once"}
			},
			"required": ["sessions", "count", "max_sessions"]
		}`),
		Annotations: &mcp.ToolAnnotations{
			Title:          "List SSH Sessions",
			ReadOnlyHint:   true,
			IdempotentHint: true,
		},
	}
}
