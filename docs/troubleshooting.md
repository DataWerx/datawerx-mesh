# Troubleshooting

Start with the health check, then match your symptom below.

```sh
dwx mesh verify --context <ctx>
kubectl -n datawerx-system logs ds/dwx-mesh-agent --tail=100
kubectl get meshpeer -o wide          # PHASE column: Pending | Connected | Error
```

## A MeshPeer is stuck `Pending`

The peer is declared but not yet reachable.

- **WireGuard mode:** the `spec.endpoint` (`host:port`) must be reachable from
  this node, and the **reciprocal** `MeshPeer` must exist in the other cluster
  with matching public keys. WireGuard is silent until both sides agree.
- Confirm the `wireguard` kernel module is loaded and the agent has `NET_ADMIN`.
- Check `dwx_meshpeer_last_handshake_timestamp_seconds` — `0` means no handshake yet.

## A MeshPeer is in a state of `Error`

Almost always an **overlapping CIDR**: the peer advertises a range that collides
with one of your `localCIDRs`, so routing it would hijack local traffic. The
`status.message` says which. Fix by giving clusters distinct CIDRs, or enable
remap on **every** cluster:

```sh
--set remapCIDR=true   # or a custom pool like 172.16.0.0/12
```

## A name won't resolve (`clusterset.local`)

1. Is the Service exported? `kubectl get serviceexport -A` (status `Valid`).
2. Did the `EndpointExport`s reach this cluster? `kubectl get endpointexport -A`.
   In the free tier your **GitOps mirror** must copy them between clusters.
3. Did a `ServiceImport` get built? `kubectl get serviceimport -A`.
4. Is CoreDNS forwarding the zone to the responder? See
   [config/coredns/README.md](../config/coredns/README.md).

## Name resolves but traffic doesn't connect

- **Headless:** the remote **pod IPs** must be routable — i.e. the underlying
  MeshPeer is `Connected` and (routed mode) the route is installed.
- **ClusterSetIP:** check the NAT data plane —
  `dwx_clusterset_nat_syncs_total{result="error"}` and the agent log line
  `clusterset NAT synced`. The agent needs the `iptable_nat` module.

## Connection opens but large transfers hang (MTU)

Classic symptom: the TCP handshake and small requests work, but anything large
(`kubectl logs`, image pulls, big API responses) stalls. The mesh device has a
smaller MTU than the pod interfaces, and Path MTU Discovery is frequently broken
across clusters (ICMP "fragmentation needed" gets dropped), so full-size segments
are silently lost.

- DataWerx clamps cross-cluster TCP MSS to the path MTU automatically in
  WireGuard mode (the `DWX-MESH-MSS` chain in the `mangle` table; see the agent
  log line `cross-cluster TCP MSS clamp enabled`). Confirm it is present:
  `iptables -t mangle -S DWX-MESH-MSS`.
- If you disabled it (`DataWerx_MESH_MSS_CLAMP_DISABLE`) or run **routed/BYO**
  mode (where the overlay owns MTU), ensure your overlay clamps MSS or set a
  correct device MTU. You can also lower the mesh device MTU with
  `DataWerx_WG_MTU`.

## Routed (BYO overlay) mode

- The agent pod must run where it can see the overlay device (real nodes, not
  nested kind). Confirm: `kubectl -n datawerx-system logs ds/dwx-mesh-agent | grep "peer data plane ready"` → `mode":"routed"`.
- Node IP forwarding must be on: `sysctl net.ipv4.ip_forward` → `1`.
- MeshNetworkPolicy needs `DataWerx_OVERLAY_INTERFACE` set in routed mode; without
  it, the firewall is skipped (logged at startup) and routing still works.
- Your overlay's ACLs must allow the pod/service ranges, not just node IPs.

## Agent crashlooping

```sh
kubectl -n datawerx-system describe pod -l app.kubernetes.io/name=datawerx-mesh
kubectl -n datawerx-system logs ds/dwx-mesh-agent --previous
```

Common causes: missing `NET_ADMIN`/privileges, `wireguard` or `iptable_nat`
module not loaded on the node, or a bad `DataWerx_*` value (the agent fails fast
with a clear message at startup).

## Still stuck?

Open an issue with `dwx mesh verify` output, the agent logs, and your `MeshPeer` /
`ServiceExport` YAML (redact keys). See [CONTRIBUTING.md](../CONTRIBUTING.md).
