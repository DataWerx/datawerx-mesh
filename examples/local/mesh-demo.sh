#!/usr/bin/env bash
# ============================================================================
# examples/local/mesh-demo.sh — a complete, self-contained DataWerx Mesh demo
# on your own machine.
#
# It stands up TWO Kubernetes clusters with kind, installs the DataWerx agent
# into both with the shipped Helm chart, links them into one mesh, and then
# calls a Service in cluster A by name from cluster B. Along the way it exercises
# the free read-only surface (verify / snapshot / graph / reach / slo) and a
# cross-cluster MeshNetworkPolicy — all open-core, nothing premium.
#
# Everything here maps 1:1 onto real clusters. The only thing that is specific
# to a laptop is kind and the local image build.
#
# Usage:
#   examples/local/mesh-demo.sh [--routed] <command>
#
# Commands:
#   check      Verify the host has everything needed (and can run the chosen mode)
#   up         Create both clusters, install the agent, and form the mesh
#   service    Deploy an echo Service in A and call it by name from B
#   tour       Run verify / snapshot / graph / reach / slo against the mesh
#   policy     Demonstrate a cross-cluster MeshNetworkPolicy (dry-run + apply)
#   all        up -> service -> tour -> policy  (the full guided demo)
#   down       Tear everything down
#   help       Show this help
#
# Flags:
#   --routed        Use bring-your-own-overlay mode (no WireGuard kernel module).
#   --wireguard     Force the default WireGuard data plane.
#
# Every command is idempotent, so re-running is safe.
# ============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
STATE_DIR="${SCRIPT_DIR}/.state"
CHART="${REPO_ROOT}/charts/datawerx-mesh"

# shellcheck source=/dev/null
source "${SCRIPT_DIR}/config.env"

# --- console styling ---------------------------------------------------------
if [[ -t 1 && -z "${NO_COLOR:-}" ]]; then
  _reset=$'\e[0m'; _blue=$'\e[34m'; _green=$'\e[32m'; _yellow=$'\e[33m'
  _red=$'\e[31m'; _bold=$'\e[1m'; _dim=$'\e[2m'
else
  _reset=; _blue=; _green=; _yellow=; _red=; _bold=; _dim=
fi
say()  { printf '%s==>%s %s\n' "${_blue}${_bold}" "${_reset}" "$*"; }
step() { printf '\n%s%s── %s ──%s\n' "${_bold}" "${_blue}" "$*" "${_reset}"; }
ok()   { printf '%s✅ %s%s\n' "${_green}" "$*" "${_reset}"; }
warn() { printf '%s⚠️  %s%s\n' "${_yellow}" "$*" "${_reset}"; }
die()  { printf '%s❌ %s%s\n' "${_red}" "$*" "${_reset}" >&2; }
dim()  { printf '%s%s%s\n' "${_dim}" "$*" "${_reset}"; }

CTX_A="kind-${CLUSTER_A}"
CTX_B="kind-${CLUSTER_B}"
DWX="${STATE_DIR}/dwx"

# --- small helpers -----------------------------------------------------------

# Escape commas so helm --set/--set-string does not read a CIDR list as a list.
esc() { printf '%s' "$1" | sed 's/,/\\,/g'; }

# The control-plane node's IP on the shared kind docker network. That is the
# reachable WireGuard endpoint (and, in routed mode, the overlay next hop).
node_ip() { docker inspect -f '{{.NetworkSettings.Networks.kind.IPAddress}}' "${1}-control-plane"; }

# Run iptables on the HOST (routed mode only), using sudo when not already root.
host_iptables() {
  if [ "$(id -u)" -eq 0 ]; then iptables "$@"; else sudo iptables "$@"; fi
}

# ============================================================================
# Preflight
# ============================================================================

wg_module_state() {
  if lsmod 2>/dev/null | grep -qw wireguard; then echo loaded; return; fi
  if modinfo wireguard >/dev/null 2>&1; then echo available; return; fi
  if compgen -G "/lib/modules/$(uname -r)/kernel/drivers/net/wireguard*" >/dev/null 2>&1; then
    echo available; return
  fi
  echo absent
}

