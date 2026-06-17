#!/usr/bin/env bash
# Form a two-cluster mesh with `dwxctl join` instead of hand-authored MeshPeers.
#
# This is the zero-friction join (design 0006) exercised end to end: each cluster
# mints a bundle describing itself, and the other consumes it to author the
# reciprocal MeshPeer. It is the same set of MeshPeer objects the manual heredoc
# in kind-up.sh produces, so the existing connectivity assertions pass unchanged
# — but with no CRDs written by hand.
#
# It is sourced by kind-up.sh (when JOIN=1) and is also runnable standalone given
# the two clusters' contexts, keys, endpoints, and CIDRs via the environment.
#
# Required environment:
#   CTX_A / CTX_B          kube contexts (e.g. kind-dwx-a / kind-dwx-b)
#   ID_A / ID_B            stable cluster IDs (e.g. cluster-a / cluster-b)
#   PUB_A / PUB_B          each cluster's WireGuard public key
#   EP_A / EP_B            each cluster's reachable host:port
#   POD_A / POD_B          each cluster's advertised pod CIDR
#   SVC_A / SVC_B          each cluster's advertised service CIDR
# Optional:
#   DWXCTL                 path to a prebuilt dwxctl (else built from source)
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "${HERE}/../.." && pwd)

# Build dwxctl once unless a binary was provided.
if [[ -z "${DWXCTL:-}" ]]; then
  DWXCTL="$(mktemp -d)/dwxctl"
  echo "==> building dwxctl"
  ( cd "${REPO_ROOT}" && CGO_ENABLED=0 go build -o "${DWXCTL}" ./cmd/dwxctl )
fi

# join <exporter-id> <exporter-pub> <exporter-ep> <exporter-pod> <exporter-svc> <importer-ctx>
# Mints the exporter's bundle and applies it as a MeshPeer in the importer cluster.
join() {
  local id=$1 pub=$2 ep=$3 pod=$4 svc=$5 importer_ctx=$6
  local token
  token=$("${DWXCTL}" join export \
    --cluster-id "${id}" --public-key "${pub}" --endpoint "${ep}" \
    --pod-cidrs "${pod}" --service-cidrs "${svc}")
  echo "==> [${importer_ctx}] importing bundle for ${id}"
  "${DWXCTL}" join import --context "${importer_ctx}" --bundle "${token}"
}

echo "==> forming the mesh with dwxctl join (no hand-written MeshPeers)"
# Each cluster imports the *other* cluster's bundle.
join "${ID_B}" "${PUB_B}" "${EP_B}" "${POD_B}" "${SVC_B}" "${CTX_A}"
join "${ID_A}" "${PUB_A}" "${EP_A}" "${POD_A}" "${SVC_A}" "${CTX_B}"
echo "==> mesh formed via join"
