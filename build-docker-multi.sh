#!/usr/bin/env bash
#
# Build and push a multi-platform image of mcp-linux-over-ssh.
#
# Usage:
#   ./build-docker-multi.sh [tag]            # build and push  (default tag: latest)
#   PUSH=0 ./build-docker-multi.sh [tag]     # build only, no push
#
set -euo pipefail

REGISTRY="${REGISTRY:-my-registry.tld}"
REPOSITORY="${REPOSITORY:-library/mcp-linux-over-ssh}"
PLATFORMS="${PLATFORMS:-linux/arm64,linux/amd64}"
BUILDER="${BUILDER:-default}"
TAG="${1:-latest}"
PUSH="${PUSH:-1}"

IMAGE="${REGISTRY}/${REPOSITORY}:${TAG}"
cd "$(dirname "$0")"

if ! docker buildx version >/dev/null 2>&1; then
    echo "docker buildx is required for multi-platform builds" >&2
    exit 1
fi

# The 'default' driver runs BuildKit embedded in dockerd, on the host's real network
# stack -- it sees /etc/hosts and the system CA trust store like any other host
# process. A docker-container builder runs BuildKit in its own container with its own
# isolated network namespace and cert store, which is the wrong tradeoff for a
# registry that's only reachable via a host-side /etc/hosts override, e.g. a
# self-hosted registry on a LAN address without split-horizon DNS. This only works for
# multi-platform push because the containerd image store is enabled (`docker info`
# shows `driver-type: io.containerd.snapshotter.v1`); without it, the default driver
# is single-platform only.
if ! docker buildx inspect "${BUILDER}" >/dev/null 2>&1; then
    echo "buildx builder '${BUILDER}' not found" >&2
    exit 1
fi

args=(build --builder "${BUILDER}" --platform "${PLATFORMS}" --tag "${IMAGE}")

if [ "${PUSH}" = "1" ]; then
    # A multi-platform result is a manifest list, which the local image store cannot
    # hold — it must go straight to a registry. There is no --load equivalent here.
    args+=(--push)
else
    echo "==> PUSH=0: building only, the result stays in the build cache"
fi

echo "==> building ${IMAGE} for ${PLATFORMS}"
docker buildx "${args[@]}" .

if [ "${PUSH}" = "1" ]; then
    echo "==> pushed ${IMAGE}"
    docker buildx imagetools inspect "${IMAGE}"
fi