preflight() {
  local fail=0

  step "Preflight (${MODE} mode)"

  local need=(docker kind kubectl helm go wg jq curl)
  for t in "${need[@]}"; do
    if command -v "$t" >/dev/null 2>&1; then
      ok "found ${t}"
    else
      die "missing ${t} — install it and re-run"; fail=1
    fi
  done

  if docker info >/dev/null 2>&1; then
    ok "docker daemon reachable"
  else
    die "docker daemon not reachable — is Docker running?"; fail=1
  fi

  if [ "${MODE}" = wireguard ]; then
    case "$(wg_module_state)" in
      loaded)
        ok "wireguard kernel module loaded" ;;
      available)
        warn "wireguard module present but not loaded."
        dim "   The agent will try to load it in-cluster (it has SYS_MODULE)."
        dim "   If the agent fails to bring up WireGuard, run:  sudo modprobe wireguard"
        dim "   or re-run this demo with --routed (no kernel module needed)." ;;
      absent)
        die "wireguard kernel module not available on this host."
        dim "   Install it (e.g. apt-get install wireguard), run 'sudo modprobe wireguard',"
        dim "   or re-run with --routed to use bring-your-own-overlay mode instead."
        fail=1 ;;
    esac
  else
    ok "routed mode selected — no WireGuard kernel module required"
    dim "   The demo will configure cross-cluster forwarding on the kind network."
    dim "   Adding the host forwarding rule may prompt for sudo once."
  fi

  return "${fail}"
}

# ============================================================================
# Build / keys
# ============================================================================

build_dwx() {
  mkdir -p "${STATE_DIR}"
  say "building the dwx CLI from source"
  ( cd "${REPO_ROOT}" && CGO_ENABLED=0 go build -o "${DWX}" ./cmd/dwx )
}

need_dwx() { [ -x "${DWX}" ] || build_dwx; }

# Load (or generate once and persist) a cluster's WireGuard keypair. Persisting
# keeps identities stable across re-runs so the reciprocal peering stays valid.
load_keys() {
  local name=$1 priv_var=$2 pub_var=$3
  mkdir -p "${STATE_DIR}"
  local pf="${STATE_DIR}/${name}.key" bf="${STATE_DIR}/${name}.pub"
  if [ ! -s "${pf}" ] || [ ! -s "${bf}" ]; then
    wg genkey > "${pf}"
    wg pubkey < "${pf}" > "${bf}"
    chmod 600 "${pf}"
  fi
  printf -v "${priv_var}" '%s' "$(cat "${pf}")"
  printf -v "${pub_var}"  '%s' "$(cat "${bf}")"
}

# ============================================================================
# Cluster + agent bring-up
# ============================================================================

kind_config() {
  cat <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  podSubnet: "$1"
  serviceSubnet: "$2"
EOF
}

create_cluster() {
  local name=$1 pod=$2 svc=$3
  if kind get clusters 2>/dev/null | grep -qx "${name}"; then
    ok "kind cluster ${name} already exists"
  else
    say "creating kind cluster ${name} (pod ${pod}, svc ${svc})"
    kind_config "${pod}" "${svc}" | kind create cluster --name "${name}" --config -
  fi
  say "loading ${IMAGE} into ${name}"
  kind load docker-image "${IMAGE}" --name "${name}"
}

# In routed mode the "overlay" is the shared kind docker network. Two plain kind
# clusters do not forward each other's pod/service CIDRs the way a real overlay
# gateway would, so reproduce that: per-node forwarding + a host rule that lets
# docker forward inter-node CIDR frames. WireGuard mode needs none of this.
setup_routed_forwarding() {
  say "enabling cross-cluster forwarding (routed mode)"
  for node in "${CLUSTER_A}-control-plane" "${CLUSTER_B}-control-plane"; do
    docker exec "${node}" sysctl -w net.ipv4.ip_forward=1 >/dev/null
    docker exec "${node}" sysctl -w net.ipv4.conf.all.rp_filter=0 >/dev/null
    docker exec "${node}" sysctl -w net.ipv4.conf.default.rp_filter=0 >/dev/null
    docker exec "${node}" iptables -P FORWARD ACCEPT
  done
  if host_iptables -C DOCKER-USER -j ACCEPT 2>/dev/null; then
    ok "host DOCKER-USER ACCEPT rule already present"
  elif host_iptables -I DOCKER-USER -j ACCEPT 2>/dev/null; then
    ok "added host DOCKER-USER ACCEPT rule (removed again by 'down')"
  else
    warn "could not add the host DOCKER-USER ACCEPT rule (needs root/sudo)."
    warn "The mesh still forms, but the cross-cluster data path needs it. Add it with:"
    warn "   sudo iptables -I DOCKER-USER -j ACCEPT"
  fi
}

teardown_routed_forwarding() {
  while host_iptables -C DOCKER-USER -j ACCEPT 2>/dev/null; do
    say "removing host DOCKER-USER ACCEPT rule"
    host_iptables -D DOCKER-USER -j ACCEPT
  done
}

