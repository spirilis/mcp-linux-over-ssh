# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

A **stdio-only** MCP server (protocol **2026-07-28**, on `github.com/spirilis/generic-go-mcp` v0.3.0) that runs commands on a *remote* host through a single, long-lived SSH shell session.

The defining constraint, from `PLAN.md`: the target is a Privileged Access Management (PAM/PSM) bastion that dispatches on a specially-crafted username, so **the SSH command can be run exactly once** and SSH multiplexing is unavailable. Everything about the architecture follows from that.

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
```

Local development without a bastion: point `ssh.command` at `bash --norc --noprofile -i`. It is a PTY session that prints a prompt and echoes, so it exercises the same code path.

## Architecture

### `session/` — the single-shell engine

`session.Session` spawns the configured command once under a PTY (`creack/pty`) and keeps that shell for the process lifetime. Commands are typed in and output is scraped back out. The invariants below are load-bearing; breaking any of them corrupts the session rather than failing loudly.

- **Exactly one command at a time.** Enforced by `slot`, a capacity-1 channel rather than a mutex, because acquisition must be selectable against a cancelled context. This is not theoretical: `transport/stream.go` dispatches every JSON-RPC message in its own goroutine.
- **Two framing mechanisms.** A per-command sentinel (`cmd; printf ... "$?"`) gives exact boundaries plus a real exit code and is preferred; the configured prompt regex frames login, backstops recovery, and takes over entirely if `probeSentinel` finds the remote shell can't emit a marker.
- **The marker is built from two shell string literals** so the echoed command line contains `'END_<nonce>__'` while the emitted line contains the joined `__MCPEND_<nonce>__:`. That asymmetry is how `stripEcho` finds and discards the echo without depending on `stty -echo`, and it survives line wrapping. The nonce is fresh per command so a marker left by an abandoned command can never satisfy the next one.
- **`settle()` runs after every sentinel match.** The shell prints its prompt *after* the marker; returning the instant the marker is seen leaves those bytes in flight to arrive prefixed to the next command's output. Settle waits for the prompt, falling back to a quiet window so a wrong prompt regex cannot silently corrupt every subsequent command.
- **Cancellation and timeout share one recovery path.** `recoverSession` sends Ctrl-C and drains to a prompt on a *fresh* context — the request's own context is already cancelled by then. Failing that, it uses the single opt-in reconnect, else the session goes `Desynced`/`Dead`.
- **A failed login is not fatal.** The session lands in `StateAwaitingPrompt` so the regex can be repaired at runtime; see the tuning loop below.
- **`ansiFilter` cleans the stream as it arrives**, so a user-supplied prompt regex can be written against the text a human sees rather than SGR-interleaved bytes. It carries unfinished escape sequences and dangling CRs across read boundaries. Raw bytes still go to the transcript, which is the thing meant for diagnosis.
- **Every mutation of the `Status` snapshot notifies the observer**, which is what drives `notifications/resources/updated` for the status resource. `setState` is not the only path: `Execute`'s success path moves `commandsRun` and the state under one lock, and `SetPromptRegex`/`setSentinel` change the snapshot without touching the state at all. Miss one and subscribers are left believing the session is still busy. The observer is handed a snapshot taken under the lock and called after it is released — each snapshot is coherent, but **two snapshots are not ordered against each other**, so the signal means "re-read", never "here is the latest state".
- **Death is announced once, with its original cause.** `setState` returns early on dead→dead, because `Close` runs after whatever actually killed the session and would otherwise overwrite the diagnosis with `"closed"` and re-announce a transition the subscriber already saw. `main.go` closes twice on purpose (deferred, plus explicitly before the "shutdown complete" line); this is what makes that harmless.
- **`compactLocked` bounds memory** by dropping already-scanned middle bytes while preserving the head. Reading always continues through to the marker — stopping early would desync the session. Anything dropped, by compaction or by the final `truncateMiddle`, is reported as `Result.Truncated`.

### The prompt-tuning loop

Screen-scraping hinges on recognising the prompt, and a regex written blind against an unfamiliar bastion often misses. The `session/transcript` resource plus the `tune_prompt_regex` tool let the model read what the shell actually printed, test a pattern, and install it — turning a restart-and-guess cycle into an in-place repair.

`TestPromptRegex` matches candidates against **recorded idle tails**, not the transcript. In scrollback a prompt is followed by the next command's output on the same line (no newline is emitted after a prompt), so an end-anchored pattern that works perfectly live would appear to match nothing. Idle tails are snapshots of the buffer taken whenever the shell falls silent, which is exactly what the live matcher sees.

### `main.go` — wiring

- **The SSH login handshake (`sess.Start`) runs in a goroutine after the transport is already serving requests, not before.** It used to block startup — spawn the shell, then wait out the full login handshake — before the transport ever read a byte of stdin. That made every login slower than a client's own connect timeout look like a dead server: a real bastion login (banners, MFA, PAM dispatch) can easily take longer than Claude Code's ~30s `CONNECT_TIMEOUT`, and the process produced no stdio traffic at all until `sess.Start` returned. `checkRunnable` already rejects `Execute` calls made before the session reaches `StateReady` (`StateStarting`/`StateAwaitingPrompt`/etc. all return a clean error without touching the PTY), so nothing actually depended on the old ordering — a tool call landing mid-handshake was always going to fail safely, just synchronously blocked instead of returning `"session is not ready: starting"` immediately. If `sess.Start` fails outright (spawn failure, not a handshake timeout — those land in `StateAwaitingPrompt` instead), the goroutine stops the transport and closes the session itself before `os.Exit`, since `os.Exit` skips the deferred `Close`.
- `watchStdin` interposes an `os.Pipe` on `os.Stdin` before the transport claims it, so the server can notice its client closing stdin. Closing stdin is the spec's shutdown signal, but the transport reads `os.Stdin` directly and its `Stop` cannot interrupt a blocked read. Without this a `docker run -i` outlives its client forever, holding the SSH session open.
- `drainingHandler` wraps the server to count in-flight requests, so shutdown-on-EOF waits for pending work instead of cancelling it. `printf '...' | mcp-linux-over-ssh` depends on this. It deliberately does *not* count `subscriptions/listen`: that stream lives until the transport stops, so counting it would spend the whole grace period on every shutdown that had a subscriber. The check sniffs `method` out of the raw frame, so a JSON-RPC *batch* (an array) reads as "not a listen" and is counted — the safe direction, and this binding sends one request per line anyway.
- **A failure after the session is spawned must close it before `os.Exit`**, which skips defers. `trans.Start`'s error path is the one place this bites; leaving the shell to die of PTY hangup is not good enough when the far end is a privileged bastion session.
- **The status observer is installed after `NewServer` but before `sess.Start`**, so it is attached to the resource registry (`NotifyUpdated` on a detached registry reaches nobody) *and* in place before the reader goroutine or a tool call could first change the session's state. That ordering is what lets a subscriber see the login handshake's own transitions (`starting` → `awaiting_prompt`/`ready`) rather than missing all of them, now that the handshake runs concurrently with the transport instead of fully before it.
- **Nothing may write to stdout.** The transport owns it for JSON-RPC framing; library logging goes to stderr.

### Config

One YAML file, two parsers: the library's `config.Load` reads `server:`/`logging:`, and `config/config.go` reads `ssh:` — the library's `Config` has no extension point. `use_sentinel` and `require_confirmation` are `*bool` so an explicit `false` is distinguishable from an omitted key (both default true). See `config.yaml.example`.

`config/config.go` also re-parses the `logging:` block itself, for `file:` — a key the library's `LoggingConfig` doesn't have. `main.go` opens it and passes it as the `io.Writer` `logging.Initialize` has taken since `generic-go-mcp` v0.4.0 (nil defaults to stderr, as before).

## Gotchas

- **Verify any `generic-go-mcp` API against the tagged module cache**, not a local checkout of the library, which runs ahead of what `go get` resolves. v0.2.0 lacked `ResourceRegistry.NotifyUpdated`/`Unregister` even though the checkout had them; v0.3.0 added them and this server now pushes status updates.
- **`exit` kills the server.** That shell *is* the session and the SSH command cannot be re-run. The `run_bash` description warns the model; use `(exit 3)` for a non-zero status.
- The runtime image is **alpine, not scratch** — this server's whole job is to exec an `ssh` command line, so it needs a real SSH client.

## Reference

- **`generic-go-mcp-consumption` skill** — authoritative for this library: tool/resource registration, `ToolFunction` signature, `ToolCallResult`/`ErrorResultf`, transports, and MRTR. It reads the library's real source, so prefer it over memory.
- **Sibling project `mcp-linux-bash`** — same library and tool name, but local `exec` over a UNIX socket with CLI-only config. Useful for the MRTR and registration patterns; its transport and config decisions do not apply here.
