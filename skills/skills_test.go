package skills

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spirilis/generic-go-mcp/mcp"
	"github.com/spirilis/generic-go-mcp/transport"
)

func skillMD(name, desc, body string) string {
	return "---\nname: " + name + "\ndescription: " + desc + "\n---\n\n" + body + "\n"
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// configMapVolume lays files out the way the kubelet does for a ConfigMap or Secret volume:
// the real files live in a hidden timestamped directory, ..data is a link to it, and every
// visible top-level entry is a link through ..data. files maps a path inside the volume to
// its content.
func configMapVolume(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	const stamp = "..2026_11_25_18_00_00.123456789"
	top := map[string]bool{}
	for p, content := range files {
		write(t, filepath.Join(dir, stamp, p), content)
		top[strings.SplitN(p, "/", 2)[0]] = true
	}
	if err := os.Symlink(stamp, filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	for name := range top {
		if err := os.Symlink(filepath.Join("..data", name), filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
}

func load(t *testing.T, roots ...string) (*mcp.SkillRegistry, *mcp.ResourceRegistry, []string, error) {
	t.Helper()
	rr := mcp.NewResourceRegistry()
	sr := mcp.NewSkillRegistry(rr)
	names, err := Load(sr, roots)
	return sr, rr, names, err
}

func TestLoadACollectionAndASingleSkill(t *testing.T) {
	collection := t.TempDir()
	write(t, filepath.Join(collection, "alpha", "SKILL.md"), skillMD("alpha", "First.", "Do alpha."))
	write(t, filepath.Join(collection, "alpha", "references", "notes.md"), "notes")
	write(t, filepath.Join(collection, "beta", "SKILL.md"), skillMD("beta", "Second.", "Do beta."))

	// The directory is deliberately not named after the skill: a single-skill root is
	// published under its frontmatter name.
	single := filepath.Join(t.TempDir(), "some-checkout")
	write(t, filepath.Join(single, "SKILL.md"), skillMD("gamma", "Third.", "Do gamma."))
	write(t, filepath.Join(single, "scripts", "run.sh"), "#!/bin/sh\n")

	sr, _, names, err := load(t, collection, single)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(names, ","); got != "alpha,beta,gamma" {
		t.Fatalf("names = %s", got)
	}
	g, ok := sr.Get("skill://gamma/SKILL.md")
	if !ok {
		t.Fatal("gamma is not published under its frontmatter name")
	}
	if len(g.Resources) != 2 {
		t.Fatalf("gamma publishes %d files, want SKILL.md and scripts/run.sh", len(g.Resources))
	}
}

// The layout a pod actually sees. A loader that ignored links would find nothing here, and
// one that read hidden entries would publish every skill a second time under the timestamp.
func TestLoadFollowsConfigMapSymlinks(t *testing.T) {
	collection := t.TempDir()
	configMapVolume(t, collection, map[string]string{
		"rchmgt-etcd-maintenance/SKILL.md": skillMD("rchmgt-etcd-maintenance", "Etcd.", "Compact."),
		"other/SKILL.md":                   skillMD("other", "Other.", "Else."),
	})
	single := filepath.Join(t.TempDir(), "cm", "solo")
	if err := os.MkdirAll(single, 0o755); err != nil {
		t.Fatal(err)
	}
	configMapVolume(t, single, map[string]string{
		"SKILL.md": skillMD("solo", "Solo.", "Alone."),
		"extra.md": "extra",
	})

	sr, _, names, err := load(t, collection, single)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(names, ","); got != "other,rchmgt-etcd-maintenance,solo" {
		t.Fatalf("names = %s", got)
	}
	for _, s := range sr.List() {
		if strings.Contains(s.URI, "..") {
			t.Errorf("published a hidden volume directory: %s", s.URI)
		}
		for _, r := range s.Resources {
			if strings.Contains(r.URI, "..") {
				t.Errorf("published a file through a hidden volume directory: %s", r.URI)
			}
		}
	}
}

// What a host does: list, read every file the manifest names, and refuse the skill if any
// digest or size differs. Driven through the real server so the bytes checked are the bytes
// resources/read serves.
func TestServedBytesMatchTheManifest(t *testing.T) {
	root := t.TempDir()
	configMapVolume(t, root, map[string]string{
		"alpha/SKILL.md": skillMD("alpha", "First.", "Body with trailing spaces   \n\ttabs and ünïcödé"),
		"alpha/data.bin": "\x00\x01\x02binary",
	})
	sr, rr, _, err := load(t, root)
	if err != nil {
		t.Fatal(err)
	}
	srv := mcp.NewServer(mcp.NewToolRegistry(), rr, &mcp.ServerConfig{Name: "t", Version: "0", Skills: sr})

	call := func(method string, params map[string]any) json.RawMessage {
		t.Helper()
		params["_meta"] = map[string]any{
			"io.modelcontextprotocol/protocolVersion":    mcp.ProtocolVersion,
			"io.modelcontextprotocol/clientCapabilities": map[string]any{},
		}
		req, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
		w := transport.NewBufferedResponseWriter()
		srv.HandleMessage(context.Background(), req, w)
		var resp struct {
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(w.Message(), &resp); err != nil || resp.Error != nil {
			t.Fatalf("%s: %s %v", method, w.Message(), err)
		}
		return resp.Result
	}

	var list struct {
		Skills []struct {
			URI       string
			Resources []struct {
				URI    string
				Digest string
				Size   int64
			}
		}
	}
	if err := json.Unmarshal(call("skills/list", map[string]any{}), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Skills) != 1 || len(list.Skills[0].Resources) != 2 {
		t.Fatalf("skills/list = %+v", list)
	}
	for _, r := range list.Skills[0].Resources {
		var read struct {
			Contents []struct {
				Text string
				Blob []byte
			}
		}
		if err := json.Unmarshal(call("resources/read", map[string]any{"uri": r.URI}), &read); err != nil {
			t.Fatal(err)
		}
		b := []byte(read.Contents[0].Text)
		if len(read.Contents[0].Blob) > 0 {
			b = read.Contents[0].Blob
		}
		sum := sha256.Sum256(b)
		if got := "sha256:" + hex.EncodeToString(sum[:]); got != r.Digest {
			t.Errorf("%s: served bytes hash to %s, manifest says %s", r.URI, got, r.Digest)
		}
		if int64(len(b)) != r.Size {
			t.Errorf("%s: served %d bytes, manifest says %d", r.URI, len(b), r.Size)
		}
	}
}

func TestLoadRefusals(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	write(t, filepath.Join(a, "dup", "SKILL.md"), skillMD("dup", "A.", "a"))
	write(t, filepath.Join(b, "dup", "SKILL.md"), skillMD("dup", "B.", "b"))
	if _, _, _, err := load(t, a, b); err == nil || !strings.Contains(err.Error(), "already provided") {
		t.Errorf("the same skill from two roots: %v", err)
	}

	empty := t.TempDir()
	if _, _, _, err := load(t, empty); err == nil {
		t.Error("a root with no SKILL.md anywhere was accepted")
	}
	if _, _, _, err := load(t, filepath.Join(empty, "missing")); err == nil {
		t.Error("a root that does not exist was accepted")
	}

	misnamed := t.TempDir()
	write(t, filepath.Join(misnamed, "dirname", "SKILL.md"), skillMD("othername", "X.", "x"))
	if _, _, _, err := load(t, misnamed); err == nil {
		t.Error("a collection entry whose directory differs from its frontmatter name was accepted")
	}

	nofm := t.TempDir()
	write(t, filepath.Join(nofm, "SKILL.md"), "just prose\n")
	if _, _, _, err := load(t, nofm); err == nil || !strings.Contains(err.Error(), "frontmatter") {
		t.Errorf("a SKILL.md with no frontmatter: %v", err)
	}

	loop := t.TempDir()
	write(t, filepath.Join(loop, "s", "SKILL.md"), skillMD("s", "S.", "s"))
	if err := os.Symlink(loop, filepath.Join(loop, "s", "again")); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := load(t, loop); err == nil {
		t.Error("a symlink loop was followed to the end")
	}
}
