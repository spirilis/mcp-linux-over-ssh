package main

import (
	"context"
	"io"
	"os"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/spirilis/generic-go-mcp/mcp"

	"github.com/spirilis/mcp-linux-over-ssh/session"
	"github.com/spirilis/mcp-linux-over-ssh/tools"
)

// The counterpart to tools.TestToolDefinitionsDoNotNameTheTransport, for the protocol text
// main.go owns: the server instructions and every resource's name and description. See that
// test for why the transport does not belong in any of it.
//
// A session is a PTY and a prompt regex. SSH is how one gets opened today, and the whole
// ssh: config block is named for it — but a client is told about sessions, not about SSH,
// so replacing the transport stays a configuration change rather than a protocol break.

func TestInstructionsDoNotNameTheTransport(t *testing.T) {
	// Every combination, since each flag appends its own paragraph.
	for _, confirm := range []bool{false, true} {
		for _, parametric := range []bool{false, true} {
			for _, gated := range []bool{false, true} {
				for _, shared := range []bool{false, true} {
					s := instructions(confirm, parametric, gated, shared)
					if strings.Contains(strings.ToLower(s), "ssh") {
						t.Errorf("instructions(confirm=%v, parametric=%v, gated=%v, shared=%v) names the transport:\n%s",
							confirm, parametric, gated, shared, s)
					}
				}
			}
		}
	}
}

func TestResourceDescriptionsDoNotNameTheTransport(t *testing.T) {
	mgr := session.NewManager(4, 0)
	t.Cleanup(mgr.CloseAll)

	reg := mcp.NewResourceRegistry()
	registerSessionsIndex(reg, mgr, tools.SingleCaller)
	// A nil session is safe here: Register only stores the read callback, and nothing in
	// this test invokes it. The descriptions are built eagerly, which is all that is under
	// test.
	registerSessionResources(reg, "s-abcd1234", nil, func(context.Context) bool { return true })

	listed := reg.List()
	if len(listed) != 3 {
		t.Fatalf("registered %d resources, want 3 (index, status, transcript)", len(listed))
	}
	for _, res := range listed {
		for label, s := range map[string]string{"uri": res.URI, "name": res.Name, "description": res.Description} {
			if strings.Contains(strings.ToLower(s), "ssh") {
				t.Errorf("resource %s %s names the transport: %q", res.URI, label, s)
			}
		}
	}
}

// --version exists to answer one question: is the binary I installed the version it says it
// is? That has two independent answers — the constant compiled into this file, which is what
// MCP clients are told, and the module version the Go toolchain resolved, which is what
// `go install ...@latest` actually fetched. They are maintained by different mechanisms and
// have drifted in this repository before.
//
// The drift case is the one that cannot be reproduced from a working tree, which is why
// renderVersion takes its BuildInfo rather than reading it.

func TestRenderVersionAlwaysIdentifiesTheBinary(t *testing.T) {
	for _, bi := range []*debug.BuildInfo{
		nil,
		{Main: debug.Module{Path: "example.com/m", Version: "(devel)"}, GoVersion: "go1.26.2"},
		{Main: debug.Module{Path: "example.com/m", Version: "v" + serverVersion}, GoVersion: "go1.26.2"},
	} {
		ok := bi != nil
		out := renderVersion(bi, ok)
		if !strings.Contains(out, serverName) {
			t.Errorf("output does not name the binary:\n%s", out)
		}
		if !strings.Contains(out, serverVersion) {
			t.Errorf("output does not state the version it reports to clients:\n%s", out)
		}
	}
}

// The whole point of the flag. A tag cut without moving the constant is a real mistake this
// project has already made once, and it is invisible from anywhere else.
func TestRenderVersionWarnsWhenTheTagAndTheConstantDisagree(t *testing.T) {
	bi := &debug.BuildInfo{
		Main:      debug.Module{Path: "github.com/spirilis/mcp-linux-over-ssh", Version: "v9.9.9"},
		GoVersion: "go1.26.2",
	}
	out := renderVersion(bi, true)

	if !strings.Contains(out, "WARNING") {
		t.Fatalf("no warning for a binary built from v9.9.9 that reports %s:\n%s", serverVersion, out)
	}
	// Both numbers have to appear, or the reader cannot tell which is which.
	for _, want := range []string{"v9.9.9", serverVersion} {
		if !strings.Contains(out, want) {
			t.Errorf("warning does not mention %q:\n%s", want, out)
		}
	}
}

func TestRenderVersionStaysQuietWhenThereIsNothingToCompare(t *testing.T) {
	// None of these is a clean release tag, so none can disagree with the constant. Warning
	// on any of them would make --version cry wolf on every developer's build.
	for _, version := range []string{
		"(devel)",                              // plain `go build`, older toolchains
		"v0.3.0+dirty",                         // built from a tag with local edits
		"v0.3.1-0.20260814185209-ed977f578d6b", // pseudo-version: after a tag, untagged
		"v1.0.0-rc1",                           // prerelease
		"",                                     // no version recorded at all
	} {
		out := renderVersion(&debug.BuildInfo{
			Main:      debug.Module{Path: "example.com/m", Version: version},
			GoVersion: "go1.26.2",
		}, true)
		if strings.Contains(out, "WARNING") {
			t.Errorf("module version %q raised a mismatch warning:\n%s", version, out)
		}
	}

	// ...and the matching case is quiet too, which is the ordinary release.
	out := renderVersion(&debug.BuildInfo{
		Main:      debug.Module{Path: "example.com/m", Version: "v" + serverVersion},
		GoVersion: "go1.26.2",
	}, true)
	if strings.Contains(out, "WARNING") {
		t.Errorf("a correctly tagged build warned about itself:\n%s", out)
	}
}

