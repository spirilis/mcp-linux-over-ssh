# Lessons

Patterns worth not repeating, recorded as they come up.

## Prefer an explicit API over a compatible one when the two conflict (2026-08-14)

Planning the multi-session change, I recommended keeping a session auto-started at boot when
no parameters were declared, with `session_id` optional and resolving to the sole session —
so existing configs and clients kept working untouched. The user chose the hard cutover
instead: nothing spawns at boot, `session_id` always required.

**Why I got it wrong:** I weighted "don't break what works" above "one mental model". For a
tool whose caller is a *model* rather than a person, that trade is backwards. Two modes means
the tool descriptions have to explain both, and an optional-id-that-sometimes-resolves is
exactly the kind of implicit behaviour an LLM caller gets wrong in the case that matters.
Breakage is a one-time cost the operator pays deliberately; ambiguity is a cost paid on every
call, forever.

**Apply as:** when weighing backward compatibility against a single unambiguous interface,
lead with the explicit design and offer compatibility as the alternative — not the reverse.
This holds most strongly for MCP tool surfaces, where the description *is* the documentation.

## Assert the invariant, not the mechanism (2026-08-14)

`TestManagerEndCancelsACommandInFlight` asserted that a command abandoned by `end_session`
returns an error. It fails intermittently: closing the PTY makes bash SIGHUP its job, print
`Hangup`, and then *still run the rest of the command line* — including the sentinel `printf`
— so the command frames cleanly and reports exit 129 instead. Nothing was broken; the test
asserted a detail the far end owns.

**Apply as:** when a test asserts on the *shape* of a failure that another process produces,
ask what actually has to be true. Here it was "`End` returns promptly, the command does not
hang, and the id is dead" — all of which hold whether the shell cooperates or dies outright.

Same session, same shape: two isolation checks substring-matched `/tmp` to prove sessions
don't share a shell. That trips whenever the server's own working directory sits under `/tmp`
— which it did under the scratch dir. Compare a value against its own earlier value rather
than against an environment-dependent literal.

## Load the example config in a test, don't just read it (2026-08-14)

`config.yaml.example` shipped `idle_timeout: 0`, which does not parse: yaml.v3 unmarshals
`time.Duration` only from a string, so a bare int fails with `cannot unmarshal !!int 0 into
time.Duration`. It looked obviously correct and was obviously broken.

**Apply as:** any config example the project ships is code. Run the binary against it (with
the target-specific fields substituted) as part of verification, rather than eyeballing it.

## A library's documented pattern is a claim to verify, not a spec (2026-08-21)

generic-go-mcp v0.8.0 added `StdioTransport.Done()` for stdio shutdown, and the README, the
consumption skill, the migration guide and the project harness template all spell it
`select { case <-sigCh: case <-trans.Done(): }`. Adopting it here would have hung the server
forever on any shutdown with a live subscriber: `Done()` closes only after `serve` finishes
`wg.Wait()`, and a `subscriptions/listen` handler returns only on context cancel, which only
`Stop` does — so the very signal meant to trigger `Stop` waits on `Stop`. Four documents
agreeing did not make it right for a server that has long-lived handlers.

**Apply as:** before replacing working code with a library's blessed pattern, check the
pattern against *this* codebase's concurrency, and prove it with a throwaway program rather
than reasoning about it. The check cost about five minutes; shipping it would have been a
hang in production with privileged sessions held open. Also: adopting part of a new API is a
legitimate outcome — `Done()` was still worth using after `Stop()`, just not as the trigger.
