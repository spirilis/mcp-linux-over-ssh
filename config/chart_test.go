package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spirilis/generic-go-mcp/mcp"
	"gopkg.in/yaml.v3"
)

const chartDir = "../charts/mcp-linux-over-ssh"

// chartBaseValues is the least a release needs: the two keys of the ssh: block that have no
// default. It is a values file rather than --set flags because a command template is full
// of the braces and commas --set gives its own meaning to.
const chartBaseValues = `ssh:
  command: "ssh -tt psmuser@{{.host}}"
  prompt_regex: '(?m)^[^\n]*[$#]\s*$'
  parameters:
    - parameter: host
      description: "Hostname of the bastion to connect through"
`

// The example from the request that introduced skills.inject, verbatim.
const chartInjectValues = `skills:
  inject:
  - name: rchmgt-etcd-maintenance
    frontmatter: Use these instructions when accessing the wmrchmgt* systems; there is custom knowledge found in here you will need.
    skill: |-
      In order to perform an etcd compaction on the wmrchmgt* systems, you need to do this over Salt.
      Run on the wmsaltmaster1 system a command like the following:
      ./etcd-maintenance.sh <hostname>
      e.g. ./etcd-maintenance.sh wmrchmgt1-dev
`

// helmTemplate renders the chart with the given values documents and --set flags, returning
// helm's combined output and its error. The test is skipped where helm is not installed.
func helmTemplate(t *testing.T, values []string, sets ...string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed")
	}
	args := []string{"template", "t", chartDir, "-n", "mcp"}
	for i, v := range values {
		f := filepath.Join(t.TempDir(), "values-"+string(rune('a'+i))+".yaml")
		if err := os.WriteFile(f, []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
		args = append(args, "-f", f)
	}
	for _, s := range sets {
		args = append(args, "--set", s)
	}
	out, err := exec.Command("helm", args...).CombinedOutput()
	return string(out), err
}

// renderChart is helmTemplate for a render that must succeed, decoded into documents.
func renderChart(t *testing.T, values []string, sets ...string) []map[string]any {
	t.Helper()
	out, err := helmTemplate(t, values, sets...)
	if err != nil {
		t.Fatalf("helm template %v: %v\n%s", sets, err, out)
	}
	var docs []map[string]any
	dec := yaml.NewDecoder(strings.NewReader(out))
	for {
		var d map[string]any
		if err := dec.Decode(&d); err != nil {
			break
		}
		if d != nil {
			docs = append(docs, d)
		}
	}
	return docs
}

func findDoc(docs []map[string]any, kind, name string) map[string]any {
	for _, d := range docs {
		if d["kind"] != kind {
			continue
		}
		if meta, _ := d["metadata"].(map[string]any); name == "" || meta["name"] == name {
			return d
		}
	}
	return nil
}

func kinds(docs []map[string]any) []string {
	var out []string
	for _, d := range docs {
		out = append(out, d["kind"].(string))
	}
	return out
}

