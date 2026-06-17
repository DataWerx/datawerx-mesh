#!/usr/bin/env bash
# Hermetic tests for the `dwxctl join` bundle round-trip. No cluster required:
# they exercise export -> import --dry-run, which authors the reciprocal MeshPeer
# without touching an API server. Run: bash hack/e2e/join_test.sh
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "${HERE}/../.." && pwd)

DWXCTL="$(mktemp -d)/dwxctl"
( cd "${REPO_ROOT}" && CGO_ENABLED=0 go build -o "${DWXCTL}" ./cmd/dwxctl )

# A valid 32-byte (all-zero) Curve25519 public key in base64 — parses as a
# WireGuard key without needing the wg tool, so the test stays hermetic.
PUB="AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

fail=0
contains() { # contains <name> <haystack> <needle>
  if [[ "$2" == *"$3"* ]]; then
    echo "ok   - $1"
  else
    echo "FAIL - $1"
    echo "  expected to contain: |$3|"
    echo "  in: |$2|"
    fail=1
  fi
}
expect_fail() { # expect_fail <name> <cmd...>
  if "${@:2}" >/dev/null 2>&1; then
    echo "FAIL - $1 (expected non-zero exit)"
    fail=1
  else
    echo "ok   - $1"
  fi
}

# Mint cluster-a's bundle (stdout is just the token; the human note goes to stderr).
TOKEN=$("${DWXCTL}" join export \
  --cluster-id cluster-a --public-key "${PUB}" --endpoint 10.20.0.1:51820 \
  --pod-cidrs 10.244.0.0/16 --service-cidrs 10.96.0.0/16)

contains "export mints a versioned bundle token" "${TOKEN}" "dwxmesh.v1."

# Import it (dry-run): the reciprocal MeshPeer is authored from the bundle alone.
PEER=$(printf '%s' "${TOKEN}" | "${DWXCTL}" join import --bundle-file - --dry-run)

contains "import authors a MeshPeer"        "${PEER}" "kind: MeshPeer"
contains "peer carries the cluster ID"      "${PEER}" "clusterID: cluster-a"
contains "peer carries the public key"      "${PEER}" "${PUB}"
contains "peer carries the endpoint"        "${PEER}" "10.20.0.1:51820"
contains "peer carries the pod CIDR"        "${PEER}" "10.244.0.0/16"
contains "peer carries the service CIDR"    "${PEER}" "10.96.0.0/16"
contains "peer is labeled as join-authored" "${PEER}" "dwxctl-join"

# A deterministic name from the cluster ID makes re-import an idempotent upsert.
contains "peer name derives from cluster ID" "${PEER}" "name: cluster-a"

# Garbage and foreign tokens are rejected, not decoded into a bogus peer.
expect_fail "rejects a non-bundle token" "${DWXCTL}" join import --bundle "not-a-bundle" --dry-run
expect_fail "rejects a foreign version"  "${DWXCTL}" join import --bundle "dwxmesh.v9.AAAA" --dry-run

if [[ "${fail}" -ne 0 ]]; then
  echo "FAILED"
  exit 1
fi
echo "PASSED"
