// Package tools implements the MCP tools this server exposes over its single shell.
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
	Cmd string `json:"cmd"`
}

// confirmSchema is the shape of the client's answer to the "may I run this?" elicitation.
var confirmSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"confirm": {"type": "boolean"}
	},
	"required": ["confirm"]
}`)

// NewRunBash builds the run_bash tool against sess.
//
// When requireConfirmation is set, the destructive-command warning in the description is
// enforced rather than merely stated: the first call to run_bash returns input_required and
// nothing executes until the caller retries with an accepted confirmation of that exact
// command. Clients that have not declared the elicitation capability cannot use the tool at
// all in that mode.
func NewRunBash(sess *session.Session, requireConfirmation bool) (mcp.Tool, mcp.ToolFunction) {
	return runBashDefinition(requireConfirmation), func(ctx context.Context, req *mcp.ToolRequest) (mcp.Result, error) {
		var args RunBashArguments
		if err := req.BindArguments(&args); err != nil {
			return mcp.ErrorResultf("invalid arguments: %v", err), nil
		}
		if strings.TrimSpace(args.Cmd) == "" {
			return mcp.ErrorResultf("cmd argument is required"), nil
		}

		if requireConfirmation {
			if !req.ClientCapabilities.HasElicitation() {
				return mcp.ErrorResultf("this tool requires user confirmation before executing any command, which requires elicitation support that the client did not declare"), nil
			}

			answer, asked := req.ElicitResponse("confirm")
			if !asked {
				return req.NeedInput(mcp.InputRequests{
					"confirm": mcp.NewElicitRequest("form",
						fmt.Sprintf("Execute this command on the remote host?\n\n%s", args.Cmd),
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

func runBashDefinition(requireConfirmation bool) mcp.Tool {
	desc := "Executes a shell command on the remote host over a persistent SSH session and returns its output and exit code. " +
		"WARNING: Do NOT use this tool for destructive commands or operations that may overwrite files without explicit user consent. " +
		"Commands run one at a time in a single shared shell, so state such as the working directory and exported variables persists between calls. " +
		"Do not run interactive or full-screen programs (vim, top, less, or anything that reads stdin) — they will hang until the command timeout and force a session resynchronisation. " +
		"Never run anything that terminates the shell itself (exit, logout, kill on its own PID): that shell IS the session, the underlying SSH command cannot be re-run, and ending it makes this server unusable until it is restarted. " +
		"To get a non-zero status without leaving the shell, use a subshell such as (exit 3)."
	if requireConfirmation {
		desc += " Every call requires the user to explicitly confirm the exact command before it runs — the tool will pause and ask."
	}

	destructive, openWorld := true, true
	return mcp.Tool{
		Name:        "run_bash",
		Title:       "Run Command Over SSH",
		Description: desc,
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"cmd": {
					"type": "string",
					"description": "The shell command to execute on the remote host"
				}
			},
			"required": ["cmd"]
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
			Title:           "Run Command Over SSH",
			DestructiveHint: &destructive,
			OpenWorldHint:   &openWorld,
		},
	}
}