helm_install() {
  local ctx=$1 id=$2 priv=$3 local_cidrs=$4
  say "[${ctx}] installing the DataWerx agent with Helm"
  local args=(
    upgrade --install "${RELEASE}" "${CHART}"
    --kube-context "${ctx}"
    -n "${NAMESPACE}" --create-namespace
    --set fullnameOverride="${FULLNAME}"
    --set dnsService.name="${DNS_SVC_NAME}"
    --set clusterID="${id}"
    --set-string localCIDRs="$(esc "${local_cidrs}")"
    --set image.repository="${IMAGE%%:*}"
    --set-string image.tag="${IMAGE##*:}"
    --set image.pullPolicy=IfNotPresent
    --set-string wgListenPort="${WG_PORT}"
    --set-string wireguard.privateKey.value="${priv}"
  )
  if [ "${MODE}" = routed ]; then
    args+=(--set dataplane=routed)
  fi
  helm "${args[@]}"
}

diagnose_agent() {
  local ctx=$1
  echo "----- pods (${ctx}) -----"
  kubectl --context "${ctx}" -n "${NAMESPACE}" get pods -o wide || true
  echo "----- recent events -----"
  kubectl --context "${ctx}" -n "${NAMESPACE}" get events --sort-by=.lastTimestamp | tail -15 || true
  echo "----- agent logs -----"
  kubectl --context "${ctx}" -n "${NAMESPACE}" logs -l app.kubernetes.io/name=datawerx-mesh --tail=60 || true
}

wait_rollout() {
  local ctx=$1
  say "[${ctx}] waiting for the agent DaemonSet to become ready"
  if kubectl --context "${ctx}" -n "${NAMESPACE}" rollout status \
      daemonset/"${FULLNAME}" --timeout="${ROLLOUT_TIMEOUT}s"; then
    ok "[${ctx}] agent is ready"
  else
    die "[${ctx}] agent DaemonSet did not become ready"
    diagnose_agent "${ctx}"
    if [ "${MODE}" = wireguard ]; then
      warn "In WireGuard mode this usually means the wireguard kernel module could"
      warn "not be loaded in the cluster. Try 'sudo modprobe wireguard' on the host,"
      warn "or re-run the demo with --routed."
    fi
    return 1
  fi
}

# Mint each cluster's join bundle and import it into the other, forming the mesh
# with no hand-written MeshPeer CRDs (the zero-friction join flow, design 0006).
form_mesh() {
  local ip_a ip_b
  ip_a="$(node_ip "${CLUSTER_A}")"
  ip_b="$(node_ip "${CLUSTER_B}")"
  say "endpoints: ${ID_A}=${ip_a}:${WG_PORT}  ${ID_B}=${ip_b}:${WG_PORT}"

  local tok
  say "[${CTX_A}] importing a join bundle for ${ID_B}"
  tok="$("${DWX}" mesh join export --cluster-id "${ID_B}" --public-key "${PUB_B}" \
        --endpoint "${ip_b}:${WG_PORT}" --pod-cidrs "${POD_B}" --service-cidrs "${SVC_B}" 2>/dev/null)"
  "${DWX}" mesh join import --context "${CTX_A}" --bundle "${tok}"

  say "[${CTX_B}] importing a join bundle for ${ID_A}"
  tok="$("${DWX}" mesh join export --cluster-id "${ID_A}" --public-key "${PUB_A}" \
        --endpoint "${ip_a}:${WG_PORT}" --pod-cidrs "${POD_A}" --service-cidrs "${SVC_A}" 2>/dev/null)"
  "${DWX}" mesh join import --context "${CTX_B}" --bundle "${tok}"
}

wait_connected() {
  local ctx=$1 peer=$2 deadline=$((SECONDS + CONVERGE_TIMEOUT)) phase
  say "[${ctx}] waiting for MeshPeer ${peer} to converge"
  while [ "${SECONDS}" -lt "${deadline}" ]; do
    phase="$(kubectl --context "${ctx}" get meshpeer "${peer}" \
      -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    case "${phase}" in
      Connected) ok "[${ctx}] MeshPeer ${peer} is Connected"; return 0 ;;
      Error)
        warn "[${ctx}] MeshPeer ${peer} is Error: $(kubectl --context "${ctx}" \
          get meshpeer "${peer}" -o jsonpath='{.status.message}' 2>/dev/null)" ;;
    esac
    sleep 3
  done
  die "[${ctx}] MeshPeer ${peer} did not reach Connected within ${CONVERGE_TIMEOUT}s (last phase: ${phase:-none})"
  return 1
}

