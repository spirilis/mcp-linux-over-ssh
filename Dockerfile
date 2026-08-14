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

# Not scratch: this server's entire job is to exec an ssh command line, so the runtime
# image needs a real SSH client.
FROM alpine:3.22

RUN apk add --no-cache openssh-client ca-certificates \
    && adduser -D -u 1000 -h /home/mcp mcp \
    && mkdir -p /home/mcp/.ssh \
    && chown -R mcp:mcp /home/mcp \
    && chmod 700 /home/mcp/.ssh

COPY --from=build /out/mcp-linux-over-ssh /usr/local/bin/mcp-linux-over-ssh

USER mcp
WORKDIR /home/mcp

# Mount at run time:
#   -v ./config.yaml:/etc/mcp-linux-over-ssh/config.yaml:ro
#   -v ~/.ssh/id_ed25519:/home/mcp/.ssh/id_ed25519:ro   (must be readable by uid 1000)
#   -v ~/.ssh/known_hosts:/home/mcp/.ssh/known_hosts:ro
#
# This is a stdio MCP server: run it attached (docker run -i --rm), never detached.
ENTRYPOINT ["/usr/local/bin/mcp-linux-over-ssh"]
CMD ["--config", "/etc/mcp-linux-over-ssh/config.yaml"]
