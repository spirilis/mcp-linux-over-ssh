# mcp-linux-over-ssh

A stdio-only MCP server (protocol `2026-07-28`) that runs commands on a remote host
through a single, long-lived SSH shell session.

It's built for targets where the SSH command can only be run once — a PAM/PSM bastion
that dispatches on a specially-crafted username, where multiplexing isn't available —
but it works against any remote shell reachable over SSH. See [PLAN.md](PLAN.md) for the
constraint that drives the design, and [CLAUDE.md](CLAUDE.md) for the architecture that
follows from it.

## Install

```
go install github.com/spirilis/mcp-linux-over-ssh@latest
```

Or build from source:

```
git clone https://github.com/spirilis/mcp-linux-over-ssh.git
cd mcp-linux-over-ssh
go build ./...
```

Requires a `go` toolchain able to satisfy `go.mod`'s `go` directive; with the default
`GOTOOLCHAIN=auto` (Go 1.21+), an older local `go` will fetch the right version
automatically.

## Usage

Copy the example config and point it at your target:

```
cp config.yaml.example config.yaml
# edit ssh.command and ssh.prompt_regex for your environment
```

Run it:

```
mcp-linux-over-ssh --config config.yaml
```

The server speaks JSON-RPC over stdio, so it's normally launched by an MCP client rather
than run interactively. For local development without a real bastion, point
`ssh.command` at `bash --norc --noprofile -i` — it's a PTY session that prints a prompt
and echoes, exercising the same code path.

See `config.yaml.example` for the full set of options, and the `session/transcript`
resource plus the `tune_prompt_regex` tool for repairing a prompt regex that doesn't
match your shell, in place, without restarting.

A client that subscribes to `mcp+stdio:///session/status` via `subscriptions/listen` is
sent `notifications/resources/updated` whenever the session changes — it goes busy and
ready around each command, records a new prompt regex, or dies — so the session's state
can be followed without polling.

## License

[MIT](LICENSE)
