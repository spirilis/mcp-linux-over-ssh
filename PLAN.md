This is a stdio-only MCP server, implementing MCP 2026-07-28, which executes an SSH command
(provided by config YAML file) and maintains the shell session open, using the shell session to execute
commands on a remote host.

We will assume that for "reasons" unknown to us, the SSH command cannot simply be executed multiple times
(1 for every tool run) and also, SSH parallel sessions (multiple channels on the same SSH link) aren't
available to us here.  This is the case for Privileged Access Management PSM systems, which provide a bastion
host that executes upon a specially-crafted username to determine where to divert the session further, making
simple SSH services unavailable beyond basic single-session usage.

As such, this software will need to manage the SSH session, looking for a typical regex indicating the
shell prompt.  It will then carry out the run_bash() tool commands on this shell prompt and collect the output,
looking for its shell prompt regex again and filtering that from the returned value.

Only 1 tool execution may run at a time as a result of this single-session limitation.

Please provide a Dockerfile for executing this as well, and a `build-docker-multi.sh` script which
provides a docker build with --platform linux/arm64,linux/amd64 as well.

Our OCI registry is `oci.spirilis.net` and we should place this in the `oci.spirilis.net/library/mcp-linux-over-ssh`
repository.
