#!/usr/bin/env bash
# Patch a cluster's CoreDNS to forward the clusterset.local zone to the
# datawerx-mesh-dns Service, so pods can resolve <svc>.<ns>.svc.clusterset.local.
#
# Usage: patch-coredns.sh <kube-context>
#
# The Corefile-rewrite logic is factored into pure functions so it can be unit
# tested without a cluster (see patch-coredns_test.sh); sourcing this file does
# not execute any kubectl calls.
set -euo pipefail

# corefile_strip_zone <corefile>
# Echo the Corefile with any existing clusterset.local server block removed. The
# block is flat, no nested braces, so we drop everything from the zone header
# through its closing brace.
corefile_strip_zone() {
  awk '
    /^clusterset\.local:53 \{/ {skip=1}
    skip {if ($0 ~ /^\}/) skip=0; next}
    {print}
  ' <<<"$1"
}

# corefile_is_current <corefile> <dns_ip>
# Succeeds iff the Corefile already forwards the clusterset.local zone to dns_ip.
# Idempotency is keyed on the IP, not just the zone name: the datawerx-mesh-dns
# ClusterIP changes whenever the Service is recreated
# Skipping merely because a clusterset.local block exists would leave
# CoreDNS forwarding to a dead IP.  The silent NXDOMAIN failure this guards.
corefile_is_current() {
  grep -qF "forward . $2" <<<"$1" && grep -q "clusterset.local" <<<"$1"
}

# corefile_ensure_zone <corefile> <dns_ip>
# Echo a Corefile that forwards clusterset.local to dns_ip, replacing any stale
# block. Zero side effects.
corefile_ensure_zone() {
  local cleaned
  cleaned=$(corefile_strip_zone "$1")
  printf '%s\nclusterset.local:53 {\n    errors\n    cache 5\n    forward . %s\n}\n' "${cleaned}" "$2"
}

main() {
  local ctx=${1:?usage: patch-coredns.sh <kube-context>}

  # Resolve the Service ClusterIP that fronts the per-node responders.
  local dns_ip
  dns_ip=$(kubectl --context "${ctx}" -n datawerx-system get svc datawerx-mesh-dns \
    -o jsonpath='{.spec.clusterIP}')
  if [[ -z "${dns_ip}" ]]; then
    echo "datawerx-mesh-dns Service has no ClusterIP yet" >&2
    exit 1
  fi

  echo "==> [${ctx}] forwarding clusterset.local -> ${dns_ip}"

  local corefile
  corefile=$(kubectl --context "${ctx}" -n kube-system get configmap coredns \
    -o jsonpath='{.data.Corefile}')

  if corefile_is_current "${corefile}" "${dns_ip}"; then
    echo "    clusterset.local -> ${dns_ip} already current"
  else
    corefile=$(corefile_ensure_zone "${corefile}" "${dns_ip}")
    # NOTE: this rewrites the configmap's .data to just Corefile,
    # which is correct for kind's CoreDNS but would drop a NodeHosts key on
    # distros that ship one. Fine for the e2e clusters this script targets.
    kubectl --context "${ctx}" -n kube-system create configmap coredns \
      --from-literal=Corefile="${corefile}" --dry-run=client -o yaml | \
      kubectl --context "${ctx}" -n kube-system apply -f -
  fi

  kubectl --context "${ctx}" -n kube-system rollout restart deployment/coredns
  kubectl --context "${ctx}" -n kube-system rollout status deployment/coredns --timeout=120s
}

# Only run main when executed directly, so tests can source the pure functions.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  main "$@"
fi
