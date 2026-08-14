package tools

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spirilis/generic-go-mcp/mcp"

	"github.com/spirilis/mcp-linux-over-ssh/config"
	"github.com/spirilis/mcp-linux-over-ssh/session"
)

// Everything a tool definition carries is protocol text: a client displays the titles, and a
// model reads the descriptions and schemas as its documentation. None of it may name the
// transport.
//
// SSH is what opens a session today. It is not what a session *is* — the engine drives a PTY
// and screen-scrapes a prompt, and would do the same over a serial console, a container exec
// or whatever replaces SSH here later. Every "SSH session" in a tool description was a
// promise this server does not actually make, and one that would have to be broken in the
// place it is most expensive to break: the description an already-deployed client has cached.
//
// The configured command in these fixtures is deliberately a real `ssh` command line, so a
// definition that echoed it back would fail this rather than sneak through.
func TestToolDefinitionsDoNotNameTheTransport(t *testing.T) {
	for _, tc := range allToolDefinitions(t) {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.tool)
			if err != nil {
				t.Fatalf("marshalling the tool definition: %v", err)
			}
			for _, line := range offendingLines(string(raw)) {
				t.Errorf("tool definition names the transport: %s", line)
			}
		})
	}
}

// The titles are the strings a human sees in a client's tool picker, so they get their own
// check: a title is short enough that "SSH" in it is the whole label rather than an aside.
func TestToolTitlesAreTransportNeutral(t *testing.T) {
	for _, tc := range allToolDefinitions(t) {
		titles := []string{tc.tool.Title}
		if tc.tool.Annotations != nil {
			titles = append(titles, tc.tool.Annotations.Title)
		}
		for _, title := range titles {
			if title == "" {
				continue
			}
			if strings.Contains(strings.ToLower(title), "ssh") {
				t.Errorf("%s title %q names the transport", tc.name, title)
			}
		}
	}
}

// is_disabled_by_flag exists only when a gate does, so the ungated fixture must not produce
// it and the gated one must. This is the invariant main.go's conditional Register enforces;
// asserting it here keeps the constructor honest even though main.go owns the decision.
func TestGatedToolsAppearOnlyWithAGate(t *testing.T) {
	var ungated, gated []string
	for _, tc := range allToolDefinitions(t) {
		if tc.gated {
			gated = append(gated, tc.tool.Name)
		} else {
			ungated = append(ungated, tc.tool.Name)
		}
	}

	if contains(ungated, "is_disabled_by_flag") {
		t.Error("is_disabled_by_flag was built for a manager with no gate")
	}
	if !contains(gated, "is_disabled_by_flag") {
		t.Error("is_disabled_by_flag was not built for a manager with a gate")
	}

	// The note only belongs on the tools a gate can actually refuse, and only when one
	// exists — an unconditional warning about a control the operator never configured is
	// noise a model has to reason past on every call.
	for _, tc := range allToolDefinitions(t) {
		mentions := strings.Contains(tc.tool.Description, "administratively disabled")
		switch tc.tool.Name {
		case "start_session", "run_bash":
			if mentions != tc.gated {
				t.Errorf("%s: mentions administrative disable = %v, want %v", tc.name, mentions, tc.gated)
			}
		}
	}
}

type toolCase struct {
	name  string
	tool  mcp.Tool
	gated bool
}

// allToolDefinitions builds every tool in both the gated and ungated shapes. Constructing a
// definition spawns nothing — the manager is only read for its cap and whether it has a gate
// — so this needs no shell and runs under -short.
func allToolDefinitions(t *testing.T) []toolCase {
	t.Helper()

	renderer, err := config.NewRenderer(config.SSHConfig{
		Command:     "ssh -tt psmuser@{{.host}}",
		PromptRegex: `(?m)^[^\n]*[$#]\s*$`,
		Parameters: []config.ParameterConfig{
			{Parameter: "host", Description: "Hostname of the target to connect to", Pattern: config.DefaultParameterPattern},
		},
	})
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}

	ungated := session.NewManager(4, 0)
	t.Cleanup(ungated.CloseAll)

	gated := session.NewManager(4, 0)
	t.Cleanup(gated.CloseAll)
	// Poll interval zero: this is about what the tools advertise, not about enforcement, and
	// a background goroutine would only add noise.
	gated.SetGate(session.NewFlagFileGate(filepath.Join(t.TempDir(), "disabled")), 0)

	var out []toolCase
	for _, m := range []struct {
		label string
		mgr   *session.Manager
		on    bool
	}{{"ungated", ungated, false}, {"gated", gated, true}} {
		add := func(tool mcp.Tool, _ mcp.ToolFunction) {
			out = append(out, toolCase{name: m.label + "/" + tool.Name, tool: tool, gated: m.on})
		}
		add(NewStartSession(m.mgr, renderer, session.Config{}))
		add(NewListSessions(m.mgr))
		add(NewEndSession(m.mgr))
		add(NewRunBash(m.mgr, true))
		add(NewTunePromptRegex(m.mgr))
		if m.on {
			add(NewIsDisabledByFlag(m.mgr))
		}
	}
	return out
}

// offendingLines returns the fragments of s that name the transport, with enough surrounding
// text to find them. Reported per occurrence rather than as one boolean so a failure says
// what to edit.
func offendingLines(s string) []string {
	lower := strings.ToLower(s)
	var out []string
	for i := 0; ; {
		j := strings.Index(lower[i:], "ssh")
		if j < 0 {
			return out
		}
		at := i + j
		start := max(at-60, 0)
		end := min(at+60, len(s))
		out = append(out, "..."+s[start:end]+"...")
		i = at + 3
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
