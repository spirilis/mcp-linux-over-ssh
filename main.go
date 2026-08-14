// Command mcp-linux-over-ssh is a stdio-only MCP server (protocol 2026-07-28) that runs
// commands on remote hosts through persistent SSH shell sessions.
//
// Sessions are opened on demand by the start_session tool rather than at startup: the
// target is chosen per connection from parameters the config declares, and several
// independent sessions can be live at once. See PLAN.md for why each session is a
// persistent shell rather than one SSH invocation per command, and CLAUDE.md for the
// architecture that follows from it.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/pflag"
	"github.com/spirilis/generic-go-mcp/compat"
	"github.com/spirilis/generic-go-mcp/logging"
	"github.com/spirilis/generic-go-mcp/mcp"
	"github.com/spirilis/generic-go-mcp/transport"

	"github.com/spirilis/mcp-linux-over-ssh/config"
	"github.com/spirilis/mcp-linux-over-ssh/session"
	"github.com/spirilis/mcp-linux-over-ssh/tools"
)

const (
	serverName    = "mcp-linux-over-ssh"
	serverVersion = "0.3.1"

	// listTTLMs is the cache hint on tools/list and resources/list. The library defaults
	// it to five minutes, which suited a fixed catalog; the resource catalog now gains and
	// loses a pair of entries with every session, and a client caching the list that long
	// would not see a session it had just opened.
	listTTLMs = 5000
)