cmd_up() {
  preflight || { die "preflight failed — fix the above and re-run"; exit 1; }
  build_dwx
  load_keys "${CLUSTER_A}" PRIV_A PUB_A
  load_keys "${CLUSTER_B}" PRIV_B PUB_B

  step "Building the agent image"
  say "docker build ${IMAGE}"
  docker build -t "${IMAGE}" "${REPO_ROOT}"

  step "Creating clusters"
  create_cluster "${CLUSTER_A}" "${POD_A}" "${SVC_A}"
  create_cluster "${CLUSTER_B}" "${POD_B}" "${SVC_B}"
  [ "${MODE}" = routed ] && setup_routed_forwarding

  step "Installing the agent"
  helm_install "${CTX_A}" "${ID_A}" "${PRIV_A}" "${POD_A},${SVC_A}"
  helm_install "${CTX_B}" "${ID_B}" "${PRIV_B}" "${POD_B},${SVC_B}"
  wait_rollout "${CTX_A}"
  wait_rollout "${CTX_B}"

  step "Forming the mesh"
  form_mesh
  wait_connected "${CTX_A}" "${ID_B}"
  wait_connected "${CTX_B}" "${ID_A}"

  echo
  ok "The two-cluster mesh is up (${MODE} mode)."
  dim "Next:  ${0##*/} service   # call a Service across the mesh"
  dim "       ${0##*/} tour      # inspect the mesh with the read-only CLI"
  dim "       ${0##*/} policy    # try a cross-cluster network policy"
}

# ============================================================================
# Workload: a Service in A, called by name from B
# ============================================================================

ensure_ns() {
  kubectl --context "$1" create namespace "${DEMO_NS}" \
    --dry-run=client -o yaml | kubectl --context "$1" apply -f - >/dev/null
}

cmd_service() {
  need_dwx
  step "Deploying an echo Service in ${CTX_A}"
  ensure_ns "${CTX_A}"
  kubectl --context "${CTX_A}" apply -f "${SCRIPT_DIR}/manifests/echo.yaml"
  kubectl --context "${CTX_A}" -n "${DEMO_NS}" rollout status deployment/echo --timeout=90s

  say "[${CTX_A}] waiting for the agent to publish an EndpointExport"
  for _ in $(seq 1 30); do
    [ -n "$(kubectl --context "${CTX_A}" -n "${DEMO_NS}" get endpointexports -o name 2>/dev/null)" ] && break
    sleep 1
  done
  kubectl --context "${CTX_A}" -n "${DEMO_NS}" get endpointexports || true

  # Free tier: your GitOps pipeline mirrors EndpointExports between clusters.
  # Here we copy them by hand so the demo needs no pipeline.
  step "Mirroring the export into ${CTX_B}"
  ensure_ns "${CTX_B}"
  kubectl --context "${CTX_B}" -n "${DEMO_NS}" delete endpointexport --all --ignore-not-found >/dev/null
  kubectl --context "${CTX_A}" -n "${DEMO_NS}" get endpointexports -o yaml \
    | kubectl --context "${CTX_B}" -n "${DEMO_NS}" apply -f -

  step "Pointing ${CTX_B} CoreDNS at the clusterset.local zone"
  "${REPO_ROOT}/hack/e2e/patch-coredns.sh" "${CTX_B}"

  step "Calling echo.${DEMO_NS}.svc.clusterset.local from ${CTX_B}"
  kubectl --context "${CTX_B}" -n "${DEMO_NS}" delete pod probe --ignore-not-found >/dev/null 2>&1 || true
  kubectl --context "${CTX_B}" -n "${DEMO_NS}" run probe --restart=Never \
    --image=curlimages/curl:8.7.1 --command -- \
    sh -c 'for _ in $(seq 1 30); do curl -sS -m 3 http://echo.demo.svc.clusterset.local:8080 && exit 0; sleep 2; done; echo "could not reach echo across the mesh" >&2; exit 1' \
    >/dev/null
  local phase=""
  while :; do
    phase="$(kubectl --context "${CTX_B}" -n "${DEMO_NS}" get pod probe \
      -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    [ "${phase}" = Succeeded ] || [ "${phase}" = Failed ] && break
    sleep 1
  done
  echo "------------------------------------------------------------"
  kubectl --context "${CTX_B}" -n "${DEMO_NS}" logs probe || true
  echo "------------------------------------------------------------"
  kubectl --context "${CTX_B}" -n "${DEMO_NS}" delete pod probe --ignore-not-found >/dev/null 2>&1 || true

  if [ "${phase}" = Succeeded ]; then
    ok "Cross-cluster call worked: a Service in ${CLUSTER_A}, reached by name from ${CLUSTER_B}."
  else
    die "The cross-cluster call failed. See the output above and docs/troubleshooting.md."
    if [ "${MODE}" = routed ]; then
      warn "In routed mode the data path needs the host DOCKER-USER ACCEPT rule."
      warn "Re-run '${0##*/} up' (it adds the rule) or add it manually with:"
      warn "   sudo iptables -I DOCKER-USER -j ACCEPT"
    fi
    return 1
  fi
}

