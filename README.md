# mcp-linux-over-ssh

An MCP server (protocol `2026-07-28`) that runs commands on remote hosts through
long-lived SSH shell sessions. It runs over stdio for the MCP client that launched it, or
over HTTP as a service for [kagent](https://kagent.dev) agents — see
[Serving agents over HTTP](#serving-agents-over-http).

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

To check which build you actually have — `go install …@latest` gives no direct feedback
about what it resolved:

```
mcp-linux-over-ssh --version
```

It prints the version reported to MCP clients, the module version the Go toolchain
recorded, the commit when built from a checkout, and the MCP protocol revision. Those
first two come from different places, so it says so plainly if they disagree.

By default the server speaks JSON-RPC over stdio, so it's normally launched by an MCP
client rather than run interactively. For local development without a real bastion, point
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
reaps idle ones. It is off by default over stdio and required over HTTP.

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

### Skills

`--skills <dir>` serves [Agent Skills](https://agentskills.io/specification) to clients
that load them over MCP (the `io.modelcontextprotocol/skills` extension, Beta). This is
where knowledge about *your* estate goes — which host runs the maintenance script, what
must not be run where — so a model reaching your systems through this server has it.

The flag is repeatable, and each directory is one of two shapes:

```
--skills ./skills            # a collection: ./skills/<name>/SKILL.md, one directory per skill
--skills ./etcd-maintenance  # a single skill: ./etcd-maintenance/SKILL.md
```

A single skill is published under the `name` in its frontmatter, whatever its directory
is called. Symbolic links are followed, so a mounted Kubernetes ConfigMap works. The
directories are read once at startup — restart to pick up an edit — and a directory that
cannot be loaded stops the server rather than leaving it quietly serving nothing. Without
the flag the extension is not advertised at all.

## Serving agents over HTTP

`server.mode: http` turns this into a long-running service for kagent agents.

**It is never anonymous.** The only credential accepted is the Substrate actor JWT that
kagent's egress gateway attaches for a `RemoteMCPServer` with `spec.actorJwt`: it is
verified against the cluster's own issuer, resolved to the Agent it belongs to, and that
Agent is checked against `actor_jwt.allowed_agents`. A request with no token gets `401`,
an Agent that isn't listed gets `403`, and the server refuses to start in http mode
without an `actor_jwt:` block. There is no static token, no OAuth, and no setting that
opens it up.

**Sessions belong to whoever opened them** — the Agent *and* the conversation. A caller
can't list, run commands in, read the transcript of, or end a session someone else
opened, including another conversation with the same Agent. `max_sessions` is still the
whole server's, and `ssh.idle_timeout` is required so an abandoned session is collected.

TLS is optional and either place: a `tls:` block makes the process serve HTTPS itself
(certificate files are re-read when they change, so cert-manager renewals and CSI-mounted
certificates need no restart), or leave it plain and terminate at a Gateway.

See the end of `config.yaml.example` for every key.

### Kubernetes

```
./build-docker-multi.sh 0.5.0        # linux/amd64 + linux/arm64, cross-compiled
helm install bastion ./charts/mcp-linux-over-ssh -n kagent -f my-values.yaml
```

A minimal `my-values.yaml`:

```yaml
image:
  repository: my-registry.tld/library/mcp-linux-over-ssh

ssh:
  command: "ssh -tt psmuser@{{.host}}"
  prompt_regex: '(?m)^[^\n]*[$#]\s*$'
  parameters:
    - parameter: host
      description: "Bastion to connect through"

# A Secret projected at ~/.ssh: id_ed25519, known_hosts, config, ...
sshCredentials:
  existingSecret: bastion-ssh

actorJwt:
  allowedAgents: ["kagent/my-agent"]

skills:
  # Written into a ConfigMap as <name>/SKILL.md and served.
  inject:
    - name: etcd-maintenance
      frontmatter: Use these instructions when maintaining etcd on the management hosts.
      skill: |-
        Compaction is run over Salt, from the Salt master:
        ./etcd-maintenance.sh <hostname>
  # Or an existing ConfigMap that is one skill: its keys are the files, SKILL.md included.
  configMaps: []
  #  - name: my-skill-configmap
  #    skill: my-skill
```

`frontmatter` is the skill's description — what tells a model when to reach for it. Give
a map instead of a string to set other frontmatter keys.

The chart's notes print the matching `RemoteMCPServer`. Beyond that it can:

- attach an `HTTPRoute` to an existing Gateway (`gateway.*`), with an HTTP→HTTPS redirect
  route, a `ReferenceGrant`, and a cert-manager `Certificate` for the listener
  (`certManager.*`);
- have the pod terminate TLS itself (`tls.*`) from a cert-manager `Certificate`, an
  existing Secret, or any volume you name (`tls.volume` — a CSI driver, say), with a
  `BackendTLSPolicy` so the Gateway speaks TLS to it.

It always runs one replica: sessions live in the process.

## License

[MIT](LICENSE)
