# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

A **stdio-only** MCP server (protocol **2026-07-28**, on `github.com/spirilis/generic-go-mcp` v0.5.0) that runs commands on *remote* hosts through long-lived SSH shell sessions.

The defining constraint, from `PLAN.md`: the target is a Privileged Access Management (PAM/PSM) bastion that dispatches on a specially-crafted username, so **any one SSH command can be run exactly once** and SSH multiplexing is unavailable. Everything about the architecture follows from that.

Sessions are **opened on demand by the `start_session` tool, never at startup**, and `ssh.command`/`ssh.prompt_regex` are `text/template`s rendered per session against caller-supplied parameter values. Several independent sessions run at once. This does not weaken the `PLAN.md` premise and is not in tension with it: each session is still its own one-shot SSH command, screen-scraped through its own PTY, running one command at a time. There are simply N of them rather than one, and nothing inside the single-shell engine had to change to allow it — `Session` already serialises itself.

## Commands

```bash
go build ./...
go vet ./...

# Full suite. The session tests drive a real `bash -i` under a PTY, so run them with -race:
# this package is almost entirely concurrency.
go test ./... -race

# Just the fast unit tests (skips everything that spawns a shell)
go test ./... -short

# A single test
go test ./session/ -run TestSessionRecoversFromTimeout -v

# End-to-end over the real stdio protocol. Note params._meta is mandatory on every
# request — omitting it is the most common first-contact failure.
printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}' \
  | ./mcp-linux-over-ssh --config config.yaml

# Multi-platform image (cross-compiled, never emulated). PUSH=0 builds without pushing.
./build-docker-multi.sh [tag]

# Worked config examples (0, 1 and 2 parameters). No --config needed.
./mcp-linux-over-ssh --help-config

# What this binary actually is: the version reported to clients, the module version the
# toolchain resolved, the commit, and the MCP protocol revision. No --config needed.
./mcp-linux-over-ssh --version
```

Local development without a bastion: point `ssh.command` at `bash --norc --noprofile -i`. It is a PTY session that prints a prompt and echoes, so it exercises the same code path. To exercise the *parametric* path too, template it — `env MCPLABEL={{.label}} bash --norc --noprofile -i` with a `label` parameter proves per-session rendering and isolation with no bastion in the loop.

Note that a `tools/list` like the one above is now the whole of what a fresh server can do: nothing is connected until a `start_session` call, so any end-to-end check of `run_bash` needs a driver that reads the session id out of `start_session`'s reply and feeds it back. One request per line into a pipe is not enough on its own.

## Architecture

### `session/` — the single-shell engine

Everything in this section describes **one** `session.Session`. `session/manager.go` owns the collection; see below.

`session.Session` spawns its rendered command once under a PTY (`creack/pty`) and keeps that shell until the session ends. Commands are typed in and output is scraped back out. The invariants below are load-bearing; breaking any of them corrupts the session rather than failing loudly.

