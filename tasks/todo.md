# mcp-linux-over-ssh — implementation todo

Plan of record: `/home/spirilis/.claude/plans/resilient-percolating-curry.md`

## Tasks

- [x] Module scaffold: `go mod init`, dependencies
- [x] `config/config.go` — `ssh:` block parsing, defaults, validation
- [x] `session/transcript.go` — bounded raw-byte ring buffer
- [x] `session/scan.go` — incremental marker/prompt scanning, ANSI + echo stripping
- [x] `session/session.go` — spawn under PTY, handshake, Execute, recovery, lifecycle
- [x] `tools/run_bash.go` — MRTR-confirmed command execution
- [x] `tools/tune_prompt.go` — runtime prompt-regex retuning
- [x] `main.go` — flags, wiring, stdio transport, shutdown
- [x] `Dockerfile` + `build-docker-multi.sh` + `config.yaml.example`
- [x] Unit tests for the scanning/stripping/truncation seams
- [x] Verification: build, vet, tests, local `bash -i` end-to-end, protocol smoke test

## Review

Everything in the plan was built and verified. Four decisions confirmed with the user up
front were implemented as agreed: sentinel + prompt-regex framing, config-driven MRTR
confirmation defaulting to true, opt-in single reconnect, and the transcript resource
paired with a `tune_prompt_regex` tool.

### Verification performed

- `go build ./...`, `go vet ./...` clean.
- `go test ./... -race -count=2` green: 20 unit tests plus 11 integration tests that drive
  a real `bash -i` under a PTY.
- 19-check end-to-end protocol smoke test against the real binary over stdio, covering the
  `params._meta` requirement, the full MRTR confirm/decline/replay cycle, exit-code
  reporting, state persistence between calls, and both resources.
- Multi-platform image built for linux/amd64 + linux/arm64; buildkit logs confirm
  `linux/arm64->amd64` cross-compilation rather than QEMU emulation.
- Containerized server driven over stdio: session reached `ready`, sentinel framing
  negotiated against busybox `sh`, clean exit on stdin close.

### Bugs the tests found (all fixed)

1. **Split OSC escape leaked its payload.** `\x1b]` was matching the "any two-byte escape"
   class, so a half-arrived OSC sequence looked complete and its text ended up in the
   output. The escape classes now exclude every introducer that needs a terminator.
2. **The prompt desynchronised every command.** The shell prints its prompt *after* the
   sentinel marker, so returning on the marker left those bytes to arrive prefixed to the
   next command's output. Added `settle()`.
3. **`settle` could exit before the shell was quiet.** A stale wake-up token made the quiet
   timer measure nothing. Surfaced only under `-race`.
4. **`tune_prompt_regex` reported no matches on a healthy session.** Candidates were tested
   against the transcript, where each prompt is followed by the next command's output on
   the same line. Now tested against recorded idle tails, which is what the live matcher
   sees.
5. **The server outlived its client.** A stdio server must exit when stdin closes; the
   transport reads `os.Stdin` directly and cannot be interrupted. `docker run -i` hung
   forever holding the SSH session. Added `watchStdin`, then `drainingHandler` when the
   first fix turned out to cancel in-flight requests.
6. **`Truncated` under-reported.** Output elided by buffer compaction was not flagged, only
   output shortened by the final truncation.

### Deviations from the plan

- ~~**No `NotifyUpdated` on the status resource.**~~ Resolved: `generic-go-mcp` v0.3.0 tags
  `ResourceRegistry.NotifyUpdated`/`Unregister`, the dependency moved to it, and the status
  resource is pushed to subscribers again. See the follow-up below.
- **`watchStdin` and `drainingHandler` were not in the plan.** Both came out of the
  container test; the plan had assumed signal-only shutdown was acceptable.

### Known limitations

- A command that terminates the shell (`exit`, `logout`) ends the session permanently. The
  tool description warns the model, but nothing enforces it — the shell is the session.
- Interactive/full-screen programs hang until `command_timeout` and then trigger recovery.
- Prompt-regex fragility is inherent to screen-scraping. It is mitigated, not eliminated,
  by the sentinel, the non-fatal `awaiting_prompt` state, and `tune_prompt_regex`.
- The reconnect path (`allow_reconnect: true`) is implemented but has no automated test:
  exercising it needs a session that dies in a way `bash -i` does not readily reproduce.

## Follow-up: generic-go-mcp v0.3.0 — pushing status updates (2026-08-13)

