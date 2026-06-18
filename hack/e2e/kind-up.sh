#!/usr/bin/env bash
# Bring up a two-cluster DataWerx Mesh on kind and wire a reciprocal WireGuard
# peering, ready for the e2e suite 
#
# `go test -tags e2e ./test/e2e/...`
#
# Prerequisites on the host:
#   - docker, kind, kubectl
#   - wireguard-tools (`wg`) to generate keys
#   - the `wireguard` kernel module loaded creating a data plane
#
# Both kind clusters share docker's "kind" network, so each cluster's nodes are
# reachable from the other by their docker IP — that is the WireGuard endpoint.
#
# IMPORTANT: the two clusters are created with DISTINCT pod/service CIDRs so the
# overlap guard does not refuse the routes.  Otherwise kind defaults both to the
# same ranges. This lets headless pod IPs route across the mesh today, before
# the ClusterSetIP DNAT work lands.
set -euo pipefail

# Shared console styling (say/ok/warn/die + color setup).
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/../lib.sh"

CLUSTER_A=${CLUSTER_A:-dwx-a}
CLUSTER_B=${CLUSTER_B:-dwx-b}
IMAGE=${IMAGE:-datawerx/mesh-agent:dev}
REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)

# Distinct CIDRs per cluster (default). With OVERLAP=1 both clusters use the
# same ranges and the agent's overlap remap is enabled.
POD_A=10.244.0.0/16; SVC_A=10.96.0.0/16
POD_B=10.245.0.0/16; SVC_B=10.97.0.0/16
REMAP=""
if [ "${OVERLAP:-0}" = "1" ]; then
  POD_B="${POD_A}"; SVC_B="${SVC_A}"
  # The remap pool MUST NOT overlap the docker network kind runs on. kind's
  # "kind" bridge is allocated from docker's default pool (172.16.0.0/12 — e.g.
  # 172.18.0.0/16 on the GitHub runner, where docker0 already owns 172.17/16),
  # so the production default of 172.16.0.0/12 makes the agent carve a virtual
  # /16 that lands ON the underlay and route it into WireGuard — blackholing
  # node↔gateway traffic and the API server. Use RFC-6598 shared space
  # (100.64.0.0/10), which docker never allocates, so the virtual ranges can
  # never collide with the kind underlay. Must match ovlRemapPool in
  # test/e2e/overlap_test.go.
  REMAP="${REMAP_CIDR:-100.64.0.0/10}"
  say "🔀 OVERLAP mode: both clusters on ${POD_A} / ${SVC_A}, remap pool ${REMAP}"
fi

# ROUTED mode: instead of DataWerx owning a WireGuard device, run the agent in
# bring-your-own-overlay mode. The shared docker "kind" network is the overlay.
# Each cluster's nodes are already reachable from the other by docker IP. The
# agent just installs host routes via the remote node IP.
DPLANE=""
if [ "${ROUTED:-0}" = "1" ]; then
  DPLANE="routed"
  say "🛣️  ROUTED mode: BYO overlay = shared kind docker network (no WireGuard device)"
fi

say "🔧 building agent image ${IMAGE}"
docker build -t "${IMAGE}" "${REPO_ROOT}"

kind_config() {
  local pod=$1 svc=$2
  cat <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  podSubnet: "${pod}"
  serviceSubnet: "${svc}"
EOF
}

create_cluster() {
  local name=$1 pod=$2 svc=$3
  if ! kind get clusters | grep -qx "${name}"; then
    say "🚀 creating kind cluster ${name} (pod ${pod}, svc ${svc})"
    kind_config "${pod}" "${svc}" | kind create cluster --name "${name}" --config -
  fi
  say "📦 loading ${IMAGE} into ${name}"
  kind load docker-image "${IMAGE}" --name "${name}"
}

create_cluster "${CLUSTER_A}" "${POD_A}" "${SVC_A}"
create_cluster "${CLUSTER_B}" "${POD_B}" "${SVC_B}"

