package config

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Example is one annotated configuration shown by --help-config.
//
// These are not prose. Every one is loaded and rendered by TestExamplesAllLoad, so an
// example that stops parsing — or stops matching what the parser actually accepts — fails
// the build rather than quietly misleading whoever copies it. That is the whole reason this
// lives in the config package instead of as a string in main.
type Example struct {
	// Name is the heading, e.g. "No parameters".
	Name string
	// Description is one line on when to reach for this shape.
	Description string
	// YAML is a complete, loadable configuration.
	YAML string
	// Params is how many parameters the example declares, and therefore how many
	// arguments its start_session takes. Asserted by the test.
	Params int
}

// Examples returns the configurations --help-config prints, simplest first.
func Examples() []Example {
	return []Example{
		{
			Name:        "No parameters — one fixed target",
			Description: "The command is a constant and start_session takes no arguments.",
			Params:      0,
			YAML: `server:
  mode: stdio
ssh:
  command: "ssh -tt psmuser@bastion.example.com"
  prompt_regex: '(?m)^[^\n]*[$#]\s*$'
`,
		},
		{
			Name:        "One parameter — the caller picks the host",
			Description: "start_session takes {\"host\": \"...\"}.",
			Params:      1,
			YAML: `server:
  mode: stdio
ssh:
  command: "ssh -tt psmuser@{{.host}}"
  prompt_regex: '(?m)^[^\n]*[$#]\s*$'
  parameters:
    - parameter: host
      description: "Hostname of the bastion to connect to"
`,
		},
		{
			Name:        "Two parameters — the caller picks the account and the host",
			Description: "start_session takes {\"user\": \"...\", \"host\": \"...\"}. prompt_regex is\ntemplated too, so it can match that user's own prompt exactly.",
			Params:      2,
			YAML: `server:
  mode: stdio
ssh:
  command: "ssh -tt {{.user}}@{{.host}}"
  prompt_regex: '(?m)^\[{{.user}}@[^\]]+\][$#]\s*$'
  parameters:
    - parameter: host
      description: "Hostname of the bastion to connect to"
    - parameter: user
      description: "PSM-crafted username encoding the target system"
      pattern: '^[a-z][a-z0-9_-]{0,63}$'
`,
		},
	}
}

// HelpConfig renders --help-config's output.
func HelpConfig() string {
	var b strings.Builder

	const title = "mcp-linux-over-ssh — configuration examples"
	b.WriteString(title + "\n")
	b.WriteString(rule("=", title) + "\n\n")
	b.WriteString("Nothing connects when this server starts. The ssh: block is a template for the\n")
	b.WriteString("sessions an MCP client opens on demand with the start_session tool, and\n")
	b.WriteString("ssh.parameters decides what that tool takes as arguments.\n\n")

	for i, ex := range Examples() {
		heading := fmt.Sprintf("%d. %s", i+1, ex.Name)
		fmt.Fprintf(&b, "%s\n%s\n", heading, rule("-", heading))
		fmt.Fprintf(&b, "%s\n\n", ex.Description)
		b.WriteString(indent(ex.YAML, "  "))
		b.WriteString("\n")
	}

	b.WriteString("Notes\n-----\n")
	fmt.Fprintf(&b, "  ssh.command is rendered and handed to `sh -c` ON THIS HOST, so every parameter\n")
	fmt.Fprintf(&b, "  value must match its pattern — %s unless you set one.\n", DefaultParameterPattern)
	b.WriteString("  That admits hostnames, usernames, IPs, paths and ports, and nothing a shell\n")
	b.WriteString("  reads as syntax. To accept looser values, widen the pattern AND wrap it:\n\n")
	b.WriteString("      command: \"ssh -tt {{.user}}@{{.host}} -- {{shquote .remote_arg}}\"\n\n")
	b.WriteString("  shquote renders a value as one safely-quoted shell word. Use it bare — never\n")
	b.WriteString("  inside quotes of your own, since it supplies its own.\n\n")
	b.WriteString("  Every {{.name}} used in command or prompt_regex must be declared in\n")
	b.WriteString("  parameters, and that is checked at startup, not on the first start_session.\n\n")
	fmt.Fprintf(&b, "  Other keys worth knowing: login_timeout (%s), command_timeout (%s),\n",
		DefaultLoginTimeout, DefaultCommandTimeout)
	fmt.Fprintf(&b, "  max_sessions (%d), idle_timeout (0 = never reap), require_confirmation,\n", DefaultMaxSessions)
	b.WriteString("  and init_commands. Durations need a unit: 30m, not 30.\n\n")
	b.WriteString("  See config.yaml.example for the fully commented set.\n")

	return b.String()
}

// rule underlines s. Counted in runes, not bytes: these headings carry em-dashes, and a
// byte count overshoots by two columns for each one.
func rule(char, s string) string {
	return strings.Repeat(char, utf8.RuneCountInString(s))
}

func indent(s, prefix string) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		if line == "" {
			b.WriteString("\n")
			continue
		}
		b.WriteString(prefix + line + "\n")
	}
	return b.String()
}
