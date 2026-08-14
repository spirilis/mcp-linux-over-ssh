# mcp-linux-over-ssh

A stdio-only MCP server (protocol `2026-07-28`) that runs commands on remote hosts
through long-lived SSH shell sessions.

It's built for targets where the SSH command can only be run once — a PAM/PSM bastion
that dispatches on a specially-crafted username, where multiplexing isn't available —
but it works against any remote shell reachable over SSH. See [PLAN.md](PLAN.md) for the
constraint that drives the design, and [CLAUDE.md](CLAUDE.md) for the architecture that
follows from it.

Sessions are opened on demand rather than at startup, and which host a session connects
to is chosen per connection from parameters the config declares, so one server can serve
several targets at once.

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

Start from a worked example — `--help-config` prints terse configurations for a fixed
target, a caller-chosen host, and a caller-chosen account and host:

```
mcp-linux-over-ssh --help-config
```

Or copy the fully commented one and point it at your target:

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

### Tools

Nothing is connected when the server starts.

| Tool | Purpose |
| --- | --- |
| `start_session` | Opens a session and returns its id. Its input schema is generated from `ssh.parameters`, so the declared parameters appear in `tools/list` with their descriptions. Returns as soon as the shell is spawned, without waiting for the login. |
| `list_sessions` | Every open session, its state, and the parameters it was opened with. This is both the readiness check and the way back to an id you've lost. |
| `run_bash` | Runs a command on the session named by `session_id`. |
| `tune_prompt_regex` | Repairs one session's prompt regex in place, without restarting. |
| `end_session` | Closes a session and frees its slot. |
| `is_disabled_by_flag` | Whether the operator has administratively disabled the server. Advertised **only** when `ssh.disable_flag_file` is configured — see below. |

Each session is one shell running one command at a time, so working directory and
exported variables persist across calls on the same id and are independent between ids.

### Parameters and safety

`ssh.command` and `ssh.prompt_regex` are Go `text/template`s rendered per session. Because
the rendered command reaches `sh -c` on the machine running this server, every parameter
value must match a regex — `^[A-Za-z0-9._@:/+-]{1,256}$` unless the config sets its own
`pattern:` — and a `{{shquote .x}}` function is available for values that need to be
looser. Declaring no parameters leaves the templates as constants.

`ssh.max_sessions` caps concurrent sessions (default 8); `ssh.idle_timeout` optionally
reaps idle ones and is off by default.

### Administrative disable

Set `ssh.disable_flag_file` to a path and it becomes a kill switch. While a file exists
there, `start_session` and `run_bash` refuse with an error saying the server is *disabled
by flag file and requires user intervention to proceed*, and sessions that are already
open are terminated as soon as they are not mid-command — a command in flight finishes,
but nothing can follow it. Removing the file resumes normal service with no restart.

Only the file's existence matters, so `touch` and `rm` are the whole interface:

```
touch /var/run/mcp-linux-over-ssh.disabled   # stop
rm    /var/run/mcp-linux-over-ssh.disabled   # resume
```

`list_sessions` keeps working throughout and reports the block, which is where a client
finds out *why* its sessions went away. So does `is_disabled_by_flag`, a no-argument tool
returning `{"disabled": true|false}` — and that tool is advertised **only** when a flag
file is configured, so its presence in `tools/list` is itself the answer to whether this
server can be stopped this way. Omit the key and neither the tool nor any of the rest of
this exists.

See `config.yaml.example` for the full set of options, and each session's
`mcp+stdio:///session/<id>/transcript` resource plus the `tune_prompt_regex` tool for
repairing a prompt regex that doesn't match your shell.

A client that subscribes to `mcp+stdio:///session/<id>/status` via `subscriptions/listen`
is sent `notifications/resources/updated` whenever that session changes — it goes busy and
ready around each command, records a new prompt regex, or dies — so a session's state can
be followed without polling. `mcp+stdio:///sessions` lists them all and is registered for
the process lifetime.

## License

[MIT](LICENSE)
