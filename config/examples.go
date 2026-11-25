package config

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// ExampleConfigURL is the fully commented reference configuration.
//
// Spelled out as a URL rather than as the bare filename "config.yaml.example", because the
// people most likely to run --help-config are the ones with no repository checkout: someone
// who ran `go install`, or who is pointing an MCP client at a container image. A filename
// they cannot locate is not a reference.
const ExampleConfigURL = "https://github.com/spirilis/mcp-linux-over-ssh/blob/main/config.yaml.example"

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
			YAML: `logging:
  level: info
  file: mcp-linux-over-ssh.log
ssh:
  command: "ssh -tt psmuser@bastion.example.com"
  prompt_regex: '(?m)^[^\n]*[$#]\s*$'
`,
		},
		{
			Name:        "One parameter — the caller picks the host",
			Description: "start_session takes {\"host\": \"...\"}.",
			Params:      1,
			YAML: `logging:
  level: info
  file: mcp-linux-over-ssh.log
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
			YAML: `logging:
  level: info
  file: mcp-linux-over-ssh.log
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
	b.WriteString("server.mode is omitted below: it defaults to stdio, where the MCP client that\n")
	b.WriteString("launched this process is the only caller. The other mode, http, is described\n")
	b.WriteString("in the notes.\n\n")

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
	b.WriteString("  logging.file earns its place in every example above. stdout carries the\n")
	b.WriteString("  JSON-RPC stream and nothing else may touch it, and an MCP client usually\n")
	b.WriteString("  swallows stderr, so without a file there is nowhere to read what went wrong\n")
	b.WriteString("  when a session will not log in. A relative path resolves against the\n")
	b.WriteString("  server's working directory, which the client chooses; use an absolute path\n")
	b.WriteString("  if you are not sure what that will be.\n\n")
	b.WriteString("  ssh.disable_flag_file is an administrative kill switch, and off unless you\n")
	b.WriteString("  name a path. While a file exists there, start_session and run_bash refuse\n")
	b.WriteString("  with an error saying so, and sessions already open are terminated as soon\n")
	b.WriteString("  as they stop running whatever they are running. Removing the file resumes\n")
	b.WriteString("  service with no restart. Only the file's existence matters — its contents\n")
	b.WriteString("  are never read — so touch and rm are the whole interface.\n\n")
	b.WriteString("  server.mode: http serves kagent agents over the network instead, and is\n")
	b.WriteString("  never anonymous: it requires an actor_jwt: block (audience, ca_file,\n")
	b.WriteString("  allowed_agents) and refuses to start without one. It also requires\n")
	b.WriteString("  ssh.idle_timeout, since no client lifetime ends an abandoned session there.\n")
	b.WriteString("  An optional tls: block (cert_file, key_file) makes the listener serve HTTPS\n")
	b.WriteString("  itself. Each caller sees and uses only the sessions it opened.\n\n")
	b.WriteString("  --skills <dir> (repeatable, any mode) serves Agent Skills to clients that\n")
	b.WriteString("  load them: a directory holding a SKILL.md, or one whose subdirectories each\n")
	b.WriteString("  hold one. It is read once, at startup.\n\n")
	fmt.Fprintf(&b, "  Other keys worth knowing: login_timeout (%s), command_timeout (%s),\n",
		DefaultLoginTimeout, DefaultCommandTimeout)
	fmt.Fprintf(&b, "  max_sessions (%d), idle_timeout (0 = never reap), require_confirmation,\n", DefaultMaxSessions)
	b.WriteString("  and init_commands. Durations need a unit: 30m, not 30.\n\n")
	b.WriteString("  The fully commented reference configuration, documenting every option and\n")
	b.WriteString("  its default, is at:\n\n")
	// Indented less than the code sample above so the line stays under the wrap column: a
	// URL broken across two lines cannot be clicked or copied, which defeats the point of
	// printing it.
	b.WriteString("  " + ExampleConfigURL + "\n")

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
