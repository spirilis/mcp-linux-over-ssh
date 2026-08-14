// Command mcp-linux-over-ssh is a stdio-only MCP server (protocol 2026-07-28) that runs
// commands on a remote host through one persistent SSH shell session.
//
// See PLAN.md for why the session is persistent rather than one SSH invocation per
// command, and CLAUDE.md for the architecture that follows from it.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/pflag"
	"github.com/spirilis/generic-go-mcp/logging"
	"github.com/spirilis/generic-go-mcp/mcp"
	"github.com/spirilis/generic-go-mcp/transport"

	"github.com/spirilis/mcp-linux-over-ssh/config"
	"github.com/spirilis/mcp-linux-over-ssh/session"
	"github.com/spirilis/mcp-linux-over-ssh/tools"
)

const (
	serverName    = "mcp-linux-over-ssh"
	serverVersion = "0.1.0"

	// stdio has no natural URI scheme for server-local resources, so invent one, as the
	// library's UNIX-socket example does with mcp+unix://.
	statusURI     = "mcp+stdio:///session/status"
	transcriptURI = "mcp+stdio:///session/transcript"
)

func main() {
	configPath := pflag.String("config", "", "Path to the YAML configuration file (required)")
	logLevel := pflag.String("log-level", "", "Override logging.level from the config file")
	pflag.Parse()

	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "--config is required")
		pflag.Usage()
		os.Exit(2)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		os.Exit(1)
	}

	level := cfg.Lib.Logging.Level
	if *logLevel != "" {
		level = *logLevel
	}
	// Logging goes to stderr; stdout belongs to the JSON-RPC stream and nothing else in
	// this process may write to it.
	logging.Initialize(level, cfg.Lib.Logging.Format)

	if cfg.Lib.Server.Mode != "stdio" {
		logging.Error("this server is stdio-only", "configured_mode", cfg.Lib.Server.Mode)
		os.Exit(1)
	}

	sess, err := session.New(session.Config{
		Command:         cfg.SSH.Command,
		PromptRegex:     cfg.SSH.PromptRegex,
		LoginTimeout:    cfg.SSH.LoginTimeout,
		CommandTimeout:  cfg.SSH.CommandTimeout,
		RecoveryTimeout: cfg.SSH.RecoveryTimeout,
		Term:            cfg.SSH.Term,
		Cols:            cfg.SSH.Cols,
		Rows:            cfg.SSH.Rows,
		InitCommands:    cfg.SSH.InitCommands,
		UseSentinel:     cfg.SSH.UseSentinelValue(),
		MaxOutputBytes:  cfg.SSH.MaxOutputBytes,
		TranscriptBytes: cfg.SSH.TranscriptBytes,
		AllowReconnect:  cfg.SSH.AllowReconnect,
	})
	if err != nil {
		logging.Error("invalid session configuration", "error", err)
		os.Exit(1)
	}

	// Start before serving: the handshake runs commands through the same single-flight
	// path the tools use, and it must not race an incoming tools/call.
	if err := sess.Start(context.Background()); err != nil {
		logging.Error("could not start the shell session", "error", err)
		os.Exit(1)
	}
	defer sess.Close()

	registry := mcp.NewToolRegistry()
	registry.Register(tools.NewRunBash(sess, cfg.SSH.RequireConfirmationValue()))
	registry.Register(tools.NewTunePromptRegex(sess))

	resourceRegistry := mcp.NewResourceRegistry()
	registerResources(resourceRegistry, sess)

	server := mcp.NewServer(registry, resourceRegistry, &mcp.ServerConfig{
		Name:         serverName,
		Version:      serverVersion,
		Instructions: instructions(cfg.SSH.RequireConfirmationValue()),
	})

	// After NewServer, which is what attaches the registry to the notification broker: a
	// NotifyUpdated on a detached registry reaches nobody. The session only changes state
	// from the reader goroutine or a tool call, neither of which can run before this point,
	// so nothing is missed by installing the observer here.
	sess.SetStatusObserver(func(st session.Status) {
		// busy/ready is two lines per command and tells an operator nothing; the states
		// worth waking up for are the ones that need a human or a repair.
		if st.State == session.StateBusy.String() || st.State == session.StateReady.String() {
			logging.Debug("session status changed", "state", st.State, "commands_run", st.CommandsRun)
		} else {
			logging.Info("session status changed", "state", st.State, "last_error", st.LastError)
		}
		resourceRegistry.NotifyUpdated(statusURI)
	})
	handler := &drainingHandler{inner: server}

	// Interpose a pipe on stdin before the transport claims it, so this process can see
	// the client close it. Closing stdin is the spec's shutdown signal for a stdio server,
	// but the transport reads os.Stdin directly and its Stop cannot interrupt a blocked
	// read (transport/stdio.go says as much), so without this the server outlives its
	// client — a `docker run -i` would sit there forever holding the SSH session open.
	stdinClosed := watchStdin()

	trans := transport.NewStdioTransport()
	if err := trans.Start(handler); err != nil {
		logging.Error("error starting transport", "error", err)
		// os.Exit skips the deferred Close, and the shell is already spawned by now.
		// Leaving it to die of PTY hangup is not good enough here: the thing on the other
		// end is a privileged bastion session, and a process that ignores SIGHUP would
		// outlive us holding it open.
		_ = sess.Close()
		os.Exit(1)
	}

	logging.Info("MCP server ready on stdio",
		"session_state", sess.Status().State,
		"require_confirmation", cfg.SSH.RequireConfirmationValue())

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		logging.Info("shutting down", "signal", sig.String())
	case <-stdinClosed:
		// Reaching EOF on stdin only means no more requests are coming, not that the ones
		// already sent have been answered — trans.Stop cancels in-flight contexts, so
		// stopping the instant EOF lands would discard responses to requests still being
		// handled. That is the whole of `printf '...' | mcp-linux-over-ssh`.
		handler.waitIdle(drainQuiet, drainGrace)
		logging.Info("shutting down", "reason", "client closed stdin")
	}
	if err := trans.Stop(); err != nil {
		logging.Error("error stopping transport", "error", err)
	}
	// Closed here rather than left to the defer at the top so the SSH session is actually
	// gone before "shutdown complete" claims it is. The defer stays for the paths this
	// line cannot cover, a panic above it chief among them; death is announced once, so
	// the second call is a no-op.
	if err := sess.Close(); err != nil {
		logging.Error("error closing session", "error", err)
	}
	logging.Info("shutdown complete")
}

