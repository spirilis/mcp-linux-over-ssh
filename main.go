// Command mcp-linux-over-ssh is an MCP server (protocol 2026-07-28) that runs commands on
// remote hosts through persistent SSH shell sessions. It speaks over stdio to the client
// that launched it, or over HTTP to callers that present a static bearer token or, for kagent
// agents, an actor JWT; see http.go for the second and for why it is never anonymous.
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
	"github.com/spirilis/mcp-linux-over-ssh/skills"
	"github.com/spirilis/mcp-linux-over-ssh/tools"
)

const (
	serverName    = "mcp-linux-over-ssh"
	serverVersion = "0.5.1"

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
	skillRoots := pflag.StringArray("skills", nil, "Directory of Agent Skills to serve: one holding a SKILL.md, or one whose subdirectories each hold one. Repeatable")
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

	// Who a request is made for. Over stdio that is the one process that launched this
	// server; over HTTP it is whoever the auth middleware attested, and a request without
	// one is refused rather than treated as stdio's implicit caller.
	httpMode := cfg.HTTPMode()
	owner := tools.OwnerFunc(tools.SingleCaller)
	if httpMode {
		owner = authenticatedOwner
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

	handlerInner, legacySessions, err := assemble(cfg, mgr, owner, requireConfirmation, *skillRoots)
	if err != nil {
		logging.Error("error assembling the server", "error", err)
		// Nothing can be open yet, but os.Exit skips the deferred CloseAll; see below.
		mgr.CloseAll()
		os.Exit(1)
	}

	if httpMode {
		// Everything from here on differs: there is no stdin whose closing means "done",
		// and the listener is owned by this process rather than by the transport. See
		// http.go. The stdio path below is untouched by HTTP support.
		os.Exit(serveHTTP(cfg, mgr, handlerInner, legacySessions, level, logOut))
	}
	handler := &drainingHandler{inner: handlerInner}

	// Interpose a pipe on stdin so this process can see the client close it, and hand the
	// transport the read end explicitly. Closing stdin is the spec's shutdown signal for a
	// stdio server; without noticing it the server outlives its client — a `docker run -i`
	// would sit there forever holding its sessions open.
	stdin, stdinClosed := watchStdin()

	trans := transport.NewStdioTransportWithStreams(stdin, os.Stdout)
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
	// Stop cancels in-flight contexts and returns without waiting, since it cannot interrupt
	// a blocked read. Done closes once the read loop has actually unwound, and every handler
	// with it — including the subscriptions/listen streams Stop just cancelled, which are the
	// ones waitIdle deliberately does not count. Waiting here is what keeps CloseAll from
	// racing a handler that still holds a session. Bounded, because a handler that ignores
	// its context must not be able to hold the process open.
	select {
	case <-trans.Done():
	case <-time.After(stopGrace):
		logging.Warn("transport did not finish unwinding within the stop grace period")
	}
	// Closed here rather than left to the defer at the top so the SSH sessions are
	// actually gone before "shutdown complete" claims it. The defer stays for the paths
	// this line cannot cover, a panic above it chief among them; a session announces its
	// death once, so the second pass is a no-op.
	mgr.CloseAll()
	logging.Info("shutdown complete")
}

// assemble builds everything between the session manager and the transport: the tools, the
// resources, the optional skills, the protocol server, and the legacy-compat overlay around
// it. It returns the handler a transport serves, plus the hook HTTP needs for a legacy
// client's GET and DELETE (nil when compat is off).
//
// It is a function rather than a stretch of main so that the tests can stand up the same
// stack the process serves. The properties worth testing here — a caller cannot reach
// another caller's session, an anonymous request reaches nothing — are properties of how
// these pieces are wired together, and a test that wired its own copy would be testing the
// copy.
func assemble(cfg *config.Config, mgr *session.Manager, owner tools.OwnerFunc, requireConfirmation bool, skillRoots []string) (transport.MessageHandler, transport.LegacySessions, error) {
	httpMode := cfg.HTTPMode()
	compatEnabled := cfg.LegacyCompatEnabled()

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

	resourceRegistry := mcp.NewResourceRegistry()
	registerSessionsIndex(resourceRegistry, mgr, owner)

	// Optional, and absent entirely unless --skills names something: with no registry the
	// extension is not declared and skills/list answers -32601, so a server started without
	// the flag is byte-for-byte the server it was before the flag existed. A directory that
	// was named and cannot be loaded is fatal — an operator who asked for skills and got a
	// server silently serving none would find out from a model that did not know something.
	var skillRegistry *mcp.SkillRegistry
	if len(skillRoots) > 0 {
		skillRegistry = mcp.NewSkillRegistry(resourceRegistry)
		names, err := skills.Load(skillRegistry, skillRoots)
		if err != nil {
			return nil, nil, fmt.Errorf("loading skills: %w", err)
		}
		logging.Info("serving skills", "count", len(names), "skills", strings.Join(names, ", "))
	}

	registry := mcp.NewToolRegistry()
	registry.Register(tools.NewStartSession(mgr, owner, cfg.Renderer, baseSession))
	registry.Register(tools.NewListSessions(mgr, owner))
	registry.Register(tools.NewEndSession(mgr, owner))
	registry.Register(tools.NewRunBash(mgr, owner, requireConfirmation))
	registry.Register(tools.NewTunePromptRegex(mgr, owner))
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
		Instructions: instructions(requireConfirmation, cfg.Renderer.Parametric(), mgr.Gated(), httpMode),
		ListTTLMs:    &listTTL,
		// Assigning a nil *SkillRegistry is safe: the field is a concrete pointer, not an
		// interface, so nil here really is "no extension".
		Skills: skillRegistry,
		// Lets a client list skill://<name> to find a skill's bundled files. Nothing else
		// becomes listable by it: the walk is over skill resources only.
		SkillsDirectoryRead: skillRegistry != nil,
	}
	if httpMode {
		// What a caller is shown — its sessions, their transcripts — depends on who it is,
		// so an intermediary must never replay one caller's answer to another. The principal
		// is also what an MRTR confirmation is bound to: an elicitation answered by one
		// caller cannot be redeemed by a different one.
		serverCfg.DefaultCacheScope = mcp.CacheScopePrivate
		serverCfg.PrincipalFromContext = func(ctx context.Context) string {
			who, _ := authenticatedOwner(ctx)
			return who
		}
	}
	if compatEnabled {
		// Feed server/discover, the legacy initialize diagnostic, and -32022 from one
		// list, so a client is never told three different stories about what this server
		// speaks.
		serverCfg.AdvertisedVersions = append([]string{mcp.ProtocolVersion}, compat.LegacyVersions...)
	}
	server := mcp.NewServer(registry, resourceRegistry, serverCfg)

	// With more than one caller, change notifications are kept inside the caller they are
	// about; see callerScope. Nil on stdio, where there is nobody to keep them from and the
	// broker's plain fan-out is exactly right — every method on it is nil-safe, so the
	// observers below need not ask which transport they are on.
	var scope *callerScope
	if httpMode {
		scope = newCallerScope(server, owner, mgr)
	}

	// After NewServer, which is what attaches the registry to the notification broker: a
	// NotifyUpdated on a detached registry reaches nobody. Sessions are created by tool
	// calls, which cannot arrive until the transport is serving, so this is comfortably
	// early enough — but it must precede the first start_session either way, since a
	// session that joined without it would never announce anything.
	mgr.SetLifecycleObserver(
		func(id, sessOwner string, sess *session.Session) {
			// Registered before the observer is installed, so no notification can be
			// aimed at a URI the registry does not know yet. Both run before the login
			// handshake begins, which is what lets a subscriber watch that handshake's
			// own transitions rather than missing all of them.
			// Counted before the registration that announces it; see callerScope.indexChanged.
			scope.catalogChanged(sessOwner)
			registerSessionResources(resourceRegistry, id, sess, func(ctx context.Context) bool {
				who, err := owner(ctx)
				return err == nil && who == sessOwner
			})

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
				scope.indexChanged(sessOwner)
				// The index carries every session's state, so it changed too. A client
				// watching the collection rather than one session is the likelier case
				// now that ids are not known ahead of time.
				resourceRegistry.NotifyUpdated(tools.SessionsURI)
			})
		},
		func(id, sessOwner string) {
			scope.catalogChanged(sessOwner)
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
	if scope != nil {
		handlerInner = scope
	}
	var legacySessions transport.LegacySessions // HTTP's legacy GET/DELETE; nil unless compat is on
	if compatEnabled {
		overlay := compat.New(handlerInner, compat.Config{
			SessionTTL: cfg.LegacyCompatSessionTTL(),
			ServerInfo: mcp.Implementation{Name: serverName, Version: serverVersion},
		})
		handlerInner = overlay
		legacySessions = overlay.LegacySessions()
		logging.Info("legacy MCP compatibility enabled", "advertised_versions", serverCfg.AdvertisedVersions)
	}

	return handlerInner, legacySessions, nil
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
	// stopGrace caps the wait for the transport's read loop to unwind after Stop. The work
	// itself has already drained by then; this covers only the unwinding, so it is short.
	stopGrace = 2 * time.Second
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

// watchStdin returns the stream the transport should serve, plus a channel closed when the
// real stdin reaches EOF. The pipe is transparent: the transport sees the same bytes in the
// same order, and the write end is closed on EOF so its own read loop terminates too. If the
// pipe cannot be created the server falls back to serving stdin directly and shutting down
// on a signal alone, rather than refusing to start.
//
// The library offers StdioTransport.Done for this, and it is deliberately not used here.
// Done closes only once the read loop has exited *and* every in-flight handler has returned
// (transport/stream.go waits on its WaitGroup), while a subscriptions/listen handler returns
// only when the transport's context is cancelled — which nothing but Stop does. A client that
// subscribes and then closes stdin would therefore never trip Done, and this server invites
// exactly that: every session publishes a status resource whose updates are worth watching.
// Verified against v0.8.1, and the transport is byte-identical in v0.11.1; the deadlock is
// real, not theoretical. Keeping the EOF signal
// separate from the drain is what makes stdin EOF prompt regardless of what is parked.
//
// What the upgrade did buy is NewStdioTransportWithStreams: the read end is now passed to the
// transport as an argument instead of being smuggled in by reassigning the os.Stdin global
// before construction, so the seam is visible at the call site and carries no ordering trap.
func watchStdin() (io.Reader, <-chan struct{}) {
	done := make(chan struct{})

	pr, pw, err := os.Pipe()
	if err != nil {
		logging.Warn("could not watch stdin for EOF; shutdown will require a signal", "error", err)
		return os.Stdin, done // never closed
	}

	real := os.Stdin

	go func() {
		defer close(done)
		_, _ = io.Copy(pw, real)
		_ = pw.Close()
	}()

	return pr, done
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
//
// What it lists depends on who is reading: a caller sees the sessions it opened and no
// others, the same rule list_sessions applies.
func registerSessionsIndex(reg *mcp.ResourceRegistry, mgr *session.Manager, owner tools.OwnerFunc) {
	reg.Register(mcp.Resource{
		URI:  tools.SessionsURI,
		Name: "Open Sessions",
		Description: "Every session you currently have open on this server: its id, the command that opened it, the parameters it was opened with, and its live state. " +
			"The same information the list_sessions tool returns.",
		MimeType: "application/json",
	}, func(ctx context.Context) (mcp.ResourceContentResult, error) {
		who, err := owner(ctx)
		if err != nil {
			return mcp.ResourceContentResult{}, err
		}
		b, err := json.MarshalIndent(mgr.List(who), "", "  ")
		if err != nil {
			return mcp.ResourceContentResult{}, err
		}
		return mcp.ResourceContentResult{Text: string(b)}, nil
	})
}

// registerSessionResources publishes one session's status and transcript, under URIs
// carrying its id. Both are unregistered when the session ends.
//
// mine reports whether the reader is the caller that opened the session. To anyone else the
// read answers exactly as an unregistered URI does. The URIs themselves are visible to every
// caller in resources/list — the registry is one catalog — which is why holding one must
// confer nothing: a transcript is verbatim terminal output.
func registerSessionResources(reg *mcp.ResourceRegistry, id string, sess *session.Session, mine func(context.Context) bool) {
	reg.Register(mcp.Resource{
		URI:         tools.StatusURI(id),
		Name:        "Session " + id + " Status",
		Description: "State of session " + id + ": whether it is alive and at a prompt, the command that opened it, the prompt regex in effect, whether sentinel framing is available, and how many commands have run.",
		MimeType:    "application/json",
	}, func(ctx context.Context) (mcp.ResourceContentResult, error) {
		if !mine(ctx) {
			return mcp.ResourceContentResult{}, mcp.ErrResourceNotFound
		}
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
	}, func(ctx context.Context) (mcp.ResourceContentResult, error) {
		if !mine(ctx) {
			return mcp.ResourceContentResult{}, mcp.ErrResourceNotFound
		}
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

func instructions(requireConfirmation, parametric, gated, shared bool) string {
	s := "Runs commands on remote hosts through persistent shell sessions. " +
		"Nothing is connected when this server starts: call start_session to open a session and get its id, pass that id to run_bash for every command, and call end_session when finished. " +
		"Sessions are capped, and each one is a real privileged login held open on the remote side, so end them rather than abandoning them. "
	if parametric {
		s += "start_session's arguments choose which host the session connects to; they are validated against patterns the operator configured. "
	}
	s += "A session takes a moment to log in — list_sessions reports each session's state, and only \"ready\" can run commands, though a command sent early fails harmlessly. " +
		"Each session is one shell running one command at a time, so working directory and exported variables persist across calls on the same id and are independent between ids. " +
		"If a session reports state \"awaiting_prompt\", or run_bash says the prompt regex has not matched, read that session's transcript resource to see what the shell printed and repair the pattern with tune_prompt_regex."
	if shared {
		s += " This server is shared: a session belongs to the caller that opened it (your conversation, or your access token), list_sessions shows only yours, and nobody else can use or see one of yours. The session limit is the whole server's, so start_session can report it reached while you hold none — that clears as other callers finish, so wait rather than retry in a loop."
	}
	if requireConfirmation {
		s += " Every run_bash call requires the caller to confirm the exact command via an elicitation round trip before it executes — clients must declare the elicitation capability."
	}
	if gated {
		s += " The operator can administratively disable this server at any moment, including mid-conversation: start_session and run_bash then refuse with an error saying so, and open sessions are terminated as soon as they finish whatever they were running. " +
			"That is a deliberate stop, not a fault — it needs a person to clear, so report it and do not retry. is_disabled_by_flag answers whether that is what has happened, and both it and list_sessions keep working throughout."
	}
	return s
}