// containerArgs returns the one container's args from a rendered Deployment.
func containerArgs(t *testing.T, docs []map[string]any) []string {
	t.Helper()
	dep := findDoc(docs, "Deployment", "")
	if dep == nil {
		t.Fatal("no Deployment rendered")
	}
	b, err := yaml.Marshal(dep)
	if err != nil {
		t.Fatal(err)
	}
	var shaped struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Args []string `yaml:"args"`
					} `yaml:"containers"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(b, &shaped); err != nil {
		t.Fatal(err)
	}
	if len(shaped.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("want one container, got %d", len(shaped.Spec.Template.Spec.Containers))
	}
	return shaped.Spec.Template.Spec.Containers[0].Args
}

// TestChartConfigLoads feeds the config file the chart renders to this server's own loader,
// so a chart that writes a key the binary rejects (or misspells one it needs) fails here
// rather than in a crash-looping pod.
func TestChartConfigLoads(t *testing.T) {
	gateway := []string{"host=mcp.example.com", "gateway.enabled=true", "gateway.httpRedirect.enabled=true",
		"certManager.enabled=true", "gateway.referenceGrant.enabled=true"}
	cases := []struct {
		name      string
		sets      []string
		wantKinds []string
		noKinds   []string
	}{
		{name: "defaults", noKinds: []string{"HTTPRoute", "Certificate", "BackendTLSPolicy", "ServiceAccount"}},
		{name: "tls from an existing secret", sets: []string{"tls.enabled=true", "tls.existingSecret=serving"},
			noKinds: []string{"Certificate"}},
		{name: "tls from cert-manager behind a gateway",
			sets: append([]string{"tls.enabled=true", "tls.certManager.enabled=true", "tls.certManager.issuerRef.name=cluster-ca",
				"gateway.backendTLS.enabled=true", "gateway.backendTLS.wellKnownCACertificates=System"}, gateway...),
			wantKinds: []string{"HTTPRoute", "Certificate", "BackendTLSPolicy", "ReferenceGrant"}},
		{name: "tls from a volume", sets: []string{"tls.enabled=true", "tls.volume.csi.driver=csi.cert-manager.io",
			"tls.certFile=cert.pem", "tls.keyFile=key.pem"}, noKinds: []string{"Certificate"}},
		{name: "plain gateway", sets: gateway, wantKinds: []string{"HTTPRoute", "Certificate", "ReferenceGrant"},
			noKinds: []string{"BackendTLSPolicy"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			docs := renderChart(t, []string{chartBaseValues}, tc.sets...)
			got := kinds(docs)
			for _, k := range tc.wantKinds {
				if !slices.Contains(got, k) {
					t.Errorf("no %s rendered; got %v", k, got)
				}
			}
			for _, k := range tc.noKinds {
				if slices.Contains(got, k) {
					t.Errorf("%s rendered and should not be; got %v", k, got)
				}
			}

			cm := findDoc(docs, "ConfigMap", "t-mcp-linux-over-ssh")
			if cm == nil {
				t.Fatal("no config ConfigMap rendered")
			}
			raw, _ := cm["data"].(map[string]any)["config.yaml"].(string)
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("the rendered config does not load: %v\n%s", err, raw)
			}
			if cfg.Lib.Server.Mode != "http" {
				t.Errorf("server.mode = %q, want http", cfg.Lib.Server.Mode)
			}
			if cfg.SSH.IdleTimeout != 30*time.Minute {
				t.Errorf("ssh.idle_timeout = %v, want the chart default of 30m", cfg.SSH.IdleTimeout)
			}
			if n := len(cfg.Renderer.Parameters()); n != 1 {
				t.Errorf("the rendered config declares %d parameters, want 1", n)
			}
			// The command template must survive the trip through values and toYaml intact.
			if cfg.SSH.Command != "ssh -tt psmuser@{{.host}}" {
				t.Errorf("ssh.command = %q", cfg.SSH.Command)
			}
			if cfg.SSH.PromptRegex != `(?m)^[^\n]*[$#]\s*$` {
				t.Errorf("ssh.prompt_regex = %q", cfg.SSH.PromptRegex)
			}
			if args := containerArgs(t, docs); slices.Contains(args, "--skills") {
				t.Errorf("--skills passed with no skills configured: %v", args)
			}
		})
	}
}

