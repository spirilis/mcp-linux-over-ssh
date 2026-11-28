# syntax=docker/dockerfile:1

# The build stage is pinned to the *build* platform and cross-compiles with GOOS/GOARCH,
# so a multi-platform build never runs a Go toolchain under QEMU emulation.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/mcp-linux-over-ssh .

# Not scratch or distroless: this server's entire job is to hand a command line to `sh -c`
# and have it exec ssh, so the runtime image needs a shell, a real SSH client, and a passwd
# entry for the uid it runs as (ssh refuses to start for a uid it cannot name). That is why
# this stage has a RUN — the one step of a multi-platform build that executes under the
# target architecture. It installs two packages; the Go toolchain never runs emulated.
FROM alpine:3.22

RUN apk add --no-cache openssh-client ca-certificates \
    && adduser -D -u 1000 -h /home/mcp mcp \
    && mkdir -p /home/mcp/.ssh \
    && chown -R mcp:mcp /home/mcp \
    && chmod 700 /home/mcp/.ssh

COPY --from=build /out/mcp-linux-over-ssh /usr/local/bin/mcp-linux-over-ssh

USER mcp
WORKDIR /home/mcp

# 8080 is only listened on with server.mode: http; a stdio run opens no socket.
EXPOSE 8080

# Mount at run time:
#   -v ./config.yaml:/etc/mcp-linux-over-ssh/config.yaml:ro
#   -v ~/.ssh/id_ed25519:/home/mcp/.ssh/id_ed25519:ro   (must be readable by uid 1000)
#   -v ~/.ssh/known_hosts:/home/mcp/.ssh/known_hosts:ro
#   -v ./skills:/etc/mcp-linux-over-ssh/skills:ro        (optional; add --skills <that path>)
#
# Two ways to run it, chosen by server.mode in the config:
#   stdio  attached to the MCP client that launched it (docker run -i --rm), never detached.
#   http   a long-running service for kagent agents and static-token callers; see
#          charts/mcp-linux-over-ssh. It needs
#          nothing writable but /tmp, so it runs with a read-only root filesystem.
ENTRYPOINT ["/usr/local/bin/mcp-linux-over-ssh"]
CMD ["--config", "/etc/mcp-linux-over-ssh/config.yaml"]
