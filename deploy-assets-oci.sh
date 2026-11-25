#!/usr/bin/env bash
#
# Build and push the multi-platform image, then package and push the Helm chart.
# The image tag and the chart's appVersion are the same value, read from Chart.yaml.
#
# Usage:
#   REGISTRY=registry.example.com ./deploy-assets-oci.sh   # build and push both
#   PUSH=0 ./deploy-assets-oci.sh                          # build and package only
#
# Pushes:
#   ${REGISTRY}/${REPOSITORY}:<appVersion> and :latest     (linux/amd64 + linux/arm64)
#   oci://${REGISTRY}/${HELM_REPOSITORY}/mcp-linux-over-ssh:<chart version>
#
set -euo pipefail
cd "$(dirname "$0")"

REGISTRY="${REGISTRY:-my-registry.tld}"
REPOSITORY="${REPOSITORY:-library/mcp-linux-over-ssh}"
HELM_REPOSITORY="${HELM_REPOSITORY:-helm}"
PLATFORMS="${PLATFORMS:-linux/amd64,linux/arm64}"
BUILDER="${BUILDER:-default}"
PUSH="${PUSH:-1}"

chart="charts/mcp-linux-over-ssh"

# helm, not yq: it is needed below anyway, and it reads the chart the way an install will.
appver="$(helm show chart "${chart}" | awk '$1 == "appVersion:" { gsub(/"/, "", $2); print $2 }')"
chartver="$(helm show chart "${chart}" | awk '$1 == "version:" { print $2 }')"
if [ -z "${appver}" ] || [ -z "${chartver}" ]; then
    echo "could not read version and appVersion from ${chart}/Chart.yaml" >&2
    exit 1
fi
echo "Application version: ${appver}"
echo "Chart version:       ${chartver}"

# The image reports serverVersion and the chart deploys the image tagged appVersion. If
# they disagree, the tag about to be pushed would name a binary that calls itself something
# else. `go test` checks the same thing; this is the last chance to notice.
if ! grep -q "serverVersion = \"${appver}\"" main.go; then
    echo "main.go's serverVersion is not ${appver}; bump it and the chart's appVersion together" >&2
    exit 1
fi

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

image="${REGISTRY}/${REPOSITORY}"
args=(build --builder "${BUILDER}" --platform "${PLATFORMS}" --tag "${image}:${appver}" --tag "${image}:latest")
if [ "${PUSH}" = "1" ]; then
    # A multi-platform result is a manifest list, which goes straight to the registry.
    args+=(--push)
else
    echo "==> PUSH=0: building and packaging only; nothing leaves this machine"
fi

echo "==> building ${image}:${appver} for ${PLATFORMS}"
docker buildx "${args[@]}" .

# Lint with the least a real install must set, so a chart that cannot render is caught
# before it is published rather than by whoever installs it.
echo "==> packaging ${chart} ${chartver}"
helm lint "${chart}" --set ssh.command=x --set ssh.prompt_regex=y >/dev/null

out="$(mktemp -d)"
trap 'rm -rf "${out}"' EXIT
helm package "${chart}" --destination "${out}" >/dev/null
package="${out}/mcp-linux-over-ssh-${chartver}.tgz"

if [ "${PUSH}" = "1" ]; then
    helm push "${package}" "oci://${REGISTRY}/${HELM_REPOSITORY}"
    echo "==> pushed ${image}:${appver} and :latest"
    docker buildx imagetools inspect "${image}:${appver}"
    echo "==> pushed oci://${REGISTRY}/${HELM_REPOSITORY}/mcp-linux-over-ssh:${chartver}"
    echo "    helm install REL oci://${REGISTRY}/${HELM_REPOSITORY}/mcp-linux-over-ssh --version ${chartver} \\"
    echo "      --set image.repository=${image} -f my-values.yaml"
else
    echo "==> built ${image}:${appver}; packaged $(basename "${package}") ($(wc -c <"${package}") bytes), not pushed"
fi