The library tagged v0.3.0, a purely additive release over v0.2.0 that exports
`ResourceRegistry.NotifyUpdated` and `Unregister`. No signature moved, so the upgrade was
`go get github.com/spirilis/generic-go-mcp@v0.3.0` and nothing more.

- [x] Bump the dependency to v0.3.0.
- [x] `SetStateObserver(func(State))` became `SetStatusObserver(func(Status))`. The status
      resource serves more than the state, so the observer now carries the whole snapshot
      and fires on every change to it, not only on state transitions.
- [x] Notify from the paths that change the snapshot without going through `setState`:
      `Execute`'s success path (which moves `commandsRun` and the state under one lock),
      `SetPromptRegex`, and `setSentinel`. Missing the first would have left a subscriber
      believing the session was still busy after every command.
- [x] `main.go` installs the observer *after* `NewServer` — that call is what attaches the
      registry to the notification broker — and calls `NotifyUpdated(statusURI)`.
- [x] `drainingHandler` no longer counts `subscriptions/listen` as work in flight. That
      stream ends only when the transport stops, so a client with a subscription open made
      every shutdown burn the full 10s grace period and warn about it.
- [x] `TestStatusObserverSeesEveryStatusChange` covers the busy edge, the ready edge with
      the command counted, and a prompt-regex install. Verified it fails when either
      notification is removed.
- [x] End-to-end over real stdio: a `subscriptions/listen` on the status URI receives two
      `notifications/resources/updated` across one `run_bash` (busy, then ready), and the
      piped no-subscriber path still drains its responses.

The transcript resource is deliberately left un-notified. It changes on every byte the
shell emits, so the only honest signal would be a flood; a client diagnosing a prompt regex
reads it on demand.

### Review round (same day)

Walked the diff with the user. Four items came out of it:

- [x] **Log volume.** `busy`/`ready` are two lines per command and tell an operator nothing;
      they moved to `Debug`. `dead`/`desynced`/`awaiting_prompt` stay at `Info` with the
      cause attached. Notifications are unaffected by log level.
- [x] **Snapshot ordering documented.** Each snapshot is coherent, but two are not ordered
      against each other — a mutation that snapshots first can be beaten to the callback.
      Harmless for a "go re-read" signal, wrong for anyone treating it as a stream.
- [x] **Batch caveat documented.** `isSubscriptionStream` sniffs `method` from the raw
      frame, so a JSON-RPC batch reads as "not a listen" and gets counted — the safe
      direction, and this binding is one request per line.
- [x] **Death is announced once, with its original cause.** Investigating whether the
      explicit `sess.Close()` could be dropped turned up two real bugs instead, both
      pre-existing:
    - `Close` called `setState(StateDead, "closed")` unconditionally, overwriting the
      *actual* cause of death (`read /dev/ptmx: input/output error`) in both the status
      resource and the log. `setState` now returns early on dead→dead.
    - **`trans.Start`'s failure path exits with `os.Exit` after the shell is spawned**, and
      `os.Exit` skips the deferred `Close` — so the session was never closed and never
      announced dead there. It now closes explicitly before exiting. Leaving a privileged
      bastion session to die of PTY hangup is not good enough.
      `Start` itself cannot leak: it errors only when `spawn` fails.
    - Both `Close` calls in `main.go` stay. The explicit one keeps "shutdown complete"
      honest; the deferred one covers a panic; announcing once makes the pair a no-op.
      Noted in passing: `Close` always returns `nil`, so main's error branch is dead today.
- [x] `TestDeathIsAnnouncedOnceWithItsOriginalCause` kills the PTY under the reader
      goroutine, then closes twice. Verified it fails without the guard: three
      announcements, cause replaced by "closed".

## Parametric connections / multi-session support (2026-08-14) — DONE

Design recorded 2026-08-14 and implemented the same day. This was a genuine architecture
shift: `main.go` used to construct exactly one `session.Session` at startup from a static
`ssh:` block and close over it in every tool/resource handler; the target host and
credentials are now a per-connection choice made at MCP-call time, with an arbitrary number
of such connections live at once.

Plan of record: `/home/spirilis/.claude/plans/ethereal-swimming-popcorn.md`

### Decisions taken before implementation

Four open questions were settled with the user up front, two of them against what the
original design note assumed:

- **Hard cutover, no auto-start.** Nothing spawns at boot and `session_id` is always
  required, rather than keeping a `default` session for configs with no parameters. This
  breaks existing configs' clients by design.
- **`list_sessions` added** (not in the original note). With nothing started at boot, a
  session id exists only in the reply to the `start_session` that created it; a model whose
  context no longer holds that reply would have no way back to a live privileged login.
