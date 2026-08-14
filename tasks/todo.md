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
