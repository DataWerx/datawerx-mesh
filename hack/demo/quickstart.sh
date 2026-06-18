#!/usr/bin/env bash
# This is the workload half of the demo. Run hack/e2e/kind-up.sh first
# to stand up the two-cluster mesh, then run this to deploy an echo Service in
# cluster 'A', export it across the mesh, and call it by name from cluster 'B'.
#
# Prereqs are docker, kind, and kubectl:
# this shells out to `kubectl` and the repo's own patch-coredns.sh. 
# It's safe to re-run since every step is idempotent.
set -euo pipefail

# Shared console styling (say/ok/die + color setup).
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/../lib.sh"

CLUSTER_A=${CLUSTER_A:-dwx-a}
CLUSTER_B=${CLUSTER_B:-dwx-b}
CTX_A="kind-${CLUSTER_A}"
CTX_B="kind-${CLUSTER_B}"
NS=demo
REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)

ns() { # ensure namespace exists on a context
  kubectl --context "$1" create namespace "${NS}" --dry-run=client -o yaml \
    | kubectl --context "$1" apply -f - >/dev/null
}

say "🚀 [${CTX_A}] deploy a headless echo Service in namespace ${NS}"
ns "${CTX_A}"
# http-echo:1.0 is a distroless image whose entrypoint is the binary, so the
# flags go in `args` (appended to the entrypoint) — NOT in `command`, which
# would replace the entrypoint and fail with "exec: -listen=:8080: not found".
kubectl --context "${CTX_A}" -n "${NS}" apply -f - <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: echo
  namespace: demo
  labels: { app: echo }
spec:
  replicas: 1
  selector: { matchLabels: { app: echo } }
  template:
    metadata: { labels: { app: echo } }
    spec:
      containers:
        - name: http-echo
          image: hashicorp/http-echo:1.0
          args: ["-listen=:8080", "-text=hi-from-a"]
          ports: [{ containerPort: 8080 }]
---
apiVersion: v1
kind: Service
metadata:
  name: echo
  namespace: demo
spec:
  clusterIP: None          # headless: pod IPs propagate across the mesh
  selector: { app: echo }
  ports: [{ port: 8080, targetPort: 8080 }]
YAML
kubectl --context "${CTX_A}" -n "${NS}" rollout status deployment/echo --timeout=90s

say "📤 [${CTX_A}] export the Service with the standard MCS ServiceExport"
kubectl --context "${CTX_A}" apply -f - <<'YAML'
apiVersion: multicluster.x-k8s.io/v1alpha1
kind: ServiceExport
metadata: { name: echo, namespace: demo }
YAML

say "⏳ [${CTX_A}] wait for the controller to generate the EndpointExport"
for _ in $(seq 1 30); do
  [ -n "$(kubectl --context "${CTX_A}" -n "${NS}" get endpointexports -o name 2>/dev/null)" ] && break
  sleep 1
done
kubectl --context "${CTX_A}" -n "${NS}" get endpointexports

# Free tier: your GitOps pipeline mirrors EndpointExports between clusters.
# Here we copy them by hand. Delete any prior mirror first so re-runs don't
# trip over the server-assigned metadata carried in `get -o yaml`.
say "🔄 [${CTX_B}] mirror EndpointExports from ${CTX_A} (GitOps does this in prod)"
ns "${CTX_B}"
kubectl --context "${CTX_B}" -n "${NS}" delete endpointexport --all --ignore-not-found >/dev/null
kubectl --context "${CTX_A}" -n "${NS}" get endpointexports -o yaml \
  | kubectl --context "${CTX_B}" -n "${NS}" apply -f -

say "🌐 [${CTX_B}] point CoreDNS at the mesh DNS responder"
"${REPO_ROOT}/hack/e2e/patch-coredns.sh" "${CTX_B}"

say "🔗 [${CTX_B}] calling echo.${NS}.svc.clusterset.local by name from cluster B"
kubectl --context "${CTX_B}" -n "${NS}" delete pod probe --ignore-not-found >/dev/null 2>&1 || true
# Retry inside the pod. The ServiceImport and DNS take a moment to converge.
kubectl --context "${CTX_B}" -n "${NS}" run probe --restart=Never \
  --image=curlimages/curl:8.7.1 --command -- \
  sh -c 'for _ in $(seq 1 30); do curl -sS -m 3 http://echo.demo.svc.clusterset.local:8080 && exit 0; sleep 2; done; echo "could not reach echo across the mesh" >&2; exit 1' \
  >/dev/null
kubectl --context "${CTX_B}" -n "${NS}" wait --for=condition=Ready pod/probe --timeout=30s >/dev/null 2>&1 || true
# Block until the pod finishes, then surface its output and real exit status.
while :; do
  phase=$(kubectl --context "${CTX_B}" -n "${NS}" get pod probe -o jsonpath='{.status.phase}' 2>/dev/null || true)
  [ "${phase}" = "Succeeded" ] || [ "${phase}" = "Failed" ] && break
  sleep 1
done
echo
echo "------------------------------------------------------------"
kubectl --context "${CTX_B}" -n "${NS}" logs probe
echo "------------------------------------------------------------"
kubectl --context "${CTX_B}" -n "${NS}" delete pod probe --ignore-not-found >/dev/null 2>&1 || true

if [ "${phase}" = "Succeeded" ]; then
  ok "Cross-cluster call worked: a Service in cluster A, reached by name from cluster B."
else
  die "The cross-cluster call failed — see the output above and docs/troubleshooting.md."
  exit 1
fi