# Control-plane node docker IP on the shared "kind" network == WG endpoint host.
# Index the "kind" network explicitly: ranging over all attached networks would
# concatenate IPs (e.g. "172.18.0.2172.19.0.3") with no separator if a node is
# on a second docker network, yielding a silently broken WireGuard endpoint.
node_ip() {
  docker inspect -f '{{.NetworkSettings.Networks.kind.IPAddress}}' "${1}-control-plane"
}
IP_A=$(node_ip "${CLUSTER_A}")
IP_B=$(node_ip "${CLUSTER_B}")
say "📡 ${CLUSTER_A} endpoint ${IP_A}:51820 / ${CLUSTER_B} endpoint ${IP_B}:51820"

# In ROUTED mode the "overlay" is the shared docker network. A real overlay
# (Tailscale/NetBird) plus the gateway node forwards each cluster's pod/service
# CIDRs; two independent kind clusters do not do that for each other by default.
# Reproduce that gateway-forwarding behavior so the data path actually works:
#   - per node: enable forwarding + relax reverse-path filtering + open FORWARD,
#   - on the host: let docker forward inter-node pod/service-CIDR frames
#     (DOCKER-USER is traversed before docker's default-drop rules).
# This is the kind stand-in for what an overlay's gateway provides in production.
if [ "${ROUTED:-0}" = "1" ]; then
  say "🔁 [routed] enabling cross-cluster forwarding (kind stand-in for overlay gateways)"
  # These are load-bearing for the data path. Do NOT swallow failures with
  # `|| true`: a silently-failed setup looks healthy until the e2e suite fails
  # mysteriously. `set -e` aborts the bring-up on any real error instead.
  for node in "${CLUSTER_A}-control-plane" "${CLUSTER_B}-control-plane"; do
    docker exec "${node}" sysctl -w net.ipv4.ip_forward=1 >/dev/null
    docker exec "${node}" sysctl -w net.ipv4.conf.all.rp_filter=0 >/dev/null
    docker exec "${node}" sysctl -w net.ipv4.conf.default.rp_filter=0 >/dev/null
    docker exec "${node}" iptables -P FORWARD ACCEPT
  done
  # DOCKER-USER is traversed before docker's default-drop, so an ACCEPT here lets
  # inter-node pod/service-CIDR frames forward. Insert it only once — re-running
  # kind-up.sh must not stack duplicate rules.  When done, everything can
  # be cleaned up by kind-down.sh.
  host_iptables() {
    if command -v sudo >/dev/null 2>&1 && [ "$(id -u)" -ne 0 ]; then
      sudo iptables "$@"
    else
      iptables "$@"
    fi
  }
  if ! host_iptables -C DOCKER-USER -j ACCEPT 2>/dev/null; then
    host_iptables -I DOCKER-USER -j ACCEPT
  fi
fi

# Deterministic WireGuard keypairs so the reciprocal MeshPeers reference the correct public keys.
PRIV_A=$(wg genkey); PUB_A=$(echo "${PRIV_A}" | wg pubkey)
PRIV_B=$(wg genkey); PUB_B=$(echo "${PRIV_B}" | wg pubkey)

