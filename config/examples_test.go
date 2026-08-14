package config

import (
	"regexp"
	"strings"
	"testing"
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
			if cfg.Lib.Server.Mode != "stdio" {
				t.Errorf("mode = %q, want stdio", cfg.Lib.Server.Mode)
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
