package authn

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

// agentPattern matches an Agent by namespace and name. Each half is a literal or a path.Match
// glob ("*", "team-*", "watch-?"), so "*" never crosses the "/".
type agentPattern struct{ ns, name string }

func (p agentPattern) String() string { return p.ns + "/" + p.name }

// parseAgentPatterns parses "namespace/name" entries. Blank entries are skipped. A malformed
// entry is an error: a typo should stop the server, not silently match nothing.
func parseAgentPatterns(entries []string) ([]agentPattern, error) {
	var out []agentPattern
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		ns, name, ok := strings.Cut(e, "/")
		if !ok || ns == "" || name == "" || strings.Contains(name, "/") {
			return nil, fmt.Errorf("allowed agent %q must be namespace/name", e)
		}
		for _, half := range []string{ns, name} {
			if _, err := path.Match(half, ""); err != nil { // malformed glob, e.g. "["
				return nil, fmt.Errorf("allowed agent %q: %w", e, err)
			}
		}
		out = append(out, agentPattern{ns, name})
	}
	return out, nil
}

// matchAgent reports whether the resolved Agent matches any pattern. It is matched by the
// Agent's own namespace/name, never by the token's actor: that names a session's sandbox.
func matchAgent(patterns []agentPattern, ns, name string) bool {
	for _, p := range patterns {
		nsOK, _ := path.Match(p.ns, ns)
		nameOK, _ := path.Match(p.name, name)
		if nsOK && nameOK {
			return true
		}
	}
	return false
}

// Kubernetes names: a namespace is a DNS label, an Agent's name a DNS subdomain.
var (
	dnsLabelRE     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	dnsSubdomainRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)
)

func validAgentName(namespace, name string) bool {
	return dnsLabelRE.MatchString(namespace) && dnsSubdomainRE.MatchString(name)
}