- **`discover_parameters` dropped.** `start_session`'s input schema is generated from the
  declared parameters instead, so they appear in `tools/list` with their descriptions,
  client-validated, with no extra round trip and no second thing to keep in sync.
- **`start_session` returns immediately**, before the login completes — the same trade the
  startup handshake already makes, for the same reason.
- **Per-parameter allowlist regex + `shquote`** for the injection question, and
  **`max_sessions` (8) with an opt-in `idle_timeout` reaper**.

### What changed

- [x] **`ssh.command` and `ssh.prompt_regex` became Go `text/template` templates**, rendered
      per session against caller-supplied values. `session.Config.Command`/`.PromptRegex` are
      still plain strings by the time they reach `session.New`; the rendering happens one
      layer up, in `config/params.go`.
- [x] **New config schema: `ssh.parameters`**, a list of objects each with `parameter:`,
      `description:` and an optional `pattern:`. Validated at load: names must be valid Go
      template identifiers and unique, patterns must compile, and every `{{.Name}}`
      referenced in either template must resolve to a declared parameter. That last check
      walks the parsed tree for `*parse.FieldNode` rather than matching the source, so
      `{{shquote .x}}` counts as a reference too.
- [x] ~~**New tool `discover_parameters`**~~ — dropped in favour of generating
      `start_session`'s input schema from the declared parameters.
- [x] **New tool `start_session`** — renders the templates, spawns the session, returns the
      id. Ids are random (`s-` + 4 bytes hex), not sequential: an id is the only handle on a
      privileged session, and a counter restarts when the process does. It returns before the
      login completes.
- [x] **New tool `end_session`** — closes the session and removes it; a command in flight is
      cancelled rather than waited for.
- [x] **New tool `list_sessions`** — the readiness check, and the way back to a lost id.
- [x] **`run_bash` gained a required `session_id`.** An unknown or ended id is a normal tool
      error naming `list_sessions`, not a protocol error.
- [x] **`session/manager.go` replaced the single `*session.Session`.** It is MCP-agnostic, so
      it is testable without standing up a server; `SetLifecycleObserver` is how `main.go`
      hangs resource registration off it. Nothing inside the single-shell engine changed to
      permit concurrency — `Session` already self-serialises through its capacity-1 `slot`.
- [x] **Resources became per-session** (`mcp+stdio:///session/{id}/status` and
      `.../transcript`), registered and unregistered with the session. A static
      `mcp+stdio:///sessions` index was added alongside them — see the review for why that
      one turned out to be load-bearing rather than a convenience.
- [x] **`tune_prompt_regex` gained a `session_id`.** The regex is a property of one running
      session now that `prompt_regex` is itself a template.
- [x] **`Session.Start` split into `Spawn` + `Handshake`** so a spawn failure is synchronous
      and reportable while the slow login is not. `Start` remains as the composition.

### Answers to the open questions

- **Command injection.** Treated as a security boundary rather than input hygiene: the
  rendered command reaches `sh -c` *on the server host*, so a metacharacter-carrying value is
  local execution — a different and worse authority than `run_bash`'s deliberate execution on
  the far end — and the values now come from an MCP client rather than from the operator who
  wrote the config. Every value must match its parameter's `pattern:`, defaulting to
  `^[A-Za-z0-9._@:/+-]{1,256}$`. A `shquote` template function covers operators who
  deliberately need to widen it. Rejection errors name the rule and never echo the value back.
- **Caps and reaping.** `ssh.max_sessions` defaults to 8, and dead sessions do not count
  against it, so an unreachable target cannot wedge the server after N attempts.
  `ssh.idle_timeout` defaults to 0 (off): killing a privileged session someone is still
  reasoning about is worse than keeping an idle one, and a stdio server's sessions already
  die with its client.
- **`allow_reconnect` and the `PLAN.md` premise.** Unchanged, per-session, and not in tension
  with anything — each session is still its own one-shot SSH command. Now stated explicitly
  in `CLAUDE.md`, since it is the load-bearing constraint the architecture follows from.
- **`CLAUDE.md`.** Rewritten rather than amended: Project Overview, `session/`, `main.go` and
  Config, plus a new `session/manager.go` section and three new gotchas.

### Review

Everything above was built and verified. Two design points emerged only during implementation
and are worth recording:

