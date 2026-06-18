#!/usr/bin/env bash
# This script joins an existing cluster into a DataWerx mesh in
# bring-your-own-overlay (routed) mode, and prints the MeshPeer the *other*
# cluster(s) should apply to reach it.
#
# Run it once against each cluster, then cross-apply the MeshPeer manifests it prints.
#
# What it does:
#   1. helm-installs the DataWerx agent in routed mode (no WireGuard device),
#      pointed at your existing overlay interface (e.g. tailscale0)
#   2. Waits for it to roll out and reads the overlay IP the agent self-discovers
#      from its logs so you don't look it up by hand
#   3. Prints a ready-to-apply MeshPeer describing this cluster
#
# Assumptions/preconditions of routed mode:
#   * Your cluster's NODES are already on the overlay (e.g. `tailscale up` has
#     run on the node, so `tailscale0` exists in the node's network namespace).
#     The agent is a hostNetwork DaemonSet, so it sees the node's overlay
#     interface — this works on real nodes (k3s, kubeadm), NOT nested kind.
#   * net.ipv4.ip_forward=1 on the nodes (most distros/CNIs set this already).
#   * You have an agent image the nodes can pull (--image), or have side-loaded one.
set -euo pipefail

# Shared console styling (say/ok/warn/die + color setup).
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/../lib.sh"

CLUSTER_ID=""
CONTEXT=""
IFACE="tailscale0"
LOCAL_CIDRS="10.244.0.0/16,10.96.0.0/16"
IMAGE=""
NAMESPACE="datawerx-system"
REMAP=""
REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
CHART="${REPO_ROOT}/charts/datawerx-mesh"

usage() {
  cat <<USAGE
Usage: $0 --cluster-id ID [options]

Required:
  --cluster-id ID        This cluster's mesh ID (e.g. cluster-a).

Options:
  --context CTX          kubectl/helm context (default: current).
  --iface NAME           Existing overlay interface (default: ${IFACE}).
  --local-cidrs CIDRs    This cluster's pod,svc ranges (default: ${LOCAL_CIDRS}).
  --image REF            Agent image the nodes can pull (default: chart default).
  --namespace NS         Install namespace (default: ${NAMESPACE}).
  --remap CIDR           Enable overlapping-CIDR remap from this pool (optional).
  -h, --help             Show this help.
USAGE
}

while [ $# -gt 0 ]; do
  case "$1" in
    --cluster-id) CLUSTER_ID="$2"; shift 2;;
    --context) CONTEXT="$2"; shift 2;;
    --iface) IFACE="$2"; shift 2;;
    --local-cidrs) LOCAL_CIDRS="$2"; shift 2;;
    --image) IMAGE="$2"; shift 2;;
    --namespace) NAMESPACE="$2"; shift 2;;
    --remap) REMAP="$2"; shift 2;;
    -h|--help) usage; exit 0;;
    *) die "unknown arg: $1"; usage; exit 1;;
  esac
done

[ -n "${CLUSTER_ID}" ] || { die "--cluster-id is required"; usage; exit 1; }
command -v kubectl >/dev/null || { die "kubectl not found"; exit 1; }
command -v helm >/dev/null || { die "helm not found"; exit 1; }

KCTX=(); [ -n "${CONTEXT}" ] && KCTX=(--kube-context "${CONTEXT}")
KUBECTL=(kubectl); [ -n "${CONTEXT}" ] && KUBECTL=(kubectl --context "${CONTEXT}")

# Helm --set parses commas as list separators, so escape them in CIDR lists.
esc() { printf '%s' "$1" | sed 's/,/\\,/g'; }

say "🚀 installing DataWerx agent (routed mode) into ${CLUSTER_ID} (iface ${IFACE})"
HELM_ARGS=(
  upgrade --install dwx "${CHART}"
  "${KCTX[@]}"
  -n "${NAMESPACE}" --create-namespace
  --set clusterID="${CLUSTER_ID}"
  --set dataplane=routed
  --set overlayInterface="${IFACE}"
  --set-string localCIDRs="$(esc "${LOCAL_CIDRS}")"
)
[ -n "${IMAGE}" ] && HELM_ARGS+=(--set image.repository="${IMAGE%%:*}" --set image.tag="${IMAGE##*:}")
[ -n "${REMAP}" ] && HELM_ARGS+=(--set remapCIDR="${REMAP}")
helm "${HELM_ARGS[@]}"

say "⏳ waiting for the agent to roll out"
"${KUBECTL[@]}" -n "${NAMESPACE}" rollout status daemonset/dwx-datawerx-mesh --timeout=120s 2>/dev/null \
  || "${KUBECTL[@]}" -n "${NAMESPACE}" rollout status daemonset -l app.kubernetes.io/name=datawerx-mesh --timeout=120s

# The agent logs the overlay IP it discovered on IFACE; pull it from a pod's logs
# so the operator doesn't have to look it up.
say "🔍 discovering this cluster's overlay address from the agent logs"
POD=$("${KUBECTL[@]}" -n "${NAMESPACE}" get pods -l app.kubernetes.io/name=datawerx-mesh -o name | head -1 || true)
OVERLAY_IP=""
if [ -n "${POD}" ]; then
  OVERLAY_IP=$("${KUBECTL[@]}" -n "${NAMESPACE}" logs "${POD}" 2>/dev/null \
    | grep -o '"this node.s overlay address"[^}]*"ip":"[^"]*"' | grep -o '"ip":"[^"]*"' | tail -1 | sed 's/.*:"//;s/"//' || true)
fi

POD_CIDR="${LOCAL_CIDRS%%,*}"
SVC_CIDR="${LOCAL_CIDRS##*,}"

echo
echo "================================================================"
if [ -n "${OVERLAY_IP}" ]; then
  ok "🎉 Cluster '${CLUSTER_ID}' is joined. Its overlay address is: ${OVERLAY_IP}"
  echo
  echo "Apply this MeshPeer in EVERY OTHER cluster so they can reach ${CLUSTER_ID}:"
  echo "----------------------------------------------------------------"
  cat <<YAML
apiVersion: networking.datawerx.io/v1alpha1
kind: MeshPeer
metadata:
  name: ${CLUSTER_ID}
spec:
  clusterID: ${CLUSTER_ID}
  publicKey: ${CLUSTER_ID}
  endpoint: ${OVERLAY_IP}
  podCIDRs: ["${POD_CIDR}"]
  serviceCIDRs: ["${SVC_CIDR}"]
YAML
  echo "----------------------------------------------------------------"
else
  warn "Cluster '${CLUSTER_ID}' is joined, but the overlay address could not be"
  echo "auto-read from the agent logs. Find a node's ${IFACE} IP manually:"
  echo "    tailscale ip -4        # or: ip -4 addr show ${IFACE}"
  echo "and use it as spec.endpoint in the MeshPeer for '${CLUSTER_ID}' on peers."
fi
echo "================================================================"
echo
echo "Reminder: ensure node IP forwarding is on (sysctl net.ipv4.ip_forward=1)"
echo "and your overlay's ACLs permit the pod/service ranges above."
