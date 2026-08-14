package session

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// ErrDisabled reports that session activity has been administratively stopped rather than
// having failed. Its message is deliberately addressed to whoever reads the tool result: a
// model that sees "requires user intervention" should stop retrying and say so, which is the
// difference between a kill switch and an outage.
var ErrDisabled = errors.New("disabled by flag file and requires user intervention to proceed")

// Gate decides whether session activity is currently permitted.
//
// It is consulted on every start_session and every run_bash, and polled by the manager to
// tear down sessions that are already open, so implementations must be cheap and safe for
// concurrent use.
type Gate interface {
	// Blocked returns nil while sessions may be started and used, or an error wrapping
	// ErrDisabled explaining the block. The error text reaches the MCP client verbatim.
	Blocked() error
}

// FlagFileGate blocks all session activity for as long as a file exists on disk.
//
// The file's *existence* is the whole signal; its contents are never read. That makes the
// control usable from anything — a `touch` in a cron job, a config-management resource, a
// human at 3am — with no protocol to get wrong, and it makes the reverse just as simple.
//
// Nothing is cached. Every check is a fresh stat, which costs a syscall and buys the
// property that matters most for a kill switch: the flag takes effect on the very next tool
// call rather than at the end of some polling interval. The manager's poll loop exists only
// to reach sessions that are already open and quiet, which no tool call would otherwise
// touch.
type FlagFileGate struct {
	path string
}

// NewFlagFileGate returns a gate blocked whenever path exists.
//
// The path is resolved to absolute here, once. A relative flag-file path would otherwise be
// interpreted against a working directory this server does not choose — an MCP client
// launches it — so the operator who created the file and the server looking for it could
// disagree about which file they meant. Resolving early also means every message and log
// line names a path that can be acted on directly.
func NewFlagFileGate(path string) *FlagFileGate {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	return &FlagFileGate{path: path}
}

// Path is the absolute path being watched.
func (g *FlagFileGate) Path() string { return g.path }

// Blocked stats the flag file.
//
// A stat error that is not "does not exist" fails *closed*. The gate's whole purpose is to
// stop a server that reaches privileged bastion sessions, and a check that cannot be
// performed is not evidence that the operator wants it running — an unreadable parent
// directory or a wedged mount would otherwise silently disarm the control at exactly the
// moment someone reached for it.
func (g *FlagFileGate) Blocked() error {
	_, err := os.Stat(g.path)
	switch {
	case err == nil:
		return fmt.Errorf("%w (flag file: %s)", ErrDisabled, g.path)
	case errors.Is(err, fs.ErrNotExist):
		return nil
	default:
		return fmt.Errorf("%w (flag file %s could not be checked: %v; refusing while its state is unknown)", ErrDisabled, g.path, err)
	}
}