// A `go install module@version` build records a version and no checkout; a local build
// records a checkout and (on older toolchains) no version. --version has to read as an
// answer in both shapes rather than printing empty fields.
func TestRenderVersionReportsWhicheverProvenanceExists(t *testing.T) {
	installed := renderVersion(&debug.BuildInfo{
		Main:      debug.Module{Path: "github.com/spirilis/mcp-linux-over-ssh", Version: "v" + serverVersion},
		GoVersion: "go1.26.2",
	}, true)
	if !strings.Contains(installed, "v"+serverVersion) {
		t.Errorf("an installed build does not show its module version:\n%s", installed)
	}
	if strings.Contains(installed, "commit:") {
		t.Errorf("an installed build has no checkout, so it must not claim a commit:\n%s", installed)
	}

	local := renderVersion(&debug.BuildInfo{
		Main:      debug.Module{Path: "github.com/spirilis/mcp-linux-over-ssh", Version: "(devel)"},
		GoVersion: "go1.26.2",
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "ed977f578d6b1c2d3e4f5a6b7c8d9e0f1a2b3c4d"},
			{Key: "vcs.time", Value: "2026-08-14T18:52:09Z"},
			{Key: "vcs.modified", Value: "true"},
		},
	}, true)
	for _, want := range []string{"ed977f578d6b", "2026-08-14T18:52:09Z", "uncommitted changes", "working tree"} {
		if !strings.Contains(local, want) {
			t.Errorf("a local build's output omits %q:\n%s", want, local)
		}
	}
	// Truncated for reading, not printed in full.
	if strings.Contains(local, "ed977f578d6b1c2d") {
		t.Errorf("the full 40-character revision was printed:\n%s", local)
	}
}

func TestReleaseVersionAcceptsOnlyCleanTags(t *testing.T) {
	for in, want := range map[string]string{
		"v0.3.0":                               "0.3.0",
		"v1.10.2":                              "1.10.2",
		"(devel)":                              "",
		"":                                     "",
		"unknown":                              "",
		"v0.3.0+dirty":                         "",
		"v0.3.1-0.20260814185209-ed977f578d6b": "",
		"v1.0.0-rc1":                           "",
	} {
		if got := releaseVersion(in); got != want {
			t.Errorf("releaseVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestWatchStdinReportsEOFWithoutTouchingTheGlobal pins the two halves of watchStdin's
// contract that the rest of the shutdown path rests on: the returned reader carries stdin's
// bytes through unchanged, and the channel closes when the real stdin reaches EOF.
//
// The os.Stdin assertion is the part worth keeping. The transport used to be handed the pipe
// by reassigning the global before constructing it, which made the seam invisible at the call
// site and load-bearing on ordering; NewStdioTransportWithStreams took the argument instead,
// and nothing should quietly go back.
func TestWatchStdinReportsEOFWithoutTouchingTheGlobal(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer pr.Close()

	realStdin := os.Stdin
	os.Stdin = pr
	defer func() { os.Stdin = realStdin }()

	stream, closed := watchStdin()

	if os.Stdin != pr {
		t.Error("watchStdin reassigned os.Stdin; the transport is handed the stream explicitly now")
	}

	select {
	case <-closed:
		t.Fatal("the EOF channel closed while stdin was still open")
	default:
	}

	const line = "{\"jsonrpc\":\"2.0\"}\n"
	if _, err := io.WriteString(pw, line); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := make([]byte, len(line))
	if _, err := io.ReadFull(stream, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != line {
		t.Errorf("stream delivered %q, want %q", got, line)
	}

	pw.Close()

	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the EOF channel never closed after stdin was closed")
	}

	// The write end is closed with the real stdin, so the transport's own read loop ends too
	// rather than parking on a pipe nothing will ever write to again.
	if _, err := io.ReadAll(stream); err != nil {
		t.Errorf("reading the stream after EOF: %v", err)
	}
}

// The chart's appVersion is the image tag it deploys, and the image reports serverVersion.
// A chart naming a version whose image predates a feature it configures is a crash-looping
// pod, so the two are bumped together or this fails.
func TestChartAppVersionMatchesTheServer(t *testing.T) {
	b, err := os.ReadFile("charts/mcp-linux-over-ssh/Chart.yaml")
	if err != nil {
		t.Fatal(err)
	}
	want := `appVersion: "` + serverVersion + `"`
	if !strings.Contains(string(b), want) {
		t.Errorf("charts/mcp-linux-over-ssh/Chart.yaml does not say %s", want)
	}
}