// TestChartInjectedSkillIsARealSkill renders the skills.inject example and hands the
// generated SKILL.md to the registry that will serve it. Rendering text that merely looks
// like frontmatter proves little; this proves the parser on the other end accepts it.
func TestChartInjectedSkillIsARealSkill(t *testing.T) {
	const name = "rchmgt-etcd-maintenance"
	extra := `skills:
  inject:
  - name: with-a-map
    frontmatter:
      description: "Tricky: a colon, a # and a 'quote'"
      license: MIT
      name: ignored-in-favour-of-the-entry
    skill: |
      Body with trailing newline.
`
	// Two documents setting skills.inject would replace, not append, so the map-form entry
	// is rendered separately.
	for _, tc := range []struct {
		values, skill, wantPrefix, wantDescription string
	}{
		{chartInjectValues, name, "---\nname: " + name + "\ndescription: ",
			"Use these instructions when accessing the wmrchmgt* systems; there is custom knowledge found in here you will need."},
		{extra, "with-a-map", "---\nname: with-a-map\n", "Tricky: a colon, a # and a 'quote'"},
	} {
		t.Run(tc.skill, func(t *testing.T) {
			docs := renderChart(t, []string{chartBaseValues, tc.values})
			cm := findDoc(docs, "ConfigMap", "t-mcp-linux-over-ssh-skills")
			if cm == nil {
				t.Fatal("no skills ConfigMap rendered")
			}
			md, _ := cm["data"].(map[string]any)[tc.skill].(string)
			if !strings.HasPrefix(md, tc.wantPrefix) {
				t.Fatalf("SKILL.md does not start with %q:\n%s", tc.wantPrefix, md)
			}
			if !strings.HasSuffix(md, "\n") || strings.HasSuffix(md, "\n\n") {
				t.Errorf("SKILL.md should end in exactly one newline: %q", md[max(0, len(md)-20):])
			}

			args := containerArgs(t, docs)
			const root = "/etc/mcp-linux-over-ssh/skills/inject"
			if i := slices.Index(args, "--skills"); i < 0 || i+1 >= len(args) || args[i+1] != root {
				t.Errorf("args do not pass --skills %s: %v", root, args)
			}
			// The volume has to put the key where the loader looks: <name>/SKILL.md.
			depYAML, _ := yaml.Marshal(findDoc(docs, "Deployment", ""))
			if want := "path: " + tc.skill + "/SKILL.md"; !strings.Contains(string(depYAML), want) {
				t.Errorf("Deployment does not project the skill to %q", want)
			}
			if !strings.Contains(string(depYAML), "checksum/skills") {
				t.Error("Deployment has no checksum/skills annotation, so an edited skill would not roll the pod")
			}

			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, tc.skill), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, tc.skill, "SKILL.md"), []byte(md), 0o644); err != nil {
				t.Fatal(err)
			}
			sr := mcp.NewSkillRegistry(mcp.NewResourceRegistry())
			if err := sr.LoadFS(os.DirFS(dir), "skill://"); err != nil {
				t.Fatalf("the generated SKILL.md is not a loadable skill: %v\n%s", err, md)
			}
			skills := sr.List()
			if len(skills) != 1 {
				t.Fatalf("loaded %d skills, want 1", len(skills))
			}
			if got := skills[0].URI; got != "skill://"+tc.skill+"/SKILL.md" {
				t.Errorf("skill URI = %q", got)
			}
			if got := skills[0].Frontmatter["name"]; got != tc.skill {
				t.Errorf("frontmatter name = %v, want %q", got, tc.skill)
			}
			if got := skills[0].Frontmatter["description"]; got != tc.wantDescription {
				t.Errorf("frontmatter description = %q, want %q", got, tc.wantDescription)
			}
			if strings.HasPrefix(tc.skill, "rchmgt") && !strings.Contains(md, "./etcd-maintenance.sh <hostname>\n") {
				t.Errorf("the skill body was altered on the way through:\n%s", md)
			}
		})
	}
}

// TestChartConfigMapSkills checks an existing ConfigMap is mounted as one skill directory and
// handed to the binary as its own root.
func TestChartConfigMapSkills(t *testing.T) {
	docs := renderChart(t, []string{chartBaseValues, chartInjectValues},
		"skills.configMaps[0].name=my-skill",
		"skills.configMaps[1].name=team-runbooks-cm", "skills.configMaps[1].skill=salt-runbook")
	args := containerArgs(t, docs)
	var roots []string
	for i, a := range args {
		if a == "--skills" && i+1 < len(args) {
			roots = append(roots, args[i+1])
		}
	}
	want := []string{
		"/etc/mcp-linux-over-ssh/skills/inject",
		"/etc/mcp-linux-over-ssh/skills/cm/my-skill",
		"/etc/mcp-linux-over-ssh/skills/cm/salt-runbook",
	}
	if !slices.Equal(roots, want) {
		t.Errorf("--skills roots = %v, want %v", roots, want)
	}
	depYAML, _ := yaml.Marshal(findDoc(docs, "Deployment", ""))
	for _, s := range []string{"name: team-runbooks-cm", "mountPath: /etc/mcp-linux-over-ssh/skills/cm/salt-runbook"} {
		if !strings.Contains(string(depYAML), s) {
			t.Errorf("Deployment lacks %q", s)
		}
	}
}

