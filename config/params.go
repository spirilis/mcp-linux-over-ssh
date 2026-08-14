package config

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"text/template"
	"text/template/parse"
)

// ParameterConfig declares one caller-supplied value that ssh.command and ssh.prompt_regex
// may interpolate.
type ParameterConfig struct {
	// Parameter is the template variable name, referenced as {{.Parameter}}. It must be a
	// valid Go template field identifier.
	Parameter string `yaml:"parameter"`

	// Description is surfaced to the MCP client verbatim, as the description of the
	// matching property on start_session's generated input schema. It is the only thing
	// telling the model what to put here, so it is worth writing well.
	Description string `yaml:"description"`

	// Pattern is a Go (RE2) regex every supplied value must match in full. Empty means
	// DefaultParameterPattern. See NewRenderer for why this is not optional in spirit.
	Pattern string `yaml:"pattern"`
}

// DefaultParameterPattern is what an omitted pattern: means. It admits hostnames,
// usernames, IPs, paths and ports, and nothing that carries meaning to a shell.
//
// The default is deliberately restrictive rather than permissive. ssh.command is rendered
// into a string handed to `sh -c` on *this* host, so a value carrying shell metacharacters
// is local command execution on the MCP server — a materially worse outcome than run_bash's
// deliberate execution on the far end, and the values now come from an MCP client rather
// than from the operator who wrote the config. An operator who genuinely needs looser values
// widens the pattern explicitly, and reaches for shquote in the template to go with it.
const DefaultParameterPattern = `^[A-Za-z0-9._@:/+-]{1,256}$`

// Template field identifiers are what {{.Foo}} can name.
var paramNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// templateFuncs are available to both templates.
//
// shquote is the escape hatch that makes a widened pattern safe: it wraps a value in single
// quotes so a shell reads it as one literal word regardless of what it contains. Use it
// bare — `{{shquote .path}}`, never `"{{shquote .path}}"` — since it supplies its own
// quoting.
var templateFuncs = template.FuncMap{"shquote": shquote}

// shquote renders v as a single POSIX shell word. Single quotes suppress every form of
// expansion, so the only character needing care is the quote itself: close, escape, reopen.
func shquote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

// Renderer turns a set of caller-supplied parameter values into the concrete command line
// and prompt regex for one session. It is built once at startup and used concurrently by
// every start_session call, so it holds nothing mutable.
type Renderer struct {
	params   []ParameterConfig
	patterns map[string]*regexp.Regexp
	command  *template.Template
	prompt   *template.Template
}

// NewRenderer validates the parameter declarations and the two templates against each
// other, so a config that could only fail at start_session time fails at startup instead.
func NewRenderer(cfg SSHConfig) (*Renderer, error) {
	r := &Renderer{patterns: make(map[string]*regexp.Regexp, len(cfg.Parameters))}

	for _, p := range cfg.Parameters {
		if !paramNameRe.MatchString(p.Parameter) {
			return nil, fmt.Errorf("ssh.parameters: %q is not a valid parameter name; it must match %s so it can be referenced as {{.%s}}",
				p.Parameter, paramNameRe, p.Parameter)
		}
		if _, dup := r.patterns[p.Parameter]; dup {
			return nil, fmt.Errorf("ssh.parameters: %q is declared more than once", p.Parameter)
		}
		if p.Pattern == "" {
			p.Pattern = DefaultParameterPattern
		}
		re, err := regexp.Compile(p.Pattern)
		if err != nil {
			return nil, fmt.Errorf("ssh.parameters: pattern for %q does not compile: %w", p.Parameter, err)
		}
		r.patterns[p.Parameter] = re
		r.params = append(r.params, p)
	}

	var err error
	if r.command, err = r.parse("ssh.command", cfg.Command); err != nil {
		return nil, err
	}
	if r.prompt, err = r.parse("ssh.prompt_regex", cfg.PromptRegex); err != nil {
		return nil, err
	}
	return r, nil
}

