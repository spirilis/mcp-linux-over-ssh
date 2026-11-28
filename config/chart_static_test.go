package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spirilis/mcp-linux-over-ssh/authn"
)

// The chart's staticTokens become one Secret volume each, mounted by subPath, and a
// static_tokens list the binary loads, reads and starts on. With actorJwt off, nothing of it
// renders: no issuer CA volume, no actor_jwt block.
func TestChartStaticTokens(t *testing.T) {
	const inline = "0123456789abcdef0123456789abcdef01"
	docs := renderChart(t, []string{chartBaseValues}, "actorJwt.enabled=false",
		"staticTokens[0].name=ci", "staticTokens[0].existingSecret=ssh-mcp-ci",
		"staticTokens[1].name=kagent-ui", "staticTokens[1].token="+inline, "staticTokens[1].discoverOnly=true")

	secret := findDoc(docs, "Secret", "t-mcp-linux-over-ssh-static-tokens")
	if secret == nil {
		t.Fatalf("no managed Secret; rendered %v", kinds(docs))
	}
	if data, _ := secret["stringData"].(map[string]any); data["kagent-ui"] != inline || len(data) != 1 {
		t.Fatalf("managed Secret holds %v", data)
	}

	spec := findDoc(docs, "Deployment", "")["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	want := map[string][2]string{"static-token-0": {"ssh-mcp-ci", "token"}, "static-token-1": {"t-mcp-linux-over-ssh-static-tokens", "kagent-ui"}}
	for _, v := range spec["volumes"].([]any) {
		v := v.(map[string]any)
		if v["name"] == "issuer-ca" || v["name"] == "kagent-ca" {
			t.Errorf("volume %s rendered with actorJwt off", v["name"])
		}
		w, ok := want[v["name"].(string)]
		if !ok {
			continue
		}
		s := v["secret"].(map[string]any)
		item := s["items"].([]any)[0].(map[string]any)
		if s["secretName"] != w[0] || item["key"] != w[1] || item["path"] != "token" {
			t.Errorf("%s = %v", v["name"], s)
		}
		delete(want, v["name"].(string))
	}
	if len(want) != 0 {
		t.Errorf("volumes missing: %v", want)
	}
	mounts := 0
	for _, m := range spec["containers"].([]any)[0].(map[string]any)["volumeMounts"].([]any) {
		m := m.(map[string]any)
		if strings.HasPrefix(m["name"].(string), "static-token-") {
			mounts++
			if m["subPath"] != "token" || !strings.HasPrefix(m["mountPath"].(string), "/etc/mcp-linux-over-ssh/static-tokens/") {
				t.Errorf("mount %v", m)
			}
		}
	}
	if mounts != 2 {
		t.Errorf("%d static token mounts, want 2", mounts)
	}

	raw := findDoc(docs, "ConfigMap", "t-mcp-linux-over-ssh")["data"].(map[string]any)["config.yaml"].(string)
	if strings.Contains(raw, "actor_jwt") {
		t.Errorf("actor_jwt rendered with actorJwt off:\n%s", raw)
	}
	// Point each token_file at a real file, as the mounts would, and start on the result.
	dir := t.TempDir()
	n := 0
	raw = regexp.MustCompile(`token_file: (/etc/mcp-linux-over-ssh/static-tokens/\S+)`).ReplaceAllStringFunc(raw, func(m string) string {
		n++
		p := filepath.Join(dir, filepath.Base(strings.TrimPrefix(m, "token_file: ")))
		if err := os.WriteFile(p, []byte(fmt.Sprintf("token-%d-0123456789abcdef0123456789abcdef\n", n)), 0o600); err != nil {
			t.Fatal(err)
		}
		return "token_file: " + p
	})
	cfg, err := Load(write(t, raw))
	if err != nil {
		t.Fatalf("rendered config does not load: %v\n%s", err, raw)
	}
	if cfg.ActorJWTEnabled() {
		t.Error("actor JWT is on")
	}
	toks, err := cfg.ReadStaticTokens()
	if err != nil {
		t.Fatal(err)
	}
	if len(toks) != 2 || toks[0].Name != "ci" || toks[0].DiscoverOnly || toks[1].Name != "kagent-ui" || !toks[1].DiscoverOnly {
		t.Fatalf("tokens = %+v", toks)
	}
	var opts authn.Options
	for _, tk := range toks {
		opts.StaticTokens = append(opts.StaticTokens, authn.StaticToken{Name: tk.Name, Value: tk.Value, DiscoverOnly: tk.DiscoverOnly})
	}
	if _, err := authn.New(opts); err != nil {
		t.Fatalf("rendered static tokens do not start: %v", err)
	}

	// Beside actor JWT (the default), both render.
	both := renderChart(t, []string{chartBaseValues}, "staticTokens[0].name=ci", "staticTokens[0].existingSecret=ssh-mcp-ci")
	raw = findDoc(both, "ConfigMap", "t-mcp-linux-over-ssh")["data"].(map[string]any)["config.yaml"].(string)
	if !strings.Contains(raw, "actor_jwt:") || !strings.Contains(raw, "static_tokens:") {
		t.Errorf("config lacks one of the two credentials:\n%s", raw)
	}
	if findDoc(both, "Secret", "t-mcp-linux-over-ssh-static-tokens") != nil {
		t.Error("a managed Secret rendered with no inline token")
	}

	// Values with no actorJwt.enabled at all, as `helm upgrade --reuse-values` from chart
	// 0.1.0 passes them, still admit agents: only an explicit false turns it off.
	reused := renderChart(t, []string{chartBaseValues}, "actorJwt.enabled=null")
	raw = findDoc(reused, "ConfigMap", "t-mcp-linux-over-ssh")["data"].(map[string]any)["config.yaml"].(string)
	if !strings.Contains(raw, "actor_jwt:") {
		t.Errorf("actorJwt.enabled absent turned actor JWT off:\n%s", raw)
	}
}

func TestChartRefusesBadStaticTokens(t *testing.T) {
	const ok = "0123456789abcdef0123456789abcdef01"
	for name, tc := range map[string]struct {
		sets []string
		want string
	}{
		"no authentication": {[]string{"actorJwt.enabled=false"}, "no authentication"},
		"short":             {[]string{"staticTokens[0].name=ci", "staticTokens[0].token=short"}, "at least 32"},
		"JWT-shaped":        {[]string{"staticTokens[0].name=ci", "staticTokens[0].token=aaaaaaaaaaaaaa.bbbbbbbbbbbbbbbbbb.ccccccccccc"}, "looks like a JWT"},
		"both":              {[]string{"staticTokens[0].name=ci", "staticTokens[0].token=" + ok, "staticTokens[0].existingSecret=x"}, "not both"},
		"neither":           {[]string{"staticTokens[0].name=ci"}, "set existingSecret"},
		"twice":             {[]string{"staticTokens[0].name=ci", "staticTokens[0].existingSecret=x", "staticTokens[1].name=ci", "staticTokens[1].existingSecret=y"}, "listed twice"},
		"bad name":          {[]string{"staticTokens[0].name=CI", "staticTokens[0].existingSecret=x"}, "must be lowercase"},
		"misspelt key":      {[]string{"staticTokens[0].name=ui", "staticTokens[0].existingSecret=x", "staticTokens[0].discover_only=true"}, "unknown key"},
		"not a map":         {[]string{"staticTokens={ci}"}, "must be a map"},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := helmTemplate(t, []string{chartBaseValues}, tc.sets...)
			if err == nil || !strings.Contains(out, tc.want) {
				t.Fatalf("render: %v, want a failure naming %q\n%s", err, tc.want, out)
			}
		})
	}
}
