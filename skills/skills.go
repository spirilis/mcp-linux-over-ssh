// Package skills loads operator-supplied Agent Skills from disk and publishes them through
// the io.modelcontextprotocol/skills extension (SEP-2640, Beta).
//
// This server ships no skills of its own. What it reaches is somebody's estate, and the
// knowledge worth handing a model — which host runs the maintenance script, what the
// bastion's prompt looks like, which command must never be run on a Tuesday — belongs to
// whoever operates that estate. So skills arrive at startup from directories named on the
// command line, and a server started without any does not declare the extension at all.
//
// A host verifies every file it reads against the skill entry's sha256 digest and byte
// size, and compares the SKILL.md frontmatter with the entry's. generic-go-mcp derives all
// three from the bytes it is handed, so nothing here may transform a file between reading
// and registering, and the directories are read exactly once: a skill that changed
// underneath a running server would be a skill whose manifest no longer matched what its
// users approved. Restart to pick up an edit.
package skills

import (
	"bytes"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing/fstest"

	"github.com/spirilis/generic-go-mcp/mcp"
	"gopkg.in/yaml.v3"
)

// URIPrefix is the scheme skill files are published under. The specification makes skill://
// a SHOULD rather than a MUST, but a skills-aware host recognises it, and it keeps static
// documentation apart from the live session resources in resources/list.
const URIPrefix = "skill://"

const skillFile = "SKILL.md"

// Bounds on what one --skills root may hold. The library enforces the specification's own
// per-skill limits when a skill is registered; these exist so that pointing the flag at the
// wrong directory (a home directory, a checkout with a build tree) is an error at startup
// rather than a process that reads the disk into memory first.
const (
	maxRootBytes = 64 << 20
	maxRootFiles = 4096
	maxDepth     = 16
)

// Load reads every root and registers the skills found onto sr, returning their names in
// the order they are published.
//
// A root is one of two shapes, told apart by whether it directly contains a SKILL.md:
//
//   - a single skill: the directory itself is the skill. It is published under the name
//     in its frontmatter, so the directory may be called anything.
//   - a collection: each directory below it that contains a SKILL.md is a skill, published
//     under its path, whose last segment must equal the frontmatter name.
//
// Symbolic links are followed and entries whose name begins with "." are skipped. Both are
// for the same reason: a Kubernetes ConfigMap or Secret volume is nothing but symlinks —
// every visible entry points through a hidden ..data directory at a hidden timestamped one
// — so a loader that ignored links would find an empty directory, and one that read hidden
// entries would publish every file twice under a name that changes on each update.
//
// Two roots that provide the same skill are an error, not a last-one-wins: which of two
// sets of instructions a model is following must not depend on argument order.
func Load(sr *mcp.SkillRegistry, roots []string) ([]string, error) {
	merged := fstest.MapFS{}
	from := map[string]string{} // top-level path -> the root that supplied it

	for _, root := range roots {
		tree, err := readRoot(root)
		if err != nil {
			return nil, fmt.Errorf("--skills %s: %w", root, err)
		}
		for p, f := range tree {
			if prev, dup := merged[p]; dup && prev != nil {
				return nil, fmt.Errorf("--skills %s: %q is already provided by --skills %s; a skill must come from exactly one place", root, path.Dir(p), from[p])
			}
			merged[p] = f
			from[p] = root
		}
	}
	if len(merged) == 0 {
		return nil, fmt.Errorf("no skills found")
	}

	if err := sr.LoadFS(merged, URIPrefix); err != nil {
		return nil, err
	}
	var names []string
	for _, s := range sr.List() {
		if n, ok := s.Frontmatter["name"].(string); ok {
			names = append(names, n)
		}
	}
	return names, nil
}

// readRoot reads one root into memory, laid out the way the library's walker expects: every
// skill in a directory named after it.
func readRoot(root string) (fstest.MapFS, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("not a directory")
	}

	r := &reader{out: fstest.MapFS{}}
	if _, err := os.Stat(filepath.Join(root, skillFile)); err == nil {
		// A single skill. The library requires the directory to carry the skill's name,
		// and the directory here is whatever the operator happened to mount or check out,
		// so the name is read from the file and the tree is published beneath it.
		b, err := os.ReadFile(filepath.Join(root, skillFile))
		if err != nil {
			return nil, err
		}
		name, err := frontmatterName(b)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", skillFile, err)
		}
		if err := r.walk(root, name, 0); err != nil {
			return nil, err
		}
		return r.out, nil
	}

	if err := r.walk(root, "", 0); err != nil {
		return nil, err
	}
	if !r.sawSkill {
		return nil, fmt.Errorf("no %s found: expected one directly in this directory, or one in each directory below it", skillFile)
	}
	return r.out, nil
}

type reader struct {
	out      fstest.MapFS
	bytes    int64
	sawSkill bool
}

// walk copies dir into the tree under rel, following links.
func (r *reader) walk(dir, rel string, depth int) error {
	if depth > maxDepth {
		return fmt.Errorf("%s is nested more than %d directories deep (a symlink loop?)", dir, maxDepth)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		full := filepath.Join(dir, name)
		// Stat, not the entry's own type: the entry of a mounted ConfigMap is a link.
		info, err := os.Stat(full)
		if err != nil {
			return fmt.Errorf("%s: %w", full, err)
		}
		child := path.Join(rel, name)
		switch {
		case info.IsDir():
			if err := r.walk(full, child, depth+1); err != nil {
				return err
			}
		case info.Mode().IsRegular():
			if len(r.out) >= maxRootFiles {
				return fmt.Errorf("more than %d files: is this the right directory?", maxRootFiles)
			}
			if r.bytes += info.Size(); r.bytes > maxRootBytes {
				return fmt.Errorf("more than %d MiB of files: is this the right directory?", maxRootBytes>>20)
			}
			b, err := os.ReadFile(full)
			if err != nil {
				return err
			}
			r.out[child] = &fstest.MapFile{Data: b, Mode: 0o444}
			if name == skillFile {
				r.sawSkill = true
			}
		}
		// Anything else — a socket, a device — is not part of a skill.
	}
	return nil
}

// frontmatterName extracts the name from a SKILL.md's YAML frontmatter. It decides only
// where a single-skill root is published; the library parses the frontmatter again, under
// the rules a host will apply, and that parse is the one that validates it.
func frontmatterName(b []byte) (string, error) {
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	rest, ok := bytes.CutPrefix(b, []byte("---"))
	if !ok {
		return "", fmt.Errorf("does not begin with YAML frontmatter (a line of ---)")
	}
	nl := bytes.IndexByte(rest, '\n')
	if nl < 0 || len(bytes.TrimSpace(rest[:nl])) != 0 {
		return "", fmt.Errorf("does not begin with YAML frontmatter (a line of ---)")
	}
	rest = rest[nl+1:]

	var block []byte
	for _, line := range bytes.SplitAfter(rest, []byte("\n")) {
		if string(bytes.TrimRight(line, "\r\n")) == "---" {
			var fm struct {
				Name string `yaml:"name"`
			}
			if err := yaml.Unmarshal(block, &fm); err != nil {
				return "", fmt.Errorf("frontmatter is not valid YAML: %w", err)
			}
			if fm.Name == "" || strings.ContainsAny(fm.Name, `/\`) || strings.HasPrefix(fm.Name, ".") {
				return "", fmt.Errorf("frontmatter needs a name, which is what the skill is published as")
			}
			return fm.Name, nil
		}
		block = append(block, line...)
	}
	return "", fmt.Errorf("frontmatter is not closed by a line of ---")
}