# ============================================================================
# The read-only tour
# ============================================================================

cmd_tour() {
  need_dwx
  local ctx
  for ctx in "${CTX_A}" "${CTX_B}"; do
    step "verify (${ctx})"
    "${DWX}" mesh verify --context "${ctx}" || warn "verify reported problems on ${ctx}"

    step "snapshot keys (${ctx})"
    dim "The full JSON is the versioned machine-readable state contract."
    "${DWX}" mesh snapshot --context "${ctx}" | jq 'keys'

    step "reach (${ctx})"
    "${DWX}" mesh reach --context "${ctx}" || true

    step "slo (${ctx})"
    "${DWX}" mesh slo --context "${ctx}" || true
  done

  step "graph (${CTX_A}, mermaid) — paste into any Markdown viewer"
  "${DWX}" mesh graph --context "${CTX_A}" --format mermaid
  ok "Read-only tour complete."
}

# ============================================================================
# Cross-cluster network policy
# ============================================================================

cmd_policy() {
  need_dwx
  local pol="${SCRIPT_DIR}/manifests/meshnetworkpolicy.yaml"

  step "Dry-run the policy's impact BEFORE applying it"
  dim "policy --dry-run composes the firewall + topology to show what changes."
  "${DWX}" mesh policy --dry-run -f "${pol}" --context "${CTX_A}" \
    --local-cidrs "${POD_A},${SVC_A}" || true

  step "Apply the MeshNetworkPolicy in ${CTX_A}"
  kubectl --context "${CTX_A}" apply -f "${pol}"

  say "waiting for the policy to be programmed"
  local deadline=$((SECONDS + 60)) phase=""
  while [ "${SECONDS}" -lt "${deadline}" ]; do
    phase="$(kubectl --context "${CTX_A}" get meshnetworkpolicy allow-cluster-b-to-echo \
      -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    [ "${phase}" = Ready ] && break
    [ "${phase}" = Error ] && break
    sleep 2
  done
  kubectl --context "${CTX_A}" get meshnetworkpolicy

  if [ "${phase}" = Ready ]; then
    ok "MeshNetworkPolicy is Ready: only cluster-b may reach echo on TCP 8080; all other mesh ingress to that range is now default-deny."
  else
    warn "MeshNetworkPolicy phase is '${phase:-none}' — see 'kubectl get meshnetworkpolicy -o yaml'."
  fi
  dim "Remove it again with:  kubectl --context ${CTX_A} delete meshnetworkpolicy allow-cluster-b-to-echo"
}

# ============================================================================
# Teardown
# ============================================================================

cmd_down() {
  local c
  for c in "${CLUSTER_A}" "${CLUSTER_B}"; do
    if kind get clusters 2>/dev/null | grep -qx "${c}"; then
      say "deleting kind cluster ${c}"
      kind delete cluster --name "${c}"
    fi
  done
  teardown_routed_forwarding || true
  ok "Teardown complete."
  dim "Generated keys are kept in ${STATE_DIR#"${REPO_ROOT}/"}; delete it to reset identities."
}

# ============================================================================
# Dispatch
# ============================================================================

# Print the header comment block (between the two ==== rules), stripped of the
# leading "# ", as the help text.
usage() {
  awk '
    /^# ={10,}/ { c++; sub(/^# ?/, ""); print; if (c==2) exit; next }
    c>=1        { sub(/^# ?/, ""); print }
  ' "${BASH_SOURCE[0]}"
}

main() {
  local cmd=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --routed)    MODE=routed;    shift ;;
      --wireguard) MODE=wireguard; shift ;;
      -h|--help|help) usage; exit 0 ;;
      check|up|service|tour|policy|all|down) cmd="$1"; shift ;;
      *) die "unknown argument: $1"; echo; usage; exit 2 ;;
    esac
  done
  [ -n "${cmd}" ] || { usage; exit 2; }

  case "${cmd}" in
    check)   preflight ;;
    up)      cmd_up ;;
    service) cmd_service ;;
    tour)    cmd_tour ;;
    policy)  cmd_policy ;;
    all)     cmd_up; cmd_service; cmd_tour; cmd_policy ;;
    down)    cmd_down ;;
  esac
}

main "$@"