- **Exactly one command at a time, per session.** Enforced by `slot`, a capacity-1 channel rather than a mutex, because acquisition must be selectable against a cancelled context. This is not theoretical: `transport/stream.go` dispatches every JSON-RPC message in its own goroutine. Because the gate lives on the `Session`, multi-session support needed nothing added for it — concurrency between sessions is simply allowed, and serialisation within one is unchanged.
- **`Start` is `Spawn` + `Handshake`, and the manager calls the halves separately.** Spawning fails fast and for reasons a caller can act on, so it happens synchronously and its error is returned; the handshake is slow in a way governed entirely by the far end (banners, MFA, PAM dispatch) and runs in a goroutine. `Start` remains as the composition for anything that wants both.
- **Two framing mechanisms.** A per-command sentinel (`cmd; printf ... "$?"`) gives exact boundaries plus a real exit code and is preferred; the configured prompt regex frames login, backstops recovery, and takes over entirely if `probeSentinel` finds the remote shell can't emit a marker.
- **The marker is built from two shell string literals** so the echoed command line contains `'END_<nonce>__'` while the emitted line contains the joined `__MCPEND_<nonce>__:`. That asymmetry is how `stripEcho` finds and discards the echo without depending on `stty -echo`, and it survives line wrapping. The nonce is fresh per command so a marker left by an abandoned command can never satisfy the next one.
- **`settle()` runs after every sentinel match.** The shell prints its prompt *after* the marker; returning the instant the marker is seen leaves those bytes in flight to arrive prefixed to the next command's output. Settle waits for the prompt, falling back to a quiet window so a wrong prompt regex cannot silently corrupt every subsequent command.
- **Cancellation and timeout share one recovery path.** `recoverSession` sends Ctrl-C and drains to a prompt on a *fresh* context — the request's own context is already cancelled by then. Failing that, it uses the single opt-in reconnect, else the session goes `Desynced`/`Dead`.
- **A failed login is not fatal.** The session lands in `StateAwaitingPrompt` so the regex can be repaired at runtime; see the tuning loop below.
- **`ansiFilter` cleans the stream as it arrives**, so a user-supplied prompt regex can be written against the text a human sees rather than SGR-interleaved bytes. It carries unfinished escape sequences and dangling CRs across read boundaries. Raw bytes still go to the transcript, which is the thing meant for diagnosis.
- **Every mutation of the `Status` snapshot notifies the observer**, which is what drives `notifications/resources/updated` for the status resource. `setState` is not the only path: `Execute`'s success path moves `commandsRun` and the state under one lock, and `SetPromptRegex`/`setSentinel` change the snapshot without touching the state at all. Miss one and subscribers are left believing the session is still busy. The observer is handed a snapshot taken under the lock and called after it is released — each snapshot is coherent, but **two snapshots are not ordered against each other**, so the signal means "re-read", never "here is the latest state".
- **Death is announced once, with its original cause.** `setState` returns early on dead→dead, because `Close` runs after whatever actually killed the session and would otherwise overwrite the diagnosis with `"closed"` and re-announce a transition the subscriber already saw. `main.go` closes twice on purpose (deferred `mgr.CloseAll`, plus explicitly before the "shutdown complete" line); this is what makes that harmless.
- **`compactLocked` bounds memory** by dropping already-scanned middle bytes while preserving the head. Reading always continues through to the marker — stopping early would desync the session. Anything dropped, by compaction or by the final `truncateMiddle`, is reported as `Result.Truncated`.

### `session/manager.go` — the collection

`Manager` holds the live sessions and **knows nothing about MCP**, so it is testable without standing up a server. Everything protocol-shaped hangs off `SetLifecycleObserver`, which `main.go` uses to register and unregister each session's resource pair.

- **The cap is enforced by construction, not by check-then-act.** `Start` holds the manager lock across `New` *and* `Spawn`, because two concurrent `start_session` calls are entirely possible and a released-then-reacquired lock would let both past a full cap. Spawn is a fork/exec, not a network round trip, so the section is short. `onAdd` and the handshake goroutine both happen after the lock is released.
- **Dead sessions do not count against the cap** (`liveCountLocked`). They are kept so the model can read *why* one died, but they hold no SSH connection; counting them would let an unreachable target wedge the server after `max_sessions` attempts.
- **`End` closes before it unregisters.** That order is what lets a subscriber watching a session see it go dead and only *then* see it leave the catalog; the reverse announces a disappearance whose reason was never delivered. A command in flight is cancelled rather than waited for. Note what "cancelled" actually looks like against a cooperative shell: closing the PTY makes bash SIGHUP its job, print `Hangup`, and then *still run the rest of the command line* — including the sentinel `printf` — so the command frames cleanly and reports exit 129 rather than an error. A shell that dies outright yields `ErrDead` instead. `TestManagerEndCancelsACommandInFlight` therefore asserts promptness and "not a clean exit 0", not a specific error.
- **The reaper skips `StateBusy` *and* `StateStarting`.** `Get` stamps `lastUsed` when a session is handed to a command, not when the command finishes, so a long-running command looks perfectly idle by that clock; and a session still logging in was never handed out at all, though a bastion login can outlast a short idle timeout. Both states are bounded (by `command_timeout` and `login_timeout`), so neither defers reaping forever. `list_sessions` reads through `List`, which deliberately does *not* stamp `lastUsed` — polling for readiness is not use.
- Session ids are random (`s-` + 4 bytes hex), not sequential: an id is the only handle on a privileged session, and a counter restarts at the same values the process does, so a stale `s-1` would silently address a different bastion.

### `session/gate.go` — the administrative kill switch

