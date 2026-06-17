# DataWerx Mesh — end-to-end tests

A multi-cluster suite that stands up **two real clusters**, wires a reciprocal
WireGuard peering, and proves cross-cluster connectivity + service discovery.

It is gated behind the `e2e` build tag, so the default `go test ./...` stays
hermetic and root-free. The data plane is real (WireGuard kernel device +
routes), so this needs root/`NET_ADMIN` and the `wireguard` module — run it on a
suitable host or in the dedicated CI job (`.github/workflows/e2e.yml`,
manual/nightly), never in the default unit-test pipeline.

## What it asserts

1. **`TestMeshPeersConnected`** — every `MeshPeer` on both clusters reaches
   `Connected` (the tunnels are up).
2. **`TestCrossClusterHeadlessDNS`** — exports a headless `echo` Service in
   cluster A, mirrors the resulting `EndpointExport`s into B (simulating the
   GitOps pipeline), waits for B's import controller to converge a
   `ServiceImport`, then runs a Job in B that `curl`s
   `echo.dwx-e2e.svc.clusterset.local:8080`. The Job succeeding proves DNS
   resolution **and** L3 connectivity across the mesh in one shot.
3. **`TestServiceImportTornDownOnUnexport`** — removing the export (and its
   mirrored `EndpointExport`) removes the `ServiceImport` in B.

4. **`TestOverlapRemapConnectivity`** (overlap mode only) — both clusters share
   the same CIDRs; B reaches A's Service via its **virtual** IP through the M3
   NETMAP remap. This is the end-to-end gate for the bidirectional NAT.

## Run it

```sh
# --- Distinct-CIDR mode (connectivity + headless/ClusterSetIP DNS) ---
hack/e2e/kind-up.sh
E2E_CONTEXT_A=kind-dwx-a E2E_CONTEXT_B=kind-dwx-b \
  go test -tags e2e -timeout 30m ./test/e2e/...     # overlap test self-skips
hack/e2e/kind-down.sh

# --- Overlap mode (M3 remap gate): same CIDRs on both clusters + remap ---
OVERLAP=1 hack/e2e/kind-up.sh
E2E_CONTEXT_A=kind-dwx-a E2E_CONTEXT_B=kind-dwx-b E2E_OVERLAP=1 \
  go test -tags e2e -timeout 30m -run 'TestMeshPeersConnected|TestOverlapRemapConnectivity' ./test/e2e/...
hack/e2e/kind-down.sh

# --- Routed / BYO-overlay mode: no WireGuard device; routes ride the shared
#     kind docker network (the "overlay"). The full suite must still pass. ---
ROUTED=1 hack/e2e/kind-up.sh
E2E_CONTEXT_A=kind-dwx-a E2E_CONTEXT_B=kind-dwx-b \
  go test -tags e2e -timeout 30m ./test/e2e/...
hack/e2e/kind-down.sh
```

All three modes (distinct, overlap, routed) run nightly in CI
(`.github/workflows/e2e.yml`, a matrix).

## Configuration

| Env | Purpose | Default |
|-----|---------|---------|
| `E2E_KUBECONFIG_A` / `E2E_CONTEXT_A` | cluster A kubeconfig / context | ambient / `kind-dwx-a` |
| `E2E_KUBECONFIG_B` / `E2E_CONTEXT_B` | cluster B kubeconfig / context | ambient / `kind-dwx-b` |

## Notes & current limits

- The suite exercises **both** service types: **headless** (resolves to routable
  pod IPs, no NAT) via `TestCrossClusterHeadlessDNS`, and **ClusterSetIP**
  (resolves to a virtual IP that the `pkg/nat` data plane DNATs + load-balances
  to the exporting clusters' real service IPs) via `TestCrossClusterClusterSetIP`.
- The two clusters are created with distinct pod/service CIDRs so the overlap
  guard does not refuse the cross-cluster routes.
- Mirroring `EndpointExport`s between clusters is done by the test itself to keep
  it self-contained; in a real free-tier deployment that is the GitOps
  pipeline's job, and in premium the SaaS syncer's.