// How long the server waits for work to finish once the client has closed stdin.
const (
	// drainQuiet is how long the handler must be idle before shutdown proceeds. It also
	// covers the gap between stdin reaching EOF and the transport dispatching the last
	// lines it had already buffered.
	drainQuiet = 250 * time.Millisecond
	// drainGrace caps the wait, so a command still running when the client vanishes
	// cannot hold the process open indefinitely.
	drainGrace = 10 * time.Second
)

// drainingHandler counts in-flight requests so shutdown can wait for them.
type drainingHandler struct {
	inner  transport.MessageHandler
	active atomic.Int64
}

func (d *drainingHandler) HandleMessage(ctx context.Context, data []byte, w transport.ResponseWriter) {
	// A subscriptions/listen stream only ends when the transport stops, so counting it as
	// work in flight would make every shutdown with a live subscriber burn the whole grace
	// period and warn about it. There is no response left to lose: the client that would
	// receive those notifications is the one that just closed stdin.
	if isSubscriptionStream(data) {
		d.inner.HandleMessage(ctx, data, w)
		return
	}

	d.active.Add(1)
	defer d.active.Add(-1)
	d.inner.HandleMessage(ctx, data, w)
}

// isSubscriptionStream reports whether data is a subscriptions/listen request.
//
// A JSON-RPC batch arrives as an array and fails to unmarshal into this struct, so a batch
// carrying a listen request reads as false and is counted like any other work. That is the
// safe direction — the old behavior, a slow shutdown — and the protocol binding this server
// speaks sends one request per line, so it does not arise today.
func isSubscriptionStream(data []byte) bool {
	var probe struct {
		Method string `json:"method"`
	}
	return json.Unmarshal(data, &probe) == nil && probe.Method == "subscriptions/listen"
}

