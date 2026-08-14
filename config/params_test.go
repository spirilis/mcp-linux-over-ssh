package config

import (
	"strings"
	"testing"
)

// parametric builds an SSHConfig with the two templates and parameters under test. The
// other fields never matter to the renderer.
func parametric(command, promptRegex string, params ...ParameterConfig) SSHConfig {
	return SSHConfig{Command: command, PromptRegex: promptRegex, Parameters: params}
}

func TestRendererAppliesTheDefaultPattern(t *testing.T) {
	r, err := NewRenderer(parametric("ssh {{.host}}", "prompt",
		ParameterConfig{Parameter: "host", Description: "the host"}))
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}

	got := r.Parameters()
	if len(got) != 1 {
		t.Fatalf("Parameters() returned %d entries, want 1", len(got))
	}
	// The effective pattern is reported, not the empty string the config carried: it is
	// what start_session's generated schema describes to the model.
	if got[0].Pattern != DefaultParameterPattern {
		t.Errorf("pattern = %q, want the default %q", got[0].Pattern, DefaultParameterPattern)
	}
	if !r.Parametric() {
		t.Error("Parametric() = false for a config that declares a parameter")
	}
}

func TestRendererRejectsBadDeclarations(t *testing.T) {
	cases := []struct {
		name string
		cfg  SSHConfig
	}{
		{
			"template references an undeclared parameter",
			parametric("ssh {{.user}}@{{.host}}", "prompt",
				ParameterConfig{Parameter: "host"}),
		},
		{
			"prompt regex references an undeclared parameter",
			parametric("ssh {{.host}}", "{{.user}}@x",
				ParameterConfig{Parameter: "host"}),
		},
		{
			"parameter name is not a template identifier",
			parametric("ssh host", "prompt", ParameterConfig{Parameter: "my-host"}),
		},
		{
			"parameter declared twice",
			parametric("ssh {{.host}}", "prompt",
				ParameterConfig{Parameter: "host"}, ParameterConfig{Parameter: "host"}),
		},
		{
			"pattern does not compile",
			parametric("ssh {{.host}}", "prompt",
				ParameterConfig{Parameter: "host", Pattern: "([unclosed"}),
		},
		{
			"command is not a valid template",
			parametric("ssh {{.host", "prompt", ParameterConfig{Parameter: "host"}),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewRenderer(tc.cfg); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

// A field used as a function argument is still a reference, which is why the check walks
// the parse tree rather than matching {{.x}} in the source.
func TestRendererSeesFieldsInsideFunctionCalls(t *testing.T) {
	if _, err := NewRenderer(parametric("sh -c {{shquote .missing}}", "prompt")); err == nil {
		t.Error("expected {{shquote .missing}} to be caught as an undeclared reference")
	}
}

func TestRenderInterpolatesBothTemplates(t *testing.T) {
	r, err := NewRenderer(parametric(
		"ssh -tt {{.user}}@{{.host}}",
		`(?m)^\[{{.user}}@[^\]]+\][$#]\s*$`,
		ParameterConfig{Parameter: "host"},
		ParameterConfig{Parameter: "user", Pattern: `^[a-z][a-z0-9_-]{0,63}$`},
	))
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}

	command, prompt, err := r.Render(map[string]string{"host": "bastion.example.com", "user": "psm-x"})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if want := "ssh -tt psm-x@bastion.example.com"; command != want {
		t.Errorf("command = %q, want %q", command, want)
	}
	if want := `(?m)^\[psm-x@[^\]]+\][$#]\s*$`; prompt != want {
		t.Errorf("prompt_regex = %q, want %q", prompt, want)
	}
}

// The defining risk of this feature: ssh.command reaches `sh -c` on the machine running
// this server, so a value carrying shell syntax is local command execution, not merely a
// bad hostname. The default pattern is what stands between the two.
func TestRenderRejectsShellMetacharactersByDefault(t *testing.T) {
	r, err := NewRenderer(parametric("ssh -tt {{.host}}", "prompt",
		ParameterConfig{Parameter: "host"}))
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}

	for _, payload := range []string{
		"a.example.com; rm -rf /",
		"a.example.com && id",
		"$(id)",
		"`id`",
		"a.example.com\nid",
		"a.example.com | tee /tmp/x",
	} {
		if _, _, err := r.Render(map[string]string{"host": payload}); err == nil {
			t.Errorf("Render accepted %q, which would have reached sh -c", payload)
		}
	}
}

// The error names the rule that was broken but never repeats the value: it is
// attacker-shaped by assumption, and the model needs the constraint, not its own input back.
func TestRenderErrorDoesNotEchoTheRejectedValue(t *testing.T) {
	r, err := NewRenderer(parametric("ssh {{.host}}", "prompt", ParameterConfig{Parameter: "host"}))
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}

	const payload = "evil.example.com; rm -rf /"
	_, _, err = r.Render(map[string]string{"host": payload})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), payload) {
		t.Errorf("error echoed the rejected value: %v", err)
	}
	if !strings.Contains(err.Error(), "host") {
		t.Errorf("error does not name the offending parameter: %v", err)
	}
}