Optional, and absent entirely unless `ssh.disable_flag_file` names a path. While a file exists there, `start_session` and `run_bash` refuse and open sessions are torn down; removing it resumes service with no restart. `Gate` is a one-method interface (`Blocked() error`, nil = permitted) so `Manager` never learns what a file is; `FlagFileGate` is the only implementation.

- **Enforcement has two halves, reaching two different things.** `Manager.Start` and both gated tools call `Blocked()` *directly*, so a raised flag stops the very next call with no window at all — nothing is cached, and each check is a fresh `os.Stat`. The `enforceGate` poll loop exists solely for sessions nobody is calling about: an open, quiet shell holding a privileged login would otherwise sit there until someone happened to use it. `disable_flag_poll_interval` (default 5s) therefore bounds only *that*, never the refusal of new work — a distinction worth keeping straight when tuning it.
- **`gateCandidates` skips `StateBusy` and nothing else.** A busy session is left for a later tick, which is the whole of "terminated once any in-flight command completes": no waiting is needed, because `run_bash` consults the gate too, so a session that goes quiet under a raised flag stays quiet. Unlike the idle reaper it does *not* spare `StateStarting` — a login still being negotiated against a target the operator has just said to stop touching is the exact thing the flag exists to prevent.
- **`Blocked()` is called outside the manager's lock.** It stats a file; holding a lock every tool call needs across a filesystem operation is how a wedged mount turns a refusal into a hang.
- **The gate fails closed.** A stat error that is not `fs.ErrNotExist` counts as disabled, because a check that cannot be performed is not evidence the operator wants this server running. The message says `could not be checked` so an operator can tell that case apart from a raised flag.
- **The path is resolved to absolute once, in `NewFlagFileGate`.** An MCP client chooses this process's working directory, so a relative path is a control the operator and the server can disagree about.
- **The gate is armed in `main.go` before the tools are registered**, because `start_session`/`run_bash` append `disabledToolNote` to their descriptions only when `mgr.Gated()`. That note is not decoration: a model that meets an unexplained refusal reasons its way around it — retry, different argument, other tool — and every attempt fails identically while looking like a broken server. A flag already raised at startup is a `WARN`, never a fatal: coming up and answering every call with the reason is a better failure than a binary that exits looking broken.
- **`list_sessions` is deliberately *not* gated**, and reports the block in both its text and structured output. It is where a model goes to work out why its sessions evaporated, and answering "you may not look" would leave it with the disappearance and no cause. `end_session` and `tune_prompt_regex` are ungated for the same reason — observing and cleaning up stay available; only opening sessions and executing commands stop.
- **`is_disabled_by_flag` (`tools/is_disabled.go`) is registered only when `mgr.Gated()`**, so its *absence* from `tools/list` is the answer to "can this server be stopped that way?". A tool that is missing cannot be misread; a permanently-false field on every deployment teaches a caller to skip it on the deployments where it means something. It reports the reason — which names the flag file — only when blocked: in the false case there is nothing to act on, and the path to a kill switch is not worth handing to a caller that is at that moment free to run commands (the server host and an SSH target are usually different machines, but nothing guarantees it). Its description says a `false` answer is only true of the moment it was asked.

### The prompt-tuning loop

Screen-scraping hinges on recognising the prompt, and a regex written blind against an unfamiliar bastion often misses. Each session's `mcp+stdio:///session/<id>/transcript` resource plus the `tune_prompt_regex` tool let the model read what that shell actually printed, test a pattern, and install it — turning a restart-and-guess cycle into an in-place repair. The regex is per session, since `ssh.prompt_regex` is itself a template and two sessions against different hosts can legitimately need different patterns.

`TestPromptRegex` matches candidates against **recorded idle tails**, not the transcript. In scrollback a prompt is followed by the next command's output on the same line (no newline is emitted after a prompt), so an end-anchored pattern that works perfectly live would appear to match nothing. Idle tails are snapshots of the buffer taken whenever the shell falls silent, which is exactly what the live matcher sees.

### `main.go` — wiring

