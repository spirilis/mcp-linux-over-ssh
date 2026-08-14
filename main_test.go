package main

import (
	"strings"
	"testing"

	"github.com/spirilis/generic-go-mcp/mcp"

	"github.com/spirilis/mcp-linux-over-ssh/session"
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
				s := instructions(confirm, parametric, gated)
				if strings.Contains(strings.ToLower(s), "ssh") {
					t.Errorf("instructions(confirm=%v, parametric=%v, gated=%v) names the transport:\n%s",
						confirm, parametric, gated, s)
				}
			}
		}
	}
}

func TestResourceDescriptionsDoNotNameTheTransport(t *testing.T) {
	mgr := session.NewManager(4, 0)
	t.Cleanup(mgr.CloseAll)

	reg := mcp.NewResourceRegistry()
	registerSessionsIndex(reg, mgr)
	// A nil session is safe here: Register only stores the read callback, and nothing in
	// this test invokes it. The descriptions are built eagerly, which is all that is under
	// test.
	registerSessionResources(reg, "s-abcd1234", nil)

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
