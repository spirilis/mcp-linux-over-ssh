// Package tools implements the MCP tools this server exposes over its sessions.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spirilis/generic-go-mcp/mcp"

	"github.com/spirilis/mcp-linux-over-ssh/session"
)

// RunBashArguments is the run_bash input.
type RunBashArguments struct {
	SessionID string `json:"session_id"`
	Cmd       string `json:"cmd"`
}

// confirmSchema is the shape of the client's answer to the "may I run this?" elicitation.
var confirmSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"confirm": {"type": "boolean"}
	},
	"required": ["confirm"]
}`)

// NewRunBash builds the run_bash tool over mgr's sessions.
//
// The session is resolved per call rather than captured at construction: which host a
// command runs on is now the caller's choice, made by session_id, and the set of open
// sessions changes underneath this tool for its whole life.
//
// When requireConfirmation is set, the destructive-command warning in the description is
// enforced rather than merely stated: the first call to run_bash returns input_required and
// nothing executes until the caller retries with an accepted confirmation of that exact
// command. Clients that have not declared the elicitation capability cannot use the tool at
// all in that mode.
func NewRunBash(mgr *session.Manager, owner OwnerFunc, requireConfirmation bool) (mcp.Tool, mcp.ToolFunction) {
	return runBashDefinition(requireConfirmation, mgr.Gated()), func(ctx context.Context, req *mcp.ToolRequest) (mcp.Result, error) {
		// Ahead of everything, including the confirmation round trip: there is no point
		// asking a user to approve a command the server has already been told not to run,
		// and re-checking here means a flag raised *during* that round trip still stops the
		// replay. A session left open only because it was mid-command when the flag went up
		// is likewise unreachable from here, which is what lets the manager's poll loop
		// close it the moment it goes quiet.
		if err := mgr.Blocked(); err != nil {
			return mcp.ErrorResultf("cannot run a command: %v", err), nil
		}

		var args RunBashArguments
		if err := req.BindArguments(&args); err != nil {
			return mcp.ErrorResultf("invalid arguments: %v", err), nil
		}
		if strings.TrimSpace(args.Cmd) == "" {
			return mcp.ErrorResultf("cmd argument is required"), nil
		}

		sess, err := resolveSession(ctx, mgr, owner, strings.TrimSpace(args.SessionID))
		if err != nil {
			return mcp.ErrorResultf("%v", err), nil
		}

		if requireConfirmation {
			if !req.ClientCapabilities.HasElicitation() {
				return mcp.ErrorResultf("this tool requires user confirmation before executing any command, which requires elicitation support that the client did not declare"), nil
			}

			answer, asked := req.ElicitResponse("confirm")
			if !asked {
				// Which host is now a per-call choice, so the confirmation names it. A
				// prompt that only showed the command would be asking the user to approve
				// half the decision.
				return req.NeedInput(mcp.InputRequests{
					"confirm": mcp.NewElicitRequest("form",
						fmt.Sprintf("Execute this command on session %s (%s)?\n\n%s",
							args.SessionID, sess.Status().Command, args.Cmd),
						confirmSchema),
				})
			}
			if !answer.Accepted() {
				return mcp.ErrorResultf("command not executed: cancelled by user"), nil
			}
			var content struct {
				Confirm bool `json:"confirm"`
			}
			if err := json.Unmarshal(answer.Content, &content); err != nil || !content.Confirm {
				return mcp.ErrorResultf("command not executed: user declined"), nil
			}
		}

		res, err := sess.Execute(ctx, args.Cmd)
		if err != nil {
			// Session-level failures are things the model can reason about and sometimes
			// repair (notably ErrAwaitingPrompt, which tune_prompt_regex fixes), so they
			// belong in an error result rather than a protocol error.
			return mcp.ErrorResultf("%v", err), nil
		}

		return &mcp.ToolCallResult{
			Content:           []mcp.Content{mcp.Text(renderResult(res))},
			StructuredContent: structuredResult(res),
		}, nil
	}
}

// renderResult is what the model reads. A non-zero exit status is a legitimate outcome
// rather than a tool failure, so it is reported in the text and left off IsError.
func renderResult(res *session.Result) string {
	var b strings.Builder
	b.WriteString(res.Output)
	if res.Output != "" {
		b.WriteString("\n")
	}
	if res.Truncated {
		b.WriteString("\n[output was truncated to fit the configured limit]\n")
	}
	if res.HasExit {
		fmt.Fprintf(&b, "\n[exit code: %d]", res.ExitCode)
	} else {
		b.WriteString("\n[exit code unavailable: this session frames output by prompt match only]")
	}
	return b.String()
}

func structuredResult(res *session.Result) map[string]any {
	out := map[string]any{
		"output":      res.Output,
		"truncated":   res.Truncated,
		"duration_ms": res.Duration.Milliseconds(),
	}
	if res.HasExit {
		out["exit_code"] = res.ExitCode
	}
	return out
}

func runBashDefinition(requireConfirmation, gated bool) mcp.Tool {
	desc := "Executes a shell command on the remote host of an open session and returns its output and exit code. " +
		"Requires a session_id from start_session; call list_sessions if you do not have one, and check that the session's state is \"ready\" first. " +
		"WARNING: Do NOT use this tool for destructive commands or operations that may overwrite files without explicit user consent. " +
		"Each session is one shell running commands one at a time, so state such as the working directory and exported variables persists between calls on the same session_id — and is entirely separate between different session_ids. " +
		"Do not run interactive or full-screen programs (vim, top, less, or anything that reads stdin) — they will hang until the command timeout and force a session resynchronisation. " +
		"Never run anything that terminates the shell itself (exit, logout, kill on its own PID): that shell IS the session, the command that opened it cannot be re-run, and ending it kills the session for good — use end_session to close one deliberately. " +
		"To get a non-zero status without leaving the shell, use a subshell such as (exit 3)."
	if requireConfirmation {
		desc += " Every call requires the user to explicitly confirm the exact command before it runs — the tool will pause and ask."
	}
	if gated {
		desc += " " + disabledToolNote + " Sessions already open are terminated once whatever they were running finishes."
	}

	destructive, openWorld := true, true
	return mcp.Tool{
		Name:        "run_bash",
		Title:       "Run Remote Command",
		Description: desc,
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"session_id": {
					"type": "string",
					"description": "Which open session to run the command on, as returned by start_session or list_sessions"
				},
				"cmd": {
					"type": "string",
					"description": "The shell command to execute on the remote host"
				}
			},
			"required": ["session_id", "cmd"]
		}`),
		OutputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"output": {"type": "string", "description": "Combined output of the command"},
				"exit_code": {"type": "integer", "description": "Exit status; absent when the session frames output by prompt match only"},
				"truncated": {"type": "boolean", "description": "Whether output was shortened to fit the configured limit"},
				"duration_ms": {"type": "integer", "description": "Wall-clock execution time in milliseconds"}
			},
			"required": ["output", "truncated", "duration_ms"]
		}`),
		Annotations: &mcp.ToolAnnotations{
			Title:           "Run Remote Command",
			DestructiveHint: &destructive,
			OpenWorldHint:   &openWorld,
		},
	}
}