- **Nothing is spawned at startup.** `main.go` builds a `session.Manager` and the tools (five always, plus `is_disabled_by_flag` when a gate is configured), and that is all; the first SSH login happens when a client calls `start_session`. The old design spawned one shell and blocked on its full login handshake before the transport read a byte of stdin, which made any login slower than the client's own connect timeout look like a dead server (a real bastion login can easily outlast Claude Code's ~30s `CONNECT_TIMEOUT`). That pressure now lands on `start_session` instead, and is answered the same way: it returns the id as soon as the shell is spawned and lets the handshake run in a goroutine. `checkRunnable` rejects `Execute` before `StateReady` (`StateStarting`/`StateAwaitingPrompt`/etc. return a clean error without touching the PTY), so a `run_bash` racing the handshake gets `"session is not ready: starting"` rather than a corrupted session — which is what makes returning early safe rather than merely fast.
- **The lifecycle observer is installed after `NewServer`.** `NewServer` is what attaches the registry to the notification broker, and `NotifyUpdated` on a detached registry reaches nobody. Within `onAdd`, resources are registered *before* the status observer is installed, so no notification can be aimed at a URI the registry does not know yet; both happen before the handshake goroutine starts, which is what lets a subscriber watch a session's own `starting` → `ready` transitions instead of missing them.
- **A static `mcp+stdio:///sessions` resource is registered for the process lifetime, and this is load-bearing.** `mcp/server.go` advertises a resources capability only when `ResourceRegistry.HasResources()` is true. With per-session resources alone, a server with nothing open would tell a client at startup that it serves no resources at all — and every pre-2026-07-28 client caches capabilities from its `initialize` and never looks again. Verified: a legacy `initialize` against an idle server still reports `capabilities: [resources, tools]`.
- **`ListTTLMs` is lowered to 5s** from the library's 5-minute default, because the resource catalog now gains and loses a pair of entries with every session and a client caching `resources/list` that long would not see a session it had just opened.
- `watchStdin` interposes an `os.Pipe` on `os.Stdin` before the transport claims it, so the server can notice its client closing stdin. Closing stdin is the spec's shutdown signal, but the transport reads `os.Stdin` directly and its `Stop` cannot interrupt a blocked read. Without this a `docker run -i` outlives its client forever, holding every open SSH session with it.
- `drainingHandler` wraps the server to count in-flight requests, so shutdown-on-EOF waits for pending work instead of cancelling it. `printf '...' | mcp-linux-over-ssh` depends on this. It deliberately does *not* count `subscriptions/listen`: that stream lives until the transport stops, so counting it would spend the whole grace period on every shutdown that had a subscriber. The check sniffs `method` out of the raw frame, so a JSON-RPC *batch* (an array) reads as "not a listen" and is counted — the safe direction, and this binding sends one request per line anyway.
- **A failure path that calls `os.Exit` must close the sessions first**, since `os.Exit` skips defers. `trans.Start`'s error path is the one place this bites. Nothing can be open that early any more, but the `mgr.CloseAll()` stays there so the invariant is local rather than resting on that argument staying true — leaving a shell to die of PTY hangup is not good enough when the far end is a privileged bastion session.
- **Nothing may write to stdout.** The transport owns it for JSON-RPC framing; library logging goes to stderr. `--help-config` and `--version` are the two exemptions, and both are exemptions for the same reason: they `return` before the transport exists, and output a user explicitly asked for belongs on stdout so it can be piped.
- **`--version` prints two versions on purpose.** `serverVersion` is a constant in `main.go` and is what clients are told over `initialize`/`server/discover`; `debug.ReadBuildInfo().Main.Version` is what the Go toolchain resolved and is the only thing that can answer "did `go install …@latest` give me what I think it did?". Those are maintained by different mechanisms and have drifted here before (tags read `v0.0.x` while the source said `0.1.0`), so `--version` prints both and emits a `WARNING` naming both numbers when a *clean release tag* disagrees with the constant. `releaseVersion` deliberately refuses to compare anything carrying `-` or `+` — a pseudo-version, a prerelease, or a `+dirty` local build is not expected to match, and warning on those would make the flag cry wolf on every developer's build. `renderVersion` takes its `*debug.BuildInfo` as a parameter rather than reading it, which is what makes the drift case testable at all: it is precisely the case a working tree cannot reproduce.

### Protocol vocabulary: a session is never an "SSH session"

