package tools

import (
	"context"
	"encoding/json"

	"github.com/spirilis/generic-go-mcp/mcp"

	"github.com/spirilis/mcp-linux-over-ssh/session"
)

// NewIsDisabledByFlag builds the is_disabled_by_flag tool.
//
// main.go registers it only when an administrative gate is actually configured, so its
// presence in tools/list is itself the answer to "can this server be disabled at all?".
// That is the reason it is a separate tool rather than a field somewhere: a tool that is
// absent cannot be misread, whereas a permanently-false flag on some other result teaches a
// model to ignore the field on the servers where it matters.
//
// It answers about the whole server, not about any one session, which is why it takes no
// arguments and works with nothing open.
func NewIsDisabledByFlag(mgr *session.Manager) (mcp.Tool, mcp.ToolFunction) {
	return isDisabledDefinition(), func(ctx context.Context, req *mcp.ToolRequest) (mcp.Result, error) {
		blocked := mgr.Blocked()

		structured := map[string]any{"disabled": blocked != nil}
		text := "false — this server is not administratively disabled. start_session and run_bash are available."
		if blocked != nil {
			// The reason names the flag file, which is what a human reading this transcript
			// needs in order to clear it. Deliberately not reported in the false case: there
			// is nothing to act on, and the path to a kill switch is not something to hand
			// out to a caller that is currently free to execute commands.
			structured["reason"] = blocked.Error()
			text = "true — " + blocked.Error() +
				"\n\nNo session can be started and no command can be run until a person clears this. " +
				"Open sessions are being terminated as they stop running whatever they are running. " +
				"Report it to the user and stop; retrying cannot change the answer."
		}

		return &mcp.ToolCallResult{
			Content:           []mcp.Content{mcp.Text(text)},
			StructuredContent: structured,
		}, nil
	}
}

func isDisabledDefinition() mcp.Tool {
	readOnly, idempotent := true, true
	openWorld := false

	return mcp.Tool{
		Name:  "is_disabled_by_flag",
		Title: "Check Administrative Disable",
		Description: "Reports whether this server is currently disabled by its operator's flag file: true means no session can be started and no command can be run until a person clears it. " +
			"Takes no arguments and works whether or not anything is open. " +
			"Call it when start_session or run_bash refuses and you are not sure whether the cause is your request or the server, and before reporting a failure to the user. " +
			"A false answer is only true of the moment it was asked — the operator can raise the flag at any time, including between two of your calls — so treat it as an explanation for a refusal rather than a guarantee about the next one.",
		InputSchema: json.RawMessage(`{"type": "object", "properties": {}, "additionalProperties": false}`),
		OutputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"disabled": {"type": "boolean", "description": "True when the operator's flag file is present and this server is refusing all session activity"},
				"reason": {"type": "string", "description": "Why it is disabled, naming the flag file; present only when disabled is true"}
			},
			"required": ["disabled"]
		}`),
		Annotations: &mcp.ToolAnnotations{
			Title:          "Check Administrative Disable",
			ReadOnlyHint:   readOnly,
			IdempotentHint: idempotent,
			OpenWorldHint:  &openWorld,
		},
	}
}