// An operator who deliberately widens a pattern needs a way to stay safe; shquote is it.
func TestRenderShquoteNeutralisesAWidenedPattern(t *testing.T) {
	r, err := NewRenderer(parametric("sh -c {{shquote .script}}", "prompt",
		ParameterConfig{Parameter: "script", Pattern: `^.{1,256}$`}))
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}

	command, _, err := r.Render(map[string]string{"script": `echo hi; rm -rf /`})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if want := `sh -c 'echo hi; rm -rf /'`; command != want {
		t.Errorf("command = %q, want %q", command, want)
	}

	// The one character single quotes cannot carry is the single quote itself.
	command, _, err = r.Render(map[string]string{"script": `it's '; rm -rf /`})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if want := `sh -c 'it'\''s '\''; rm -rf /'`; command != want {
		t.Errorf("command = %q, want %q", command, want)
	}
}

func TestRenderRequiresEveryDeclaredParameter(t *testing.T) {
	r, err := NewRenderer(parametric("ssh {{.user}}@{{.host}}", "prompt",
		ParameterConfig{Parameter: "host"}, ParameterConfig{Parameter: "user"}))
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}

	if _, _, err := r.Render(map[string]string{"host": "bastion"}); err == nil {
		t.Error("expected an error when a declared parameter has no value")
	}
}

// Ignoring an unknown key would let a model believe it had selected a target it had not.
func TestRenderRejectsUndeclaredValues(t *testing.T) {
	r, err := NewRenderer(parametric("ssh {{.host}}", "prompt", ParameterConfig{Parameter: "host"}))
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}

	_, _, err = r.Render(map[string]string{"host": "bastion", "port": "2222"})
	if err == nil {
		t.Fatal("expected an error for an undeclared parameter")
	}
	if !strings.Contains(err.Error(), "port") {
		t.Errorf("error does not name the undeclared parameter: %v", err)
	}
}

// A config that declares nothing still renders — the templates are simply constants. This
// is what keeps a single-target deployment from having to care that any of this exists.
func TestRenderWithNoParametersIsAConstant(t *testing.T) {
	r, err := NewRenderer(parametric("ssh -tt user@bastion", `(?m)^[^\n]*[$#]\s*$`))
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}
	if r.Parametric() {
		t.Error("Parametric() = true for a config with no parameters")
	}

	command, prompt, err := r.Render(map[string]string{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if command != "ssh -tt user@bastion" {
		t.Errorf("command = %q, want it unchanged", command)
	}
	if prompt != `(?m)^[^\n]*[$#]\s*$` {
		t.Errorf("prompt_regex = %q, want it unchanged", prompt)
	}
}
