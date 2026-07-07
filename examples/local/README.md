# Local demo: two clusters, one mesh, on your laptop

This is the fastest way to watch DataWerx Mesh actually work. One script creates
**two Kubernetes clusters** with [kind](https://kind.sigs.k8s.io), installs the
DataWerx agent into both with the shipped Helm chart, links them into a single
mesh, and then calls a Service in one cluster **by name** from the other.

Every step is the same thing you would run against real clusters. Only kind and
the local image build are laptop-specific.

```
   ┌───── kind: dwx-a ─────┐            ┌───── kind: dwx-b ─────┐
   │  agent DaemonSet      │  mesh /    │  agent DaemonSet      │
   │  clusterID: cluster-a │◄─ tunnel ─►│  clusterID: cluster-b │
   │  echo Service ────────┼─ exported ─┼──► echo.demo.svc.clusterset.local
   │  pod 10.244.0.0/16    │            │  pod 10.245.0.0/16    │
   └───────────────────────┘            └───────────────────────┘
      both clusters share the "kind" docker network as their underlay
```

## What it demonstrates

- **Cross-cluster connectivity** over a per-node WireGuard tunnel, brought up by
  the agent from a couple of `MeshPeer` objects.
- **Zero-friction peering** with `dwx mesh join` — each cluster mints a bundle
  and imports the other's, so no `MeshPeer` YAML is written by hand.
- **Service-by-name across clusters** using the standard Kubernetes
  Multi-Cluster Services API (`ServiceExport`) plus the `clusterset.local` DNS
  zone the agent serves.
- **The read-only operator surface** — `dwx mesh verify / snapshot / graph /
  reach / slo` — run against the live mesh.
- **Cross-cluster network policy** — a `MeshNetworkPolicy` dry-run and apply.

All of this is open-core. Nothing here touches a premium component or a SaaS
endpoint.

## Prerequisites

- [Docker](https://docs.docker.com/get-docker/) (running)
- [kind](https://kind.sigs.k8s.io/docs/user/quick-start/#installation)
- `kubectl`, `helm`
- `go` (to build the agent image and the `dwx` CLI from this tree)
- `wg` from `wireguard-tools` (used to generate the demo keypairs)
- `jq`, `curl`

The default data plane is WireGuard, which needs the **`wireguard` kernel
module** on your host. Most modern Linux kernels ship it; load it once with
`sudo modprobe wireguard`. Docker Desktop's Linux VM already has it.

**No WireGuard module?** Run the demo in bring-your-own-overlay mode with
`--routed`. The agent then programs host routes over the shared kind network
instead of owning a WireGuard device. The mesh contract you are watching —
`MeshPeer` convergence, service discovery, the by-name call — is identical.

Run `./mesh-demo.sh check` first and it tells you exactly what is missing.

## Run it

From the repo root:

```sh
# One shot: clusters -> agent -> mesh -> service call -> read-only tour -> policy
examples/local/mesh-demo.sh all

# ...or step by step:
examples/local/mesh-demo.sh check     # confirm prerequisites
examples/local/mesh-demo.sh up        # two clusters + agent + peering
examples/local/mesh-demo.sh service   # echo in A, called by name from B
examples/local/mesh-demo.sh tour      # verify / snapshot / graph / reach / slo
examples/local/mesh-demo.sh policy    # a cross-cluster MeshNetworkPolicy

examples/local/mesh-demo.sh down      # tear it all down
```

Bring-your-own-overlay instead of WireGuard:

```sh
examples/local/mesh-demo.sh --routed all
```

Every command is idempotent, so you can re-run any of them.

## What a successful run looks like

- `up` ends with `The two-cluster mesh is up`, and both `MeshPeer` objects reach
  `Connected`.
- `service` prints `hello from cluster A` — the echo pod in `dwx-a`, reached by
  name from `dwx-b`.
- `tour` shows `verify` passing on both clusters and a peer count of 1 each.

## How the pieces map to production

| Demo step | On your real clusters |
|---|---|
| `kind create cluster` x2 | Your existing clusters. |
| `docker build` + `kind load` | Pull the published image `ghcr.io/datawerx/datawerx-mesh/mesh-agent`. |
| `helm install ... --set fullnameOverride=dwx-mesh-agent` | Same chart. `fullnameOverride` names the DaemonSet what the `dwx` CLI expects, so `dwx mesh verify` needs no `--daemonset` flag. |
| `dwx mesh join export/import` | The same commands, swapping bundles between cluster admins — or author `MeshPeer` CRDs from GitOps. |
| Manual `EndpointExport` mirror | Your GitOps pipeline mirrors `EndpointExport`s between clusters. |
| `patch-coredns.sh` | Point cluster CoreDNS at the `clusterset.local` zone once. |

## Files

| File | Purpose |
|---|---|
| `mesh-demo.sh` | The driver. Read the header for the full command list. |
| `config.env` | Cluster names, CIDRs, image tag, mode. The one place to change settings. |
| `manifests/echo.yaml` | The echo Deployment, headless Service, and `ServiceExport`. |
| `manifests/meshnetworkpolicy.yaml` | The cross-cluster ingress policy used by `policy`. |

## Troubleshooting

- **Agent pod not ready in WireGuard mode** — almost always the `wireguard`
  kernel module. Run `sudo modprobe wireguard`, then `mesh-demo.sh up` again, or
  switch to `--routed`.
- **`service` call fails in `--routed` mode** — the cross-cluster data path needs
  a host forwarding rule the demo tries to add for you. If it could not (no
  sudo), add it with `sudo iptables -I DOCKER-USER -j ACCEPT` and re-run
  `service`. `down` removes the rule again.
- Deeper help: [`docs/troubleshooting.md`](../../docs/troubleshooting.md).
