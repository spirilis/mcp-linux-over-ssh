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
	Regex string `json:"regex"`
	Apply bool   `json:"apply"`
}

// NewTunePromptRegex builds the tool that lets the caller repair the prompt regex at
// runtime.
//
// Screen-scraping a shell hinges entirely on recognising its prompt, and a regex written
// blind against an unfamiliar bastion often does not match on the first try. Pairing this
// tool with the session transcript resource turns that from a restart-and-guess loop into
// something the model can resolve in place: read what the shell actually printed, test a
// pattern against it, then apply the one that matches.
func NewTunePromptRegex(sess *session.Session) (mcp.Tool, mcp.ToolFunction) {
	return tunePromptDefinition(), func(ctx context.Context, req *mcp.ToolRequest) (mcp.Result, error) {
		var args TunePromptArguments
		if err := req.BindArguments(&args); err != nil {
			return mcp.ErrorResultf("invalid arguments: %v", err), nil
		}
		if strings.TrimSpace(args.Regex) == "" {
			return mcp.ErrorResultf("regex argument is required"), nil
		}

		matches, err := sess.TestPromptRegex(args.Regex, 10)
		if err != nil {
			return mcp.ErrorResultf("%v", err), nil
		}

		var b strings.Builder
		if len(matches) == 0 {
			b.WriteString("The regex compiles but matches nothing in the retained transcript.\n")
			b.WriteString("Read the mcp+stdio:///session/transcript resource to see what the shell actually printed.\n")
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
		Description: "Tests, and optionally installs, the regular expression this server uses to recognise the remote shell's prompt. " +
			"Use it when run_bash reports that the prompt regex has not matched, or when the session is stuck: read the " +
			"mcp+stdio:///session/transcript resource to see what the shell actually printed, then test a Go (RE2) regex against it here. " +
			"With apply set to false the tool only reports what the pattern would match. With apply set to true it installs the pattern and " +
			"resynchronises the session, which is what promotes a session out of the awaiting_prompt state.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
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
			"required": ["regex"]
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