1. **The static `mcp+stdio:///sessions` resource is load-bearing.** `mcp/server.go:165`
   advertises a resources capability only when `ResourceRegistry.HasResources()` is true.
   With per-session resources alone, a server with nothing open would tell a client at
   startup that it serves no resources at all — and every pre-2026-07-28 client caches
   capabilities from its `initialize` and never looks again. Confirmed by driving a legacy
   `initialize` against an idle server: it reports `capabilities: [resources, tools]`.
2. **`ListTTLMs` had to drop** from the library's five-minute default to 5s. The resource
   catalog now gains and loses a pair of entries with every session, and a client caching
   `resources/list` for five minutes would not see a session it had just opened.

The reaper also skips `StateStarting` as well as `StateBusy`. A session still logging in was
never handed out, so `lastUsed` never moved for it, and a bastion login with banners and MFA
can outlast a short idle timeout — reaping one mid-handshake would destroy work in progress.
Both states are bounded by their own timeouts, so neither defers reaping indefinitely.

#### Verification performed

- `go build ./...`, `go vet ./...` clean.
- `go test ./... -race -count=2` green: the existing tests plus new ones covering the
  renderer, parameter validation, the manager, and the `Spawn`/`Handshake` split.
- A 35-check end-to-end run over real stdio against the built binary, with the command
  templated to `env MCPLABEL={{.label}} bash --norc --noprofile -i`: the generated schema,
  injection refusal, the cap, readiness, per-session resource registration and removal,
  cross-session isolation of both working directory and environment, exit codes, and clean
  shutdown on stdin close.
- A second 10-check pass with `require_confirmation: true` covering the MRTR round trip (the
  elicitation now names the session and its command line), a per-session
  `subscriptions/listen` seeing busy/ready transitions, and `list_changed` firing on both
  registration and unregistration.
- Legacy (`2025-11-25`) `initialize` → `tools/list` → `resources/list` → `tools/call` smoke
  test through `compat.Overlay`.
- Multi-platform image rebuilt and driven over stdio through a full start → run → end cycle.

#### Bugs found by testing

1. **`idle_timeout: 0` did not parse.** yaml.v3 unmarshals `time.Duration` only from a
   string, so the bare int in the shipped `config.yaml.example` failed with
   `cannot unmarshal !!int 0 into time.Duration`. Caught by actually loading the example
   rather than trusting it; the key is now documented as requiring a unit, and commented out
   since it is the default.
2. **Generated schema descriptions ran together.** An operator's `description:` rarely ends
   in punctuation, so it collided with the appended pattern sentence
   (`...connect through Must match...`). Terminated before appending.
3. **Two test bugs that first looked like product bugs**, both worth keeping the reasoning
   for. `TestManagerEndCancelsACommandInFlight` asserted that the abandoned command returns
   an error — but closing the PTY makes bash SIGHUP its job, print `Hangup`, and then *still
   run the sentinel `printf`*, so the command frames cleanly and reports exit 129. The real
   invariant is promptness, not an error. And the cross-session isolation checks
   substring-matched `/tmp`, which trips when the server's own working directory is under
   `/tmp`; they now compare a session's directory against its own earlier value.

### Follow-up: `--help-config` self-documentation (2026-08-14)

- [x] **`config/examples.go`** — three terse worked configurations (0 parameters / host /
      user + host) plus a notes section covering the pattern default, `shquote`, and the
      keys worth knowing. `--help-config` prints them and exits before the transport exists,
      so stdout is correct there rather than a violation of the no-stdout rule.
- [x] **`Examples()` returns them as data, not as one string**, so they can be tested.
      `config/examples_test.go` loads every example through the real parser, renders it,
      checks the declared parameter count matches what the example claims, verifies each
      pattern accepts the ordinary value for its parameter, and compiles the rendered
      `prompt_regex` — which nothing else does, since a parametric one is deferred past
      `Load`. This is the direct answer to the `idle_timeout: 0` bug above: shipped
      documentation is code, so it gets a test.
- [x] Verified beyond the unit tests by extracting the three YAML blocks back out of
      `--help-config`'s own printed output, substituting only the transport (`ssh` → a local
      shell), and starting a real server on each: they advertise `start_session` schemas
      with no arguments, `host`, and `host`+`user` respectively.

#### Known limitations

- `tools/` still has no Go tests; both constructors take a concrete `*session.Manager`, so a
  test there would spawn real shells. The protocol-level end-to-end covers that ground
  instead, but it is a Python driver in a scratch directory rather than something checked in.
- A parametric `prompt_regex` that cannot compile is only caught at `start_session`, not at
  startup. With no parameters declared the original startup check is unchanged.
- The reconnect path (`allow_reconnect: true`) still has no automated test.