// parse compiles one template and checks that every field it references was declared. The
// check is the difference between a typo failing at startup and failing on the first
// start_session call against a bastion nobody can reach twice.
func (r *Renderer) parse(name, text string) (*template.Template, error) {
	t, err := template.New(name).Funcs(templateFuncs).Option("missingkey=error").Parse(text)
	if err != nil {
		return nil, fmt.Errorf("%s is not a valid template: %w", name, err)
	}
	for _, ref := range fieldRefs(t.Tree.Root) {
		if _, ok := r.patterns[ref]; !ok {
			return nil, fmt.Errorf("%s references {{.%s}}, which is not declared in ssh.parameters (declared: %s)",
				name, ref, r.nameList())
		}
	}
	return t, nil
}

// Parameters returns the declared parameters, each with its effective pattern (the default
// substituted where the config omitted one).
func (r *Renderer) Parameters() []ParameterConfig {
	out := make([]ParameterConfig, len(r.params))
	copy(out, r.params)
	return out
}

// Parametric reports whether any parameter is declared. A config with none still renders —
// the templates are simply constant — which is what keeps a single-target deployment from
// having to care that any of this exists.
func (r *Renderer) Parametric() bool { return len(r.params) > 0 }

// Render validates values and produces one session's command line and prompt regex.
//
// Every declared parameter must be present and match its pattern, and no undeclared key is
// accepted: silently ignoring an unknown key would let a model believe it had selected a
// target it had not.
func (r *Renderer) Render(values map[string]string) (command, promptRegex string, err error) {
	if err := r.validate(values); err != nil {
		return "", "", err
	}
	if command, err = execute(r.command, values); err != nil {
		return "", "", err
	}
	if promptRegex, err = execute(r.prompt, values); err != nil {
		return "", "", err
	}
	return command, promptRegex, nil
}

func (r *Renderer) validate(values map[string]string) error {
	for _, p := range r.params {
		v, ok := values[p.Parameter]
		if !ok {
			return fmt.Errorf("missing required parameter %q", p.Parameter)
		}
		// The rejected value is deliberately not echoed back: it is attacker-shaped by
		// assumption, and the model needs the rule it broke, not its own input repeated.
		if !r.patterns[p.Parameter].MatchString(v) {
			return fmt.Errorf("the value given for parameter %q is not allowed: it must match %s",
				p.Parameter, p.Pattern)
		}
	}
	for k := range values {
		if _, ok := r.patterns[k]; !ok {
			return fmt.Errorf("unknown parameter %q (declared: %s)", k, r.nameList())
		}
	}
	return nil
}

func execute(t *template.Template, values map[string]string) (string, error) {
	var b strings.Builder
	if err := t.Execute(&b, values); err != nil {
		return "", fmt.Errorf("could not render %s: %w", t.Name(), err)
	}
	return b.String(), nil
}

func (r *Renderer) nameList() string {
	if len(r.params) == 0 {
		return "none"
	}
	names := make([]string, 0, len(r.params))
	for _, p := range r.params {
		names = append(names, p.Parameter)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// fieldRefs collects the first identifier of every field reference in a parsed template, so
// {{.host}} and {{shquote .host}} both yield "host".
//
// It walks the parse tree rather than pattern-matching the source, which is what makes it
// right about {{if .x}} and about a field appearing as a function argument. A node type it
// does not know is simply not descended into; the Option("missingkey=error") set in parse is
// the backstop for anything that slips through, turning it into a render-time error instead
// of a silently empty interpolation.
func fieldRefs(root parse.Node) []string {
	var out []string
	var walk func(parse.Node)
	walk = func(n parse.Node) {
		switch x := n.(type) {
		case *parse.ListNode:
			if x == nil {
				return
			}
			for _, c := range x.Nodes {
				walk(c)
			}
		case *parse.ActionNode:
			walk(x.Pipe)
		case *parse.PipeNode:
			if x == nil {
				return
			}
			for _, c := range x.Cmds {
				walk(c)
			}
		case *parse.CommandNode:
			for _, a := range x.Args {
				walk(a)
			}
		case *parse.FieldNode:
			if len(x.Ident) > 0 {
				out = append(out, x.Ident[0])
			}
		case *parse.IfNode:
			walk(x.Pipe)
			walk(x.List)
			walk(x.ElseList)
		case *parse.RangeNode:
			walk(x.Pipe)
			walk(x.List)
			walk(x.ElseList)
		case *parse.WithNode:
			walk(x.Pipe)
			walk(x.List)
			walk(x.ElseList)
		}
	}
	walk(root)
	return out
}
