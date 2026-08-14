package config

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

// The examples --help-config prints are documentation that ships with the binary, so they
// are held to the same standard as the code: each one is loaded through the real parser,
// rendered, and the result checked for anything a copy-paste user would hit. A previous
// round of this project shipped a config.yaml.example whose idle_timeout: 0 did not parse,
// which is exactly the failure this prevents.
func TestExamplesAllLoad(t *testing.T) {
	// Plausible values for every parameter name the examples use.
	values := map[string]string{
		"host": "bastion.example.com",
		"user": "psm-x",
	}

	for _, ex := range Examples() {
		t.Run(ex.Name, func(t *testing.T) {
			cfg, err := Load(write(t, ex.YAML))
			if err != nil {
				t.Fatalf("example does not load: %v", err)
			}
			// The examples deliberately omit server.mode. This asserts the library's
			// default still lands on stdio, which is what makes that omission safe —
			// if it ever changed, every example would silently describe an HTTP server.
			if cfg.Lib.Server.Mode != "stdio" {
				t.Errorf("mode = %q, want stdio to be the default for an omitted server: block", cfg.Lib.Server.Mode)
			}
			// Logging to a file is shown in every example on purpose: stdout is the
			// JSON-RPC stream and clients tend to swallow stderr, so this is the only
			// place an operator can read a failed login.
			if cfg.Logging.File == "" {
				t.Error("example does not set logging.file")
			}
			if cfg.Lib.Logging.Level != "info" {
				t.Errorf("logging.level = %q, want info", cfg.Lib.Logging.Level)
			}

			params := cfg.Renderer.Parameters()
			if len(params) != ex.Params {
				t.Fatalf("declares %d parameters, want %d", len(params), ex.Params)
			}

			supplied := make(map[string]string, len(params))
			for _, p := range params {
				v, ok := values[p.Parameter]
				if !ok {
					t.Fatalf("example uses parameter %q that this test has no value for; add one", p.Parameter)
				}
				// The example's own pattern must accept the obvious value for that
				// parameter, or the example is a trap.
				if !regexp.MustCompile(p.Pattern).MatchString(v) {
					t.Errorf("pattern %s for %q rejects the ordinary value %q", p.Pattern, p.Parameter, v)
				}
				supplied[p.Parameter] = v
			}

			command, prompt, err := cfg.Renderer.Render(supplied)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			// Nothing may survive rendering — a stray {{ means a typo'd action that
			// text/template parsed as literal text.
			for _, s := range []string{command, prompt} {
				if strings.Contains(s, "{{") || strings.Contains(s, "}}") {
					t.Errorf("template delimiters survived rendering: %q", s)
				}
			}
			// The rendered prompt regex has to compile: for a parametric example that is
			// deferred past Load, so this is the only place it gets checked.
			if _, err := regexp.Compile(prompt); err != nil {
				t.Errorf("rendered prompt_regex does not compile: %v", err)
			}
		})
	}
}

// The zero-parameter example is the one people copy first, so it must genuinely need no
// arguments rather than merely declaring none.
func TestExamplesSimplestNeedsNoArguments(t *testing.T) {
	cfg, err := Load(write(t, Examples()[0].YAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Renderer.Parametric() {
		t.Fatal("the first example should not be parametric")
	}
	if _, _, err := cfg.Renderer.Render(map[string]string{}); err != nil {
		t.Errorf("Render with no values: %v", err)
	}
}

func TestHelpConfigMentionsEveryExample(t *testing.T) {
	out := HelpConfig()
	for _, ex := range Examples() {
		if !strings.Contains(out, ex.Name) {
			t.Errorf("--help-config output omits %q", ex.Name)
		}
	}
	// The security note is the reason this output exists at all rather than just a link.
	if !strings.Contains(out, DefaultParameterPattern) {
		t.Error("--help-config output does not state the default parameter pattern")
	}
	if !strings.Contains(out, "shquote") {
		t.Error("--help-config output does not mention shquote")
	}
}

// This is terminal output, so it has to fit a terminal. Measured in runes rather than
// bytes: the headings carry em-dashes, which cost three bytes and one column. Broken twice
// during development — once by underlining a heading with a byte count, once by a URL that
// overflowed — hence a test rather than an eyeball.
func TestHelpConfigFitsEightyColumns(t *testing.T) {
	for i, line := range strings.Split(HelpConfig(), "\n") {
		if n := utf8.RuneCountInString(line); n > 80 {
			t.Errorf("line %d is %d columns, want <= 80:\n%s", i+1, n, line)
		}
	}
}

// Anything this output points at has to be reachable by someone who has never seen the
// repository — a `go install`'d binary or a container image is the common case, and a bare
// "config.yaml.example" is unresolvable from either. This guards against quietly sliding
// back to a filename.
func TestHelpConfigPointsAtAResolvableReference(t *testing.T) {
	out := HelpConfig()

	if !strings.HasPrefix(ExampleConfigURL, "https://") {
		t.Errorf("ExampleConfigURL = %q, want a full URL", ExampleConfigURL)
	}
	if !strings.Contains(out, ExampleConfigURL) {
		t.Errorf("--help-config output does not contain %q", ExampleConfigURL)
	}

	// The filename may only appear as part of the URL, never standing on its own.
	const bare = "config.yaml.example"
	if strings.Count(out, bare) != strings.Count(out, ExampleConfigURL) {
		t.Errorf("--help-config mentions %q outside of the full URL; a reader with no checkout cannot resolve that", bare)
	}
}

// --help-config is read by an operator before anything else, so it sets the vocabulary. SSH
// belongs in it exactly three ways: as the name of the `ssh:` config block, as an example
// command *value*, and inside this project's own name. It must never describe what a session
// *is* — a session is a PTY and a prompt regex, and the transport that opens one is a
// configuration detail that can change without any of the rest of this changing.
//
// The tool descriptions and resource text are held to the same rule by
// tools.TestToolDefinitionsDoNotNameTheTransport and the matching test in package main;
// this is the operator-facing half of the same scrub.
func TestHelpConfigNamesSSHOnlyAsConfigurationNeverAsConcept(t *testing.T) {
	out := HelpConfig()

	// Uppercase is prose: the config keys and the example command lines are all lowercase,
	// so an "SSH" here can only be someone describing the thing rather than naming a key.
	if strings.Contains(out, "SSH") {
		t.Error("--help-config contains \"SSH\" in prose; sessions are not SSH-specific")
	}

	lower := strings.ToLower(out)
	for i := 0; ; {
		j := strings.Index(lower[i:], "ssh")
		if j < 0 {
			break
		}
		at := i + j
		i = at + 3

		rest := out[at+3:]
		switch {
		case strings.HasSuffix(out[:at], "over-"): // mcp-linux-over-ssh, and its repo URL
		case strings.HasPrefix(rest, ":"): // the ssh: block
		case strings.HasPrefix(rest, "."): // ssh.command, ssh.parameters, ...
		case strings.HasPrefix(rest, " -"): // an example command value: ssh -tt ...
		default:
			start := max(at-50, 0)
			end := min(at+50, len(out))
			t.Errorf("--help-config uses ssh as prose rather than as a key, a command value or this project's name: ...%s...", out[start:end])
		}
	}
}