// waitIdle blocks until nothing has been in flight for a full quiet window, or grace runs
// out.
func (d *drainingHandler) waitIdle(quiet, grace time.Duration) {
	deadline := time.Now().Add(grace)
	var idleSince time.Time

	for time.Now().Before(deadline) {
		if d.active.Load() == 0 {
			if idleSince.IsZero() {
				idleSince = time.Now()
			} else if time.Since(idleSince) >= quiet {
				return
			}
		} else {
			idleSince = time.Time{}
		}
		time.Sleep(10 * time.Millisecond)
	}
	logging.Warn("requests still in flight when the drain grace period expired")
}

// watchStdin replaces os.Stdin with the read end of a pipe fed from the real stdin, and
// returns a channel closed when the real stdin reaches EOF.
//
// The transport reads os.Stdin directly, so this is the seam that lets the server notice
// its client going away. The pipe is transparent to the transport: it sees the same bytes
// in the same order, and the pipe's write end is closed on EOF so the transport's own read
// loop terminates too. If the pipe cannot be created the server simply falls back to
// signal-only shutdown rather than refusing to start.
func watchStdin() <-chan struct{} {
	done := make(chan struct{})

	pr, pw, err := os.Pipe()
	if err != nil {
		logging.Warn("could not watch stdin for EOF; shutdown will require a signal", "error", err)
		return done // never closed
	}

	real := os.Stdin
	os.Stdin = pr

	go func() {
		defer close(done)
		_, _ = io.Copy(pw, real)
		_ = pw.Close()
	}()

	return done
}

func registerResources(reg *mcp.ResourceRegistry, sess *session.Session) {
	reg.Register(mcp.Resource{
		URI:         statusURI,
		Name:        "Session Status",
		Description: "State of the persistent SSH shell session: whether it is alive and at a prompt, the prompt regex in effect, whether sentinel framing is available, and how many commands have run.",
		MimeType:    "application/json",
	}, func(context.Context) (mcp.ResourceContentResult, error) {
		b, err := json.MarshalIndent(sess.Status(), "", "  ")
		if err != nil {
			return mcp.ResourceContentResult{}, err
		}
		return mcp.ResourceContentResult{Text: string(b)}, nil
	})

	reg.Register(mcp.Resource{
		URI:  transcriptURI,
		Name: "Session Transcript",
		Description: "Recent raw bytes emitted by the remote shell, escape sequences intact, for diagnosing a prompt regex that does not match. " +
			"WARNING: this is verbatim terminal output and may contain command output, hostnames, or secrets that were printed to the shell.",
		MimeType: "text/plain",
	}, func(context.Context) (mcp.ResourceContentResult, error) {
		return mcp.ResourceContentResult{Text: string(sess.Transcript(0))}, nil
	})
}

func instructions(requireConfirmation bool) string {
	s := "Runs commands on a remote host through a single persistent SSH shell session. " +
		"Because that one shell is shared, commands execute strictly one at a time and shell state (working directory, exported variables) persists between calls. " +
		"If run_bash reports that the prompt regex has not matched, read the mcp+stdio:///session/transcript resource to see what the shell printed and repair the pattern with tune_prompt_regex; " +
		"mcp+stdio:///session/status reports the session's current state."
	if requireConfirmation {
		s += " Every run_bash call requires the caller to confirm the exact command via an elicitation round trip before it executes — clients must declare the elicitation capability."
	}
	return s
}
