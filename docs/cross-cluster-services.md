# Cross-cluster services

DataWerx implements the Kubernetes [Multi-Cluster Services
API](https://github.com/kubernetes/enhancements/tree/master/keps/sig-multicluster/1645-multi-cluster-services-api)
(MCS): you mark a Service for export, and it becomes reachable by a stable name
from every cluster in the mesh.

## Export a service

Create a `ServiceExport` with the **same name/namespace** as the Service:

```yaml
apiVersion: multicluster.x-k8s.io/v1alpha1
kind: ServiceExport
metadata:
  name: payments
  namespace: prod
```

The agent validates it against the real Service and publishes an
`EndpointExport` — the broker-less wire format describing this cluster's
contribution (type, ports, reachable IPs).

## Make exports visible to other clusters

Other clusters consume `EndpointExport` objects. How they arrive is your choice:

- **Free tier:** your **GitOps pipeline** mirrors `EndpointExport`s between
  clusters (they're plain CRDs — sync them like anything else).
- **Premium tier:** a managed control plane materializes them for you.

Either way, the import side is identical: each cluster builds a `ServiceImport`
from the `EndpointExport`s it can see.

## Resolve it by name

Imported services answer in the `clusterset.local` zone:

```
payments.prod.svc.clusterset.local
```

Point cluster CoreDNS at the DataWerx responder so that zone resolves — see
**[config/coredns/README.md](../config/coredns/README.md)** (the
`hack/e2e/patch-coredns.sh` script does this for kind).

## Two service types

| Type | Resolves to | Path |
|---|---|---|
| **Headless** (`clusterIP: None`) | the exporting clusters' **routable pod IPs** | direct, no NAT |
| **ClusterSetIP** (default) | a single stable **virtual IP** per service | DNAT + load-balanced across all exporting clusters' pods |

The cluster-set VIP is allocated by a **pure, deterministic function** of the
service set, so every cluster derives the *same* IP with zero coordination. It's
drawn from a reserved range (`241.0.0.0/8` by default; configurable, and
dual-stack capable — see [configuration](configuration.md)).

## Withdraw

Delete the `ServiceExport` (and, in the free tier, the mirrored
`EndpointExport`s). The `ServiceImport`, its VIP, DNS records, and NAT rules are
torn down automatically across the mesh.