Everything a client can read — tool names, titles, descriptions, input/output schema descriptions, resource names and descriptions, the server instructions, and the client-facing error strings in `session/` — calls these **sessions**, never *SSH sessions*. SSH is how one gets opened today; it is not what one *is*. The engine drives a PTY and screen-scrapes a prompt, and would do the same over a serial console, a container `exec`, or whatever replaces SSH here later. A description that promised SSH would then have to be broken in the most expensive place to break one: the text an already-deployed client has cached from its `initialize`.

Three tests enforce this, and each was mutation-checked against a reintroduced "SSH":

- `tools/vocabulary_test.go` marshals every tool definition — in both the gated and ungated shapes — and rejects any `ssh` in the JSON. Its fixture config deliberately sets `command: "ssh -tt psmuser@{{.host}}"`, so a definition that echoed the operator's command line back would fail rather than slip through. It also pins that `is_disabled_by_flag` and the `disabledToolNote` appear only with a gate. (This is the first Go test in `tools/`; building a definition spawns nothing, so it needs no shell.)
- `main_test.go` covers what `main.go` owns: `instructions()` across all eight flag combinations, plus the URI, name and description of all three resources. `registerSessionResources` is called with a nil `*Session` — `Register` only stores the read callback, and the descriptions are built eagerly.
- `config/examples_test.go` covers `--help-config`, where the rule is narrower rather than absolute: `ssh` is legitimate there as the `ssh:` block name, as `ssh.<key>`, as an example command *value* (`ssh -tt …`), and inside `mcp-linux-over-ssh`. Anything else is prose and fails. Uppercase `SSH` fails outright, since every legitimate use is lowercase.

The `ssh:` config block keeps its name — renaming it would break every deployed config for a cosmetic gain, and an operator writing `command:` is choosing a transport, which is exactly the level at which SSH is the right word. The split is deliberate: SSH is a *configuration* concept here, never a *protocol* one.

### Config

One YAML file, two parsers: the library's `config.Load` reads `server:`/`logging:`, and `config/config.go` reads `ssh:` — the library's `Config` has no extension point. `use_sentinel` is a `*bool` so an explicit `false` is distinguishable from an omitted key (defaults true). `require_confirmation` is the same pointer shape but its default is not a fixed constant — see below. `disable_flag_file` is empty by default and that emptiness is the feature switch: no path, no gate, no stat on any call. See `config.yaml.example`.

**`ssh.command` and `ssh.prompt_regex` are `text/template`s; `config/params.go` owns them.** `config.Load` builds a `Renderer` so a template that references an undeclared parameter is a *startup* failure rather than a first-`start_session` one — the check walks the parsed tree for `*parse.FieldNode` rather than pattern-matching the source, so `{{shquote .x}}` and `{{if .x}}` count as references too. `Option("missingkey=error")` is the backstop for any node type the walk does not descend into.

One consequence to keep in mind: **`prompt_regex` can no longer be compiled at load time when it is parametric**, since its final form differs per session. `applyDefaults` still compiles it when `ssh.parameters` is empty (preserving the original fail-fast, and the existing test for it); otherwise the compile happens in `session.New` during `start_session`, where a failure is a clean tool error.

**`config/examples.go` is documentation that is compiled and tested.** `--help-config` prints three worked configurations (0, 1 and 2 parameters) and exits before the transport exists, which is why writing to stdout there does not violate the no-stdout rule. `Examples()` returns them as data so `examples_test.go` can `Load` every one, render it, check the declared parameter count matches what the example claims, and compile the rendered `prompt_regex` — the last of which nothing else checks, since a parametric one is deferred past `Load`. An example that stops parsing fails the build instead of misleading whoever copies it. Add a new example and you must add a plausible value for any new parameter name to that test, or it fails loudly.

**Parameter values are validated against a per-parameter allowlist, and this is a security boundary rather than input hygiene.** The rendered `ssh.command` is handed to `sh -c` **on the host running this server**, so a value carrying shell metacharacters is *local* command execution — a materially different and worse authority than `run_bash`'s deliberate execution on the far end — and the values come from an MCP client, not from the operator who wrote the config. `pattern:` defaults to `^[A-Za-z0-9._@:/+-]{1,256}$`: hostnames, usernames, IPs, paths and ports, and nothing a shell reads as syntax. An operator who needs looser values widens the pattern *and* reaches for the `shquote` template function, which renders a value as one safely-quoted shell word. Rejection errors name the rule but never echo the value back.