func main() {
	configPath := pflag.String("config", "", "Path to the YAML configuration file (required)")
	logLevel := pflag.String("log-level", "", "Override logging.level from the config file")
	helpConfig := pflag.Bool("help-config", false, "Print annotated example configurations and exit")
	showVersion := pflag.Bool("version", false, "Print version and build information and exit")
	pflag.Parse()

	// Both are checked before --config is required, since not having one yet is the whole
	// reason to ask. Writing to stdout does not break the "nothing may write to stdout"
	// rule: that rule protects the JSON-RPC framing, and these paths return before the
	// transport exists. Output a user explicitly asked for belongs on stdout so it can be
	// piped.
	if *helpConfig {
		fmt.Print(config.HelpConfig())
		return
	}
	if *showVersion {
		fmt.Print(versionInfo())
		return
	}

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
	// Logging goes to stderr by default, or to logging.file if the config sets one;
	// stdout belongs to the JSON-RPC stream and nothing else in this process may write to
	// it. logging.Initialize takes the writer directly as of generic-go-mcp v0.4.0 (see
	// config.LoggingConfig for why the file path itself lives outside the library's own
	// logging: block).
	var logOut io.Writer
	if cfg.Logging.File != "" {
		logFile, err := os.OpenFile(cfg.Logging.File, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error opening logging.file %q: %v\n", cfg.Logging.File, err)
			os.Exit(1)
		}
		defer logFile.Close()
		logOut = logFile
	}
	logging.Initialize(level, cfg.Lib.Logging.Format, logOut)

	if cfg.Lib.Server.Mode != "stdio" {
		logging.Error("this server is stdio-only", "configured_mode", cfg.Lib.Server.Mode)
		os.Exit(1)
	}

	compatEnabled := cfg.LegacyCompatEnabled()
	requireConfirmation := cfg.SSH.RequireConfirmationValue(compatEnabled)
	if !requireConfirmation && cfg.SSH.RequireConfirmation == nil {
		// The operator never set require_confirmation either way; it only came out false
		// because legacy compat is on and MRTR has no legacy equivalent. Surface that
		// loudly rather than silently changing the security posture of an existing
		// deployment that upgrades and picks up the new default.
		logging.Warn("require_confirmation defaulted to false because legacy MCP compatibility is enabled and MRTR confirmation cannot reach a pre-2026-07-28 client; set require_confirmation explicitly in the config to override")
	}

	// Everything that is not per-connection. start_session fills in Command and
	// PromptRegex by rendering cfg.Renderer against the values it was called with.
	baseSession := session.Config{
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
	}

	mgr := session.NewManager(cfg.SSH.MaxSessions, cfg.SSH.IdleTimeout)
	defer mgr.CloseAll()

	// Armed before the tools are built, because their descriptions depend on whether a gate
	// exists at all: a model that is told an administrative refusal is possible reports one
	// to the user, and a model that is not told routes around it.
	if cfg.SSH.DisableFlagFile != "" {
		gate := session.NewFlagFileGate(cfg.SSH.DisableFlagFile)
		mgr.SetGate(gate, cfg.SSH.DisableFlagPollInterval)
		logging.Info("administrative disable flag file armed",
			"path", gate.Path(), "poll_interval", cfg.SSH.DisableFlagPollInterval)
		if err := gate.Blocked(); err != nil {
			// Not fatal: the server comes up serving tools/list and answering every call
			// with the reason, which is a far better failure for whoever set the flag — and
			// for the client that just launched this process — than a binary that exits and
			// looks broken. Removing the file re-enables it with no restart.
			logging.Warn("starting with session activity already disabled", "reason", err.Error())
		}
	}

	resourceRegistry := mcp.NewResourceRegistry()
	registerSessionsIndex(resourceRegistry, mgr)

	registry := mcp.NewToolRegistry()
	registry.Register(tools.NewStartSession(mgr, cfg.Renderer, baseSession))
	registry.Register(tools.NewListSessions(mgr))
	registry.Register(tools.NewEndSession(mgr))
	registry.Register(tools.NewRunBash(mgr, requireConfirmation))
	registry.Register(tools.NewTunePromptRegex(mgr))
	if mgr.Gated() {
		// Registered only when a gate exists, so the tool's presence in tools/list is itself
		// the answer to whether this server can be disabled at all. A permanently-false
		// check on every deployment would teach a caller to skip it on the ones where it
		// means something.
		registry.Register(tools.NewIsDisabledByFlag(mgr))
	}

	listTTL := int64(listTTLMs)
	serverCfg := &mcp.ServerConfig{
		Name:         serverName,
		Version:      serverVersion,
		Instructions: instructions(requireConfirmation, cfg.Renderer.Parametric(), mgr.Gated()),
		ListTTLMs:    &listTTL,
	}
	if compatEnabled {
		// Feed server/discover, the legacy initialize diagnostic, and -32022 from one
		// list, so a client is never told three different stories about what this server
		// speaks.
		serverCfg.AdvertisedVersions = append([]string{mcp.ProtocolVersion}, compat.LegacyVersions...)
	}
	server := mcp.NewServer(registry, resourceRegistry, serverCfg)

	// After NewServer, which is what attaches the registry to the notification broker: a
	// NotifyUpdated on a detached registry reaches nobody. Sessions are created by tool
	// calls, which cannot arrive until the transport is serving, so this is comfortably
	// early enough — but it must precede the first start_session either way, since a
	// session that joined without it would never announce anything.
	mgr.SetLifecycleObserver(
		func(id string, sess *session.Session) {
			// Registered before the observer is installed, so no notification can be
			// aimed at a URI the registry does not know yet. Both run before the login
			// handshake begins, which is what lets a subscriber watch that handshake's
			// own transitions rather than missing all of them.
			registerSessionResources(resourceRegistry, id, sess)

			statusURI := tools.StatusURI(id)
			sess.SetStatusObserver(func(st session.Status) {
				// busy/ready is two lines per command and tells an operator nothing; the
				// states worth waking up for are the ones that need a human or a repair.
				if st.State == session.StateBusy.String() || st.State == session.StateReady.String() {
					logging.Debug("session status changed", "session_id", id, "state", st.State, "commands_run", st.CommandsRun)
				} else {
					logging.Info("session status changed", "session_id", id, "state", st.State, "last_error", st.LastError)
				}
				resourceRegistry.NotifyUpdated(statusURI)
				// The index carries every session's state, so it changed too. A client
				// watching the collection rather than one session is the likelier case
				// now that ids are not known ahead of time.
				resourceRegistry.NotifyUpdated(tools.SessionsURI)
			})
		},
		func(id string) {
			// Unregistering fires resources/list_changed; the session's own final "dead"
			// update has already gone out, because Manager.End closes before it removes.
			resourceRegistry.Unregister(tools.StatusURI(id))
			resourceRegistry.Unregister(tools.TranscriptURI(id))
			resourceRegistry.NotifyUpdated(tools.SessionsURI)
		},
	)

	// Wrap server in the legacy-compat overlay, not the other way around: the overlay
	// translates a pre-2026-07-28 request into the modern wire shape and forwards it to
	// server unchanged, so everything above this line (tools, resources, the status
	// observer) stays oblivious to which era a given caller is on.
	var handlerInner transport.MessageHandler = server
	if compatEnabled {
		handlerInner = compat.New(server, compat.Config{
			SessionTTL: cfg.LegacyCompatSessionTTL(),
			ServerInfo: mcp.Implementation{Name: serverName, Version: serverVersion},
		})
		logging.Info("legacy MCP compatibility enabled", "advertised_versions", serverCfg.AdvertisedVersions)
	}
	handler := &drainingHandler{inner: handlerInner}

	// Interpose a pipe on stdin before the transport claims it, so this process can see
	// the client close it. Closing stdin is the spec's shutdown signal for a stdio server,
	// but the transport reads os.Stdin directly and its Stop cannot interrupt a blocked
	// read (transport/stdio.go says as much), so without this the server outlives its
	// client — a `docker run -i` would sit there forever holding its SSH sessions open.
	stdinClosed := watchStdin()

	trans := transport.NewStdioTransport()
	if err := trans.Start(handler); err != nil {
		logging.Error("error starting transport", "error", err)
		// os.Exit skips the deferred CloseAll. Nothing can be open this early — sessions
		// only exist once a tool call has asked for one, and none can have arrived — but
		// the call costs nothing and keeps the invariant local rather than dependent on
		// that argument staying true.
		mgr.CloseAll()
		os.Exit(1)
	}

	logging.Info("MCP server ready on stdio",
		"require_confirmation", requireConfirmation,
		"legacy_compat", compatEnabled,
		"max_sessions", cfg.SSH.MaxSessions,
		"idle_timeout", cfg.SSH.IdleTimeout,
		"parameters", len(cfg.Renderer.Parameters()))

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
	// Closed here rather than left to the defer at the top so the SSH sessions are
	// actually gone before "shutdown complete" claims it. The defer stays for the paths
	// this line cannot cover, a panic above it chief among them; a session announces its
	// death once, so the second pass is a no-op.
	mgr.CloseAll()
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

// registerSessionsIndex publishes the collection of open sessions.
//
// It is registered for the process lifetime, unlike the per-session pair, and that is
// load-bearing rather than cosmetic: mcp.Server advertises a resources capability only when
// the registry is non-empty (it checks HasResources when building its capabilities), so a
// server whose only resources came and went with sessions would tell a client at startup —
// when nothing is open — that it serves no resources at all. A client that caches
// capabilities from its first handshake, which every pre-2026-07-28 client does, would
// never look again.
func registerSessionsIndex(reg *mcp.ResourceRegistry, mgr *session.Manager) {
	reg.Register(mcp.Resource{
		URI:  tools.SessionsURI,
		Name: "Open Sessions",
		Description: "Every session currently open on this server: its id, the command that opened it, the parameters it was opened with, and its live state. " +
			"The same information the list_sessions tool returns.",
		MimeType: "application/json",
	}, func(context.Context) (mcp.ResourceContentResult, error) {
		b, err := json.MarshalIndent(mgr.List(), "", "  ")
		if err != nil {
			return mcp.ResourceContentResult{}, err
		}
		return mcp.ResourceContentResult{Text: string(b)}, nil
	})
}

// registerSessionResources publishes one session's status and transcript, under URIs
// carrying its id. Both are unregistered when the session ends.
func registerSessionResources(reg *mcp.ResourceRegistry, id string, sess *session.Session) {
	reg.Register(mcp.Resource{
		URI:         tools.StatusURI(id),
		Name:        "Session " + id + " Status",
		Description: "State of session " + id + ": whether it is alive and at a prompt, the command that opened it, the prompt regex in effect, whether sentinel framing is available, and how many commands have run.",
		MimeType:    "application/json",
	}, func(context.Context) (mcp.ResourceContentResult, error) {
		b, err := json.MarshalIndent(sess.Status(), "", "  ")
		if err != nil {
			return mcp.ResourceContentResult{}, err
		}
		return mcp.ResourceContentResult{Text: string(b)}, nil
	})

	reg.Register(mcp.Resource{
		URI:  tools.TranscriptURI(id),
		Name: "Session " + id + " Transcript",
		Description: "Recent raw bytes emitted by session " + id + "'s remote shell, escape sequences intact, for diagnosing a prompt regex that does not match. " +
			"WARNING: this is verbatim terminal output and may contain command output, hostnames, or secrets that were printed to the shell.",
		MimeType: "text/plain",
	}, func(context.Context) (mcp.ResourceContentResult, error) {
		return mcp.ResourceContentResult{Text: string(sess.Transcript(0))}, nil
	})
}

// versionInfo renders --version.
func versionInfo() string {
	bi, ok := debug.ReadBuildInfo()
	return renderVersion(bi, ok)
}

// renderVersion reports what this binary is, and — the reason the flag exists — whether the
// version it *claims* is the version it was actually built from.
//
// serverVersion is a constant in this source file, and it is what every MCP client is told
// over initialize and server/discover. The Go toolchain separately records the module
// version it resolved, and that is the only thing that can answer "did `@latest` give me
// what I think it did?". The two are maintained by different mechanisms and have drifted
// before in this repository — the tags read v0.0.x while the source said 0.1.0 — so this
// prints both rather than picking one, and says so plainly when they disagree.
//
// Split out from versionInfo so the disagreement case is testable: it is precisely the case
// that cannot be reproduced from a working tree.
func renderVersion(bi *debug.BuildInfo, ok bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n", serverName, serverVersion)

	if !ok || bi == nil {
		// Only reachable for a binary stripped of its build info. Nothing below can be
		// stated, and guessing would defeat the point of asking.
		b.WriteString("\n  No build information is embedded in this binary, so the module\n")
		b.WriteString("  version it was installed from cannot be confirmed.\n")
		return b.String()
	}

	module := bi.Main.Version
	if module == "" {
		module = "unknown"
	}
	fmt.Fprintf(&b, "\n  module:        %s %s\n", bi.Main.Path, module)
	if module == "(devel)" {
		b.WriteString("                 (built from a working tree, not `go install ...@version`)\n")
	}

	if rev, t, dirty := vcsInfo(bi); rev != "" {
		fmt.Fprintf(&b, "  commit:        %s", rev)
		if dirty {
			b.WriteString(" (with uncommitted changes)")
		}
		if t != "" {
			fmt.Fprintf(&b, "  %s", t)
		}
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, "  built with:    %s  %s/%s\n", bi.GoVersion, runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(&b, "  MCP protocol:  %s\n", mcp.ProtocolVersion)
	fmt.Fprintf(&b, "  reported to clients as version %s\n", serverVersion)

	if tagged := releaseVersion(module); tagged != "" && tagged != serverVersion {
		fmt.Fprintf(&b, "\n  WARNING: this binary was built from module version %s but reports\n", module)
		fmt.Fprintf(&b, "  %s to MCP clients. The module version is the one to trust; the\n", serverVersion)
		b.WriteString("  constant in the source was not updated when the tag was cut.\n")
	}

	return b.String()
}

// releaseVersion reduces a module version to the plain release it names, or "" when this
// build carries no clean release tag at all.
//
// Anything with a prerelease or build-metadata suffix is a pseudo-version, a dirty tree or a
// prerelease — none of which is expected to equal the constant in the source, so none of
// them may raise the mismatch warning. Only an exact tag is comparable, and only an exact
// tag is what somebody running `go install ...@latest` actually got.
func releaseVersion(v string) string {
	if v == "" || v == "(devel)" || v == "unknown" {
		return ""
	}
	if strings.ContainsAny(v, "-+") {
		return ""
	}
	return strings.TrimPrefix(v, "v")
}

// vcsInfo pulls the revision, commit time and dirty flag out of the build settings. All
// three are absent from a `go install module@version` build, where the toolchain has a
// version to record and no checkout to inspect.
func vcsInfo(bi *debug.BuildInfo) (revision, when string, dirty bool) {
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
			if len(revision) > 12 {
				revision = revision[:12]
			}
		case "vcs.time":
			when = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	return revision, when, dirty
}

func instructions(requireConfirmation, parametric, gated bool) string {
	s := "Runs commands on remote hosts through persistent shell sessions. " +
		"Nothing is connected when this server starts: call start_session to open a session and get its id, pass that id to run_bash for every command, and call end_session when finished. " +
		"Sessions are capped, and each one is a real privileged login held open on the remote side, so end them rather than abandoning them. "
	if parametric {
		s += "start_session's arguments choose which host the session connects to; they are validated against patterns the operator configured. "
	}
	s += "A session takes a moment to log in — list_sessions reports each session's state, and only \"ready\" can run commands, though a command sent early fails harmlessly. " +
		"Each session is one shell running one command at a time, so working directory and exported variables persist across calls on the same id and are independent between ids. " +
		"If a session reports state \"awaiting_prompt\", or run_bash says the prompt regex has not matched, read that session's transcript resource to see what the shell printed and repair the pattern with tune_prompt_regex."
	if requireConfirmation {
		s += " Every run_bash call requires the caller to confirm the exact command via an elicitation round trip before it executes — clients must declare the elicitation capability."
	}
	if gated {
		s += " The operator can administratively disable this server at any moment, including mid-conversation: start_session and run_bash then refuse with an error saying so, and open sessions are terminated as soon as they finish whatever they were running. " +
			"That is a deliberate stop, not a fault — it needs a person to clear, so report it and do not retry. is_disabled_by_flag answers whether that is what has happened, and both it and list_sessions keep working throughout."
	}
	return s
}