// TestChartRefusesBadValues pins the combinations that must fail the render, with the
// message a person would need to fix them.
func TestChartRefusesBadValues(t *testing.T) {
	dup := `skills:
  inject:
  - {name: twice, frontmatter: one, skill: a}
  - {name: twice, frontmatter: two, skill: b}
`
	cases := []struct {
		name   string
		values []string
		sets   []string
		want   string
	}{
		{"no ssh.command", nil, []string{"ssh.prompt_regex=x"}, "ssh.command is required"},
		{"no ssh.prompt_regex", nil, []string{"ssh.command=bash"}, "ssh.prompt_regex is required"},
		{"idle timeout switched off", []string{chartBaseValues}, []string{"ssh.idle_timeout=0s"}, "ssh.idle_timeout"},
		{"bad skill name", []string{chartBaseValues},
			[]string{"skills.inject[0].name=Not_A_Name", "skills.inject[0].frontmatter=d", "skills.inject[0].skill=b"},
			`name "Not_A_Name"`},
		{"skill without a description", []string{chartBaseValues},
			[]string{"skills.inject[0].name=ok", "skills.inject[0].skill=b"}, "frontmatter is required"},
		{"skill without a body", []string{chartBaseValues},
			[]string{"skills.inject[0].name=ok", "skills.inject[0].frontmatter=d"}, "skill is required"},
		{"frontmatter map without a description", []string{chartBaseValues},
			[]string{"skills.inject[0].name=ok", "skills.inject[0].frontmatter.license=MIT", "skills.inject[0].skill=b"},
			"must have a description"},
		{"duplicate injected skill", []string{chartBaseValues, dup}, nil, `skill "twice" is defined more than once`},
		{"configmap skill duplicating an injected one", []string{chartBaseValues, chartInjectValues},
			[]string{"skills.configMaps[0].name=rchmgt-etcd-maintenance"}, "defined more than once"},
		{"two tls sources", []string{chartBaseValues},
			[]string{"tls.enabled=true", "tls.existingSecret=a", "tls.volume.csi.driver=x"}, "exactly one of"},
		{"tls with no source", []string{chartBaseValues}, []string{"tls.enabled=true"}, "tls.enabled needs a certificate"},
		{"gateway without host", []string{chartBaseValues}, []string{"gateway.enabled=true"}, "host must be set"},
		{"gateway to a tls pod without backendTLS", []string{chartBaseValues},
			[]string{"host=h.example.com", "gateway.enabled=true", "tls.enabled=true", "tls.existingSecret=a"},
			"gateway.backendTLS"},
		{"malformed allowed agent", []string{chartBaseValues}, []string{"actorJwt.allowedAgents={kagent}"}, "must be namespace/name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := helmTemplate(t, tc.values, tc.sets...)
			if err == nil {
				t.Fatal("the render succeeded and should have failed")
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("failure does not mention %q:\n%s", tc.want, out)
			}
		})
	}
}

// TestChartShape pins the properties of the workload that are decisions rather than defaults.
func TestChartShape(t *testing.T) {
	docs := renderChart(t, []string{chartBaseValues}, "sshCredentials.existingSecret=bastion-ssh")
	depYAML, _ := yaml.Marshal(findDoc(docs, "Deployment", ""))
	dep := string(depYAML)
	for _, want := range []string{
		"replicas: 1",
		"type: Recreate",
		"automountServiceAccountToken: false",
		"readOnlyRootFilesystem: true",
		"fsGroup: 1000",
		"runAsUser: 1000",
		"secretName: bastion-ssh",
		"mountPath: /home/mcp/.ssh",
		"path: /readyz",
		"path: /healthz",
		"checksum/config",
	} {
		if !strings.Contains(dep, want) {
			t.Errorf("Deployment lacks %q", want)
		}
	}
	// 0440 written in the template is octal; decoded it is 288.
	if !strings.Contains(dep, "defaultMode: 288") {
		t.Errorf("credentials are not mounted mode 0440:\n%s", dep)
	}

	raw, err := os.ReadFile(filepath.Join(chartDir, "Chart.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var chart struct {
		AppVersion string `yaml:"appVersion"`
	}
	if err := yaml.Unmarshal(raw, &chart); err != nil {
		t.Fatal(err)
	}
	if chart.AppVersion == "" {
		t.Error("Chart.yaml has no appVersion, so the image tag would be empty")
	}
}

// TestChartNamesNoOtherAuthentication keeps the chart honest about what the binary does: it
// authenticates by static token and actor JWT and nothing else, so no template or value may
// offer an anonymous mode or an OAuth one.
func TestChartNamesNoOtherAuthentication(t *testing.T) {
	err := filepath.WalkDir(chartDir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		low := strings.ToLower(string(b))
		for _, banned := range []string{"oauth", "allowanonymous", "github"} {
			if strings.Contains(low, banned) {
				t.Errorf("%s mentions %q", p, banned)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
