#!/usr/bin/env bash
# Tear down the two-cluster e2e environment created by kind-up.sh.
set -euo pipefail

# Shared console styling (say/ok + color setup).
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/../lib.sh"

CLUSTER_A=${CLUSTER_A:-dwx-a}
CLUSTER_B=${CLUSTER_B:-dwx-b}

for c in "${CLUSTER_A}" "${CLUSTER_B}"; do
  if kind get clusters | grep -qx "${c}"; then
    say "🧹 deleting kind cluster ${c}"
    kind delete cluster --name "${c}"
  fi
done

# Remove the host DOCKER-USER ACCEPT rule that ROUTED-mode kind-up.sh inserts,
# so repeated up/down cycles don't leave it behind. Loop in case earlier runs
# before the idempotency guard stacked duplicates.
host_iptables() {
  if command -v sudo >/dev/null 2>&1 && [ "$(id -u)" -ne 0 ]; then
    sudo iptables "$@"
  else
    iptables "$@"
  fi
}
while host_iptables -C DOCKER-USER -j ACCEPT 2>/dev/null; do
  say "🧹 removing host DOCKER-USER ACCEPT rule"
  host_iptables -D DOCKER-USER -j ACCEPT
done

ok "🧼 teardown complete"
