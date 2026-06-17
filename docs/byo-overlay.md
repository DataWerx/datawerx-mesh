# Bring your own overlay (routed mode)

Already running **Tailscale, NetBird, Cilium, plain WireGuard, or a cloud VPN**?
Keep it! Don't rip it out! In *routed* mode DataWerx creates no tunnel of its own — it
assumes your overlay already gives nodes mutual reachability and adds **only the
Kubernetes layer** on top: host routes for remote pod/service ranges, plus
cross-cluster DNS, cluster-set VIPs, overlap remap, and network policy.

## How this is complementary

Overlay VPNs connect **machines** brilliantly. They do not implement the
Kubernetes [MCS API](cross-cluster-services.md), they do not give you a stable cluster-set
VIP that load-balances across clusters, and they do no serve `*.clusterset.local`, or reconcile
overlapping pod CIDRs. DataWerx does exactly those tasks while using your overlay as the
wire. Your overlay keeps owning encryption, NAT traversal, and identity.

## Enable/Disable routed mode

| Setting | Helm value | Env | Meaning |
|---|---|---|---|
| Mode | `dataplane: routed` | `DataWerx_DATAPLANE=routed` | DataWerx Mesh won't own a WireGuard device; it will program routes instead. |
| Overlay device | `overlayInterface: tailscale0` | `DataWerx_OVERLAY_INTERFACE` | Output device for routes (e.g. `tailscale0`, `wt0`, `wg0`). Optional; the kernel resolves the next-hop if unset — but **required to enable MeshNetworkPolicy** in routed mode. |

In routed mode the `MeshPeer` fields mean:

- `spec.endpoint` — the remote gateway's **overlay IP** (e.g. its Tailscale
  `100.x` address). A `host:port` is tolerated; the port is ignored.
- `spec.publicKey` — any stable unique ID (it keys per-peer route bookkeeping;
  it is not a WireGuard key here).
- `spec.podCIDRs` / `serviceCIDRs` — the remote ranges to route, same as standalone.

## Try it locally (no external overlay)

Two kind clusters share docker's network, so they're already mutually reachable.  That makes it a perfect stand-in, 'demo' overlay:

```sh
ROUTED=1 hack/e2e/kind-up.sh        # routed mode; no WireGuard device is created
# prove there's no DataWerx tunnel — the docker network is the transport:
docker exec dwx-a-control-plane ip link show dwx-mesh0   # → does not exist
hack/e2e/kind-down.sh
```

## On a real tailnet (Tailscale)

Run `tailscale up` on each cluster's gateway node, enable forwarding
(`sysctl -w net.ipv4.ip_forward=1`), then join each cluster with the helper:

```sh
hack/demo/routed-join.sh --cluster-id cluster-a --iface tailscale0 \
  --local-cidrs 10.244.0.0/16,10.96.0.0/16 --image you/mesh-agent:tag
```

It installs the agent in routed mode, reads this node's overlay IP from the
agent's logs, and prints the exact `MeshPeer` to apply in the *other* cluster.
Run it per cluster, cross-apply the printed manifests, then export Services as
usual ([cross-cluster services](cross-cluster-services.md)).

## Requirements

- Your cluster **nodes** are on the overlay, so the agent's host-network pod sees
  e.g. `tailscale0`. This works on real nodes (k3s, kubeadm); it does **not**
  work with nested kind, where the overlay lives on the host, not in the node.
- Node IP forwarding on (`net.ipv4.ip_forward=1`) — most CNIs set this already.
- Your overlay's ACLs permit the pod/service ranges, not just the node IPs.

## Standalone vs routed

| | Standalone, uses `wireguard` | Routed, BYO overlay |
|---|---|---|
| No existing network | ✅ simplest | — |
| Already run Tailscale/NetBird/Cilium | redundant tunnels | ✅ layer on top |
| Owns encryption/keys | DataWerx + `wireguard` | your overlay |

Same CRDs, DNS, and behavior either way — only who owns the transport changes.