deploy() {
  local ctx=$1 cluster_id=$2 priv=$3 local_cidrs=$4
  say "📥 [${ctx}] installing CRDs + agent"
  kubectl --context "kind-${ctx}" apply -f "${REPO_ROOT}/config/crd/"
  kubectl --context "kind-${ctx}" apply -f "${REPO_ROOT}/deploy/agent.yaml"

  kubectl --context "kind-${ctx}" -n datawerx-system create secret generic dwx-wg \
    --from-literal=private-key="${priv}" --dry-run=client -o yaml | kubectl --context "kind-${ctx}" apply -f -
  # DataWerx_LOG_LEVEL=debug surfaces the steady-state confirmations that the e2e assertions 
  # and failure diagnostics grep for; the default Info floor suppresses them.
  kubectl --context "kind-${ctx}" -n datawerx-system set env daemonset/dwx-mesh-agent \
    DataWerx_CLUSTER_ID="${cluster_id}" DataWerx_LOCAL_CIDRS="${local_cidrs}" \
    DataWerx_LOG_LEVEL=debug \
    ${REMAP:+DataWerx_REMAP_CIDR="${REMAP}"} \
    ${DPLANE:+DataWerx_DATAPLANE="${DPLANE}"}
  # Strategic merge instead of --type merge: a JSON merge patch replaces the whole
  # containers array and would drop the required `image` field. Strategic merge
  # keys list items by name, so it merges the WG-key env into the existing
  # container and preserves image/args.
  kubectl --context "kind-${ctx}" -n datawerx-system patch daemonset dwx-mesh-agent --type strategic -p '{
    "spec":{"template":{"spec":{"containers":[{"name":"agent","env":[
      {"name":"DataWerx_WG_PRIVATE_KEY","valueFrom":{"secretKeyRef":{"name":"dwx-wg","key":"private-key"}}}
    ]}]}}}}'

  kubectl --context "kind-${ctx}" apply -f "${REPO_ROOT}/config/coredns/datawerx-mesh-dns-service.yaml"
  "${REPO_ROOT}/hack/e2e/patch-coredns.sh" "kind-${ctx}"

  kubectl --context "kind-${ctx}" -n datawerx-system rollout status daemonset/dwx-mesh-agent --timeout=120s
}

deploy "${CLUSTER_A}" cluster-a "${PRIV_A}" "${POD_A},${SVC_A}"
deploy "${CLUSTER_B}" cluster-b "${PRIV_B}" "${POD_B},${SVC_B}"

# Reciprocal peering: each cluster gets a MeshPeer describing the *other* cluster's
# key, endpoint, and (distinct) CIDRs.
meshpeer() {
  local ctx=$1 id=$2 pub=$3 endpoint=$4 pod=$5 svc=$6
  kubectl --context "kind-${ctx}" apply -f - <<EOF
apiVersion: networking.datawerx.io/v1alpha1
kind: MeshPeer
metadata:
  name: ${id}
spec:
  clusterID: ${id}
  publicKey: "${pub}"
  endpoint: "${endpoint}"
  podCIDRs: ["${pod}"]
  serviceCIDRs: ["${svc}"]
EOF
}

if [[ "${JOIN:-0}" == "1" ]]; then
  # Form the mesh with `dwxctl join` instead of hand-authored MeshPeers (0006).
  say "🔗 wiring reciprocal MeshPeers via dwxctl join"
  CTX_A="kind-${CLUSTER_A}" CTX_B="kind-${CLUSTER_B}" \
    ID_A=cluster-a ID_B=cluster-b \
    PUB_A="${PUB_A}" PUB_B="${PUB_B}" \
    EP_A="${IP_A}:51820" EP_B="${IP_B}:51820" \
    POD_A="${POD_A}" POD_B="${POD_B}" SVC_A="${SVC_A}" SVC_B="${SVC_B}" \
    bash "${REPO_ROOT}/hack/e2e/join.sh"
else
  say "🔗 wiring reciprocal MeshPeers"
  meshpeer "${CLUSTER_A}" cluster-b "${PUB_B}" "${IP_B}:51820" "${POD_B}" "${SVC_B}"
  meshpeer "${CLUSTER_B}" cluster-a "${PUB_A}" "${IP_A}:51820" "${POD_A}" "${SVC_A}"
fi

ok "🎉 Two-cluster mesh is up."
cat <<EOF

Run the e2e suite with:

  E2E_CONTEXT_A=kind-${CLUSTER_A} E2E_CONTEXT_B=kind-${CLUSTER_B} \\
    go test -tags e2e -timeout 30m ./test/e2e/...

To also run the join e2e (TestJoinFormsMesh), export the peer inputs:

  E2E_PUB_A=${PUB_A} E2E_EP_A=${IP_A}:51820 E2E_POD_A=${POD_A} E2E_SVC_A=${SVC_A} \\
  E2E_PUB_B=${PUB_B} E2E_EP_B=${IP_B}:51820 E2E_POD_B=${POD_B} E2E_SVC_B=${SVC_B} \\
    go test -tags e2e -run TestJoinFormsMesh -timeout 30m ./test/e2e/...

Tear down with: hack/e2e/kind-down.sh
EOF