`config/config.go` also re-parses the `logging:` block itself, for `file:` — a key the library's `LoggingConfig` doesn't have. `main.go` opens it and passes it as the `io.Writer` `logging.Initialize` has taken since `generic-go-mcp` v0.4.0 (nil defaults to stderr, as before).

**`server.legacy_compat` defaults to on, the opposite of the library's own default.** `generic-go-mcp` v0.5.0 added `compat.Overlay` (see the `generic-go-mcp-consumption` skill) to let a 2026-07-28-only server also serve MCP clients still on 2025-11-25 or earlier; the library ships it off unless a config explicitly turns it on. `config.Load` inverts that: if `server.legacy_compat` is absent from the file entirely, it synthesizes `&libconfig.LegacyCompatConfig{Enabled: true}` before handing the parsed config back — an explicit `enabled: true`/`false` in the file always wins over that synthesis. The reasoning is deployment-shaped rather than protocol-shaped: most MCP clients that exist today do not yet speak 2026-07-28, so a server that silently stopped accepting them on upgrade would be a worse default than one that keeps working and lets an operator narrow it later. `main.go` wraps `server` in `compat.New(...)` — not the other way around — precisely when `cfg.LegacyCompatEnabled()` is true, and widens `mcp.ServerConfig.AdvertisedVersions` to include `compat.LegacyVersions` in the same condition, so `server/discover`, the legacy `initialize` diagnostic, and `-32022` never disagree about what this server speaks.

**`require_confirmation`'s unset default now depends on legacy compat, not a fixed `true`.** MRTR (`req.NeedInput`, which is what makes `run_bash`'s confirmation elicitation work) has no legacy-MCP equivalent — `compat.Overlay` can't downgrade it, so a legacy caller hitting a confirmation-gated `run_bash` gets a visible `isError: true` refusal on every call instead of the round trip, i.e. the tool is unusable for that caller. `SSHConfig.RequireConfirmationValue(legacyCompatEnabled bool)` therefore only honors the *explicit* pointer unconditionally; left `nil`, it returns `!legacyCompatEnabled` — false while compat is on (today's default), true otherwise (the original behavior, still what you get with `legacy_compat.enabled: false`). `main.go` logs a `WARN` at startup whenever confirmation lands on `false` purely from this default (never when it was set explicitly to `false`), since it is a real security-posture change for anyone upgrading a deployment that never mentioned `require_confirmation` in its config.

## Gotchas

- **Verify any `generic-go-mcp` API against the tagged module cache**, not a local checkout of the library, which runs ahead of what `go get` resolves. v0.2.0 lacked `ResourceRegistry.NotifyUpdated`/`Unregister` even though the checkout had them; v0.3.0 added them and this server now pushes status updates.
- **`exit` kills its session.** That shell *is* the session and its SSH command cannot be re-run — but it no longer kills the *server*, which is the one thing multi-session support genuinely softened. The `run_bash` description still warns the model; use `(exit 3)` for a non-zero status, and `end_session` to close one deliberately.
- **Durations must be written with a unit.** `idle_timeout: 0` fails to unmarshal (`cannot unmarshal !!int 0 into time.Duration`) where `0s` works — yaml.v3 parses `time.Duration` only from a string. The same trap applies to every timeout in the block, and it was live in `config.yaml.example` until testing the shipped example caught it.
- **`subscriptions/listen` filters by `params.notifications`, not by a `uri`.** A per-resource watch is `{"notifications":{"resourceSubscriptions":["<uri>"]}}`; sending `{"uri": ...}` is silently accepted and matches nothing, because the filter unmarshals to its zero value. Worth remembering when hand-driving the protocol.
- The runtime image is **alpine, not scratch** — this server's whole job is to exec an `ssh` command line, so it needs a real SSH client.

## Reference

- **`generic-go-mcp-consumption` skill** — authoritative for this library: tool/resource registration, `ToolFunction` signature, `ToolCallResult`/`ErrorResultf`, transports, and MRTR. It reads the library's real source, so prefer it over memory.
- **Sibling project `mcp-linux-bash`** — same library and tool name, but local `exec` over a UNIX socket with CLI-only config. Useful for the MRTR and registration patterns; its transport and config decisions do not apply here.
