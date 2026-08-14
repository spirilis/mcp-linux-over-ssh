package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spirilis/generic-go-mcp/mcp"

	"github.com/spirilis/mcp-linux-over-ssh/config"
	"github.com/spirilis/mcp-linux-over-ssh/session"
)

// NewStartSession builds the start_session tool.
//
// Its input schema is generated from the parameters the config declares rather than being
// fixed, so the target of a connection is a first-class, described, client-validated
// argument that shows up in tools/list. That is the whole point of the parametric design:
// the model picks a target the operator has sanctioned, without a second tool call to learn
// what the choices are.
//
// base carries every session setting that is not per-connection (timeouts, PTY geometry,
// init commands); only Command and PromptRegex are rendered per session.
func NewStartSession(mgr *session.Manager, r *config.Renderer, base session.Config) (mcp.Tool, mcp.ToolFunction) {
	params := r.Parameters()

	return startSessionDefinition(params, mgr.Max(), mgr.Gated()), func(ctx context.Context, req *mcp.ToolRequest) (mcp.Result, error) {
		// First, before the arguments are even looked at. An administrative stop is a fact
		// about the server rather than about this request, so it is the honest answer no
		// matter what was asked for, and rendering a command line for a session that cannot
		// open is wasted work. Manager.Start checks again and is the actual enforcement
		// point; this is here for the message, which needs to tell a model to stop.
		if err := mgr.Blocked(); err != nil {
			return mcp.ErrorResultf("cannot start a session: %v", err), nil
		}

		values, err := bindParameterValues(req.Arguments)
		if err != nil {
			return mcp.ErrorResultf("invalid arguments: %v", err), nil
		}

		command, promptRegex, err := r.Render(values)
		if err != nil {
			return mcp.ErrorResultf("%v", err), nil
		}

		cfg := base
		cfg.Command, cfg.PromptRegex = command, promptRegex

		id, err := mgr.Start(cfg, values)
		if err != nil {
			return mcp.ErrorResultf("%v", err), nil
		}

		// Reported rather than waited for. The login handshake runs in the background,
		// so this is very often "starting"; see Manager.Start for why blocking on a
		// bastion login is the wrong trade.
		sess, ok := mgr.Get(id)
		state := session.StateStarting.String()
		if ok {
			state = sess.Status().State
		}

		var b strings.Builder
		fmt.Fprintf(&b, "Session %s is opening.\n\n", id)
		fmt.Fprintf(&b, "Command: %s\n", command)
		fmt.Fprintf(&b, "State:   %s\n\n", state)
		b.WriteString("The login is still in progress; it can take a while on a target that shows banners or asks for MFA. ")
		b.WriteString("Wait for the state to reach \"ready\" — call list_sessions, or read ")
		b.WriteString(StatusURI(id))
		b.WriteString(" — before running a command. A run_bash sent too early fails harmlessly with \"session is not ready\", so retrying is safe.\n")
		b.WriteString("If it reaches \"awaiting_prompt\" instead, the prompt regex did not match this host: read ")
		b.WriteString(TranscriptURI(id))
		b.WriteString(" and repair it with tune_prompt_regex.\n\n")
		b.WriteString("Call end_session with this id when finished — each session is a real privileged login and they are capped.")

		return &mcp.ToolCallResult{
			Content: []mcp.Content{mcp.Text(b.String())},
			StructuredContent: map[string]any{
				"session_id":     id,
				"state":          state,
				"command":        command,
				"status_uri":     StatusURI(id),
				"transcript_uri": TranscriptURI(id),
			},
		}, nil
	}
}

func startSessionDefinition(params []config.ParameterConfig, max int, gated bool) mcp.Tool {
	desc := "Opens a new shell session on a remote host and returns the session id that every other tool in this server takes. " +
		"Nothing is connected until you call this. " +
		"The session is a real login that stays open until you end it, so shell state (working directory, exported variables) persists across run_bash calls on the same id, " +
		"and several independent sessions can be open at once. " +
		fmt.Sprintf("At most %d may be open simultaneously; call end_session when you are finished with one. ", max) +
		"This returns as soon as the shell is spawned, before the login completes — check list_sessions or the session's status resource for state \"ready\" before running commands."
	if len(params) > 0 {
		desc += " Each argument selects which host this session connects to; they are validated against patterns the operator configured, and a rejected value is a configuration boundary rather than something to work around."
	}
	if gated {
		desc += " " + disabledToolNote
	}

	openWorld := true
	return mcp.Tool{
		Name:         "start_session",
		Title:        "Open Session",
		Description:  desc,
		InputSchema:  startSessionSchema(params),
		OutputSchema: startSessionOutputSchema(),
		Annotations: &mcp.ToolAnnotations{
			Title:         "Open Session",
			OpenWorldHint: &openWorld,
		},
	}
}

// startSessionSchema turns the declared parameters into JSON Schema, one required string
// property each.
//
// The configured pattern is folded into the description rather than emitted as the schema's
// own "pattern" keyword. Patterns are Go RE2, JSON Schema's are ECMA-262, and the two agree
// on the common cases but not on all of them — a client validating an operator's `(?i)` or
// `\p{L}` against the wrong dialect would reject a value this server would have accepted.
// The model still gets the rule; the authoritative check stays server-side in
// config.Renderer, which is where it has to be anyway.
func startSessionSchema(params []config.ParameterConfig) json.RawMessage {
	type property struct {
		Type        string `json:"type"`
		Description string `json:"description,omitempty"`
	}
	schema := struct {
		Type                 string              `json:"type"`
		Properties           map[string]property `json:"properties"`
		Required             []string            `json:"required,omitempty"`
		AdditionalProperties bool                `json:"additionalProperties"`
	}{
		Type:       "object",
		Properties: make(map[string]property, len(params)),
	}

	for _, p := range params {
		desc := p.Description
		if desc == "" {
			desc = "Value for the " + p.Parameter + " connection parameter."
		}
		// Operators do not reliably punctuate a YAML description, and running theirs
		// straight into ours reads as one garbled sentence.
		if !strings.HasSuffix(desc, ".") && !strings.HasSuffix(desc, "!") && !strings.HasSuffix(desc, "?") {
			desc += "."
		}
		desc += fmt.Sprintf(" Must match the regular expression %s.", p.Pattern)
		schema.Properties[p.Parameter] = property{Type: "string", Description: desc}
		schema.Required = append(schema.Required, p.Parameter)
	}

	// Marshalling rather than hand-writing the JSON: descriptions come from an operator's
	// config and will eventually contain a quote or a backslash.
	b, err := json.Marshal(schema)
	if err != nil {
		// Unreachable: every field above is a plain string, map or slice.
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return b
}

func startSessionOutputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"session_id": {"type": "string", "description": "Identifier to pass to run_bash, tune_prompt_regex and end_session"},
			"state": {"type": "string", "description": "Session state at the moment this returned; usually \"starting\", since the login runs in the background"},
			"command": {"type": "string", "description": "The rendered command line that opened this session"},
			"status_uri": {"type": "string", "description": "Resource URI reporting this session's live state"},
			"transcript_uri": {"type": "string", "description": "Resource URI carrying this session's raw shell output"}
		},
		"required": ["session_id", "state", "command", "status_uri", "transcript_uri"]
	}`)
}
