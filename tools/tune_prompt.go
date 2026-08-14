package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spirilis/generic-go-mcp/mcp"

	"github.com/spirilis/mcp-linux-over-ssh/session"
)

// TunePromptArguments is the tune_prompt_regex input.
type TunePromptArguments struct {
	SessionID string `json:"session_id"`
	Regex     string `json:"regex"`
	Apply     bool   `json:"apply"`
}

// NewTunePromptRegex builds the tool that lets the caller repair a session's prompt regex at
// runtime.
//
// Screen-scraping a shell hinges entirely on recognising its prompt, and a regex written
// blind against an unfamiliar bastion often does not match on the first try. Pairing this
// tool with the session transcript resource turns that from a restart-and-guess loop into
// something the model can resolve in place: read what the shell actually printed, test a
// pattern against it, then apply the one that matches.
//
// The regex is a property of one running session, not of the process: ssh.prompt_regex is
// itself a template, so two sessions opened against different hosts can legitimately need
// different patterns, and repairing one must not disturb the other.
func NewTunePromptRegex(mgr *session.Manager) (mcp.Tool, mcp.ToolFunction) {
	return tunePromptDefinition(), func(ctx context.Context, req *mcp.ToolRequest) (mcp.Result, error) {
		var args TunePromptArguments
		if err := req.BindArguments(&args); err != nil {
			return mcp.ErrorResultf("invalid arguments: %v", err), nil
		}
		if strings.TrimSpace(args.Regex) == "" {
			return mcp.ErrorResultf("regex argument is required"), nil
		}

		sess, err := resolveSession(mgr, strings.TrimSpace(args.SessionID))
		if err != nil {
			return mcp.ErrorResultf("%v", err), nil
		}

		matches, err := sess.TestPromptRegex(args.Regex, 10)
		if err != nil {
			return mcp.ErrorResultf("%v", err), nil
		}

		var b strings.Builder
		if len(matches) == 0 {
			b.WriteString("The regex compiles but matches nothing in the retained transcript.\n")
			fmt.Fprintf(&b, "Read the %s resource to see what the shell actually printed.\n", TranscriptURI(args.SessionID))
		} else {
			fmt.Fprintf(&b, "The regex matched %d place(s) in the retained transcript:\n", len(matches))
			for _, m := range matches {
				fmt.Fprintf(&b, "  %q\n", m)
			}
		}

		if !args.Apply {
			b.WriteString("\nNot applied (apply was false).")
			return &mcp.ToolCallResult{
				Content:           []mcp.Content{mcp.Text(b.String())},
				StructuredContent: map[string]any{"matches": matches, "applied": false},
			}, nil
		}

		if len(matches) == 0 {
			b.WriteString("\nNot applied: refusing to install a regex that matches nothing, which would leave the session unusable.")
			return &mcp.ToolCallResult{
				Content:           []mcp.Content{mcp.Text(b.String())},
				StructuredContent: map[string]any{"matches": matches, "applied": false},
			}, nil
		}

		if err := sess.SetPromptRegex(args.Regex); err != nil {
			return mcp.ErrorResultf("%v", err), nil
		}

		resyncErr := sess.Resync(ctx)
		status := sess.Status()
		if resyncErr != nil {
			fmt.Fprintf(&b, "\nApplied, but the session did not reach a prompt afterwards: %v\nSession state is now %q.", resyncErr, status.State)
		} else {
			fmt.Fprintf(&b, "\nApplied and the session resynchronised. Session state is now %q.", status.State)
		}

		return &mcp.ToolCallResult{
			Content: []mcp.Content{mcp.Text(b.String())},
			StructuredContent: map[string]any{
				"matches":  matches,
				"applied":  true,
				"resynced": resyncErr == nil,
				"state":    status.State,
			},
		}, nil
	}
}

func tunePromptDefinition() mcp.Tool {
	return mcp.Tool{
		Name:  "tune_prompt_regex",
		Title: "Tune Shell Prompt Regex",
		Description: "Tests, and optionally installs, the regular expression one session uses to recognise its remote shell's prompt. " +
			"Use it when a session is stuck in the awaiting_prompt state or run_bash reports that the prompt regex has not matched: read that session's " +
			"transcript resource (mcp+stdio:///session/<session_id>/transcript, shown by list_sessions) to see what the shell actually printed, then test a Go (RE2) regex against it here. " +
			"With apply set to false the tool only reports what the pattern would match. With apply set to true it installs the pattern and " +
			"resynchronises the session, which is what promotes a session out of the awaiting_prompt state. " +
			"The change affects only the named session; other open sessions keep their own patterns.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"session_id": {
					"type": "string",
					"description": "Which open session's prompt regex to tune, as returned by start_session or list_sessions"
				},
				"regex": {
					"type": "string",
					"description": "Go (RE2) regular expression matching the shell prompt, e.g. (?m)^[^\\n]*[$#]\\s*$"
				},
				"apply": {
					"type": "boolean",
					"description": "Install the regex and resynchronise the session. When false (the default) the regex is only tested against the transcript.",
					"default": false
				}
			},
			"required": ["session_id", "regex"]
		}`),
		OutputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"matches": {"type": "array", "items": {"type": "string"}, "description": "What the regex matched in the retained transcript"},
				"applied": {"type": "boolean"},
				"resynced": {"type": "boolean"},
				"state": {"type": "string"}
			},
			"required": ["matches", "applied"]
		}`),
	}
}
