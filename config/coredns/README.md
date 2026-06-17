# Wiring cluster CoreDNS to the clusterset.local zone

Each DataWerx Mesh agent pod runs an authoritative DNS responder for the
`clusterset.local` zone (default `:5353`), answering
`<svc>.<ns>.svc.clusterset.local` from the `ServiceImport` objects the import
controller produces. To make pods resolve those names, point the cluster's
CoreDNS at it.

## 1. Expose the responders

Apply the Service that fronts the per-node responders:

```sh
kubectl apply -f datawerx-mesh-dns-service.yaml
```

This gives a stable ClusterIP (`datawerx-mesh-dns.datawerx-system`) on port 53
that load-balances across the agent pods.

## 2. Forward the zone from CoreDNS

Add a server block to the cluster CoreDNS `Corefile` (usually the `coredns`
ConfigMap in `kube-system`). Forward to the Service's **ClusterIP** (substitute
the real value from `kubectl -n datawerx-system get svc datawerx-mesh-dns`):

```caddy
clusterset.local:53 {
    errors
    cache 5
    forward . <DATAWERX_MESH_DNS_CLUSTERIP>
}
```

Then roll CoreDNS:

```sh
kubectl -n kube-system rollout restart deployment/coredns
```

## 3. Verify

From any pod in any mesh cluster:

```sh
nslookup payments.prod.svc.clusterset.local
# -> the imported service's ClusterSetIP (e.g. 241.x.x.x)
```

## Notes

- **ClusterSetIP services** resolve to the virtual ClusterSetIP. Reachability of
  that IP across the mesh requires the ClusterSetIP DNAT/load-balancing data
  plane (tracked separately); DNS resolution itself works today.
- **Headless services** resolve to the union of backing pod IPs published by
  every exporting cluster (via `EndpointExport`), so a headless name works
  end-to-end across the mesh today — those pod IPs are already routable over
  WireGuard. A headless service with no ready endpoints returns NXDOMAIN, the
  correct empty-backend behavior.
- A native CoreDNS plugin (compiled into a custom CoreDNS image, via Submariner
  Lighthouse) can replace the forward-based wiring later; the responder protocol
  is plain DNS, so either integration works.
