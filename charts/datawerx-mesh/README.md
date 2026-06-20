# datawerx-mesh Helm chart

Deploys the DataWerx Mesh node agent as a DaemonSet, with the CRDs, RBAC, and
`clusterset.local` DNS responder Service it needs.

## Prerequisites

- Kubernetes 1.26+
- Each node must allow the agent to program the kernel: the `wireguard` module
  must be available, and the pod must be granted `NET_ADMIN` (the default
  `securityContext` runs privileged for the simplest installation.  In production this would be tightened.
- For real, long-lived (non-ephemeral) peering, a WireGuard private key per cluster, stored
  in a Secret.  Otherwise a new key public/private would constantly be recreated.

## Install

From the published chart on GitHub Container Registry (GHCR), no clone needed:

```sh
helm install dwx oci://ghcr.io/datawerx/datawerx-mesh/charts/datawerx-mesh \
  --namespace datawerx-system --create-namespace \
  --set clusterID=cluster-a \
  --set wireguard.privateKey.existingSecret=dwx-wg \
  --set wireguard.privateKey.existingSecretKey=private-key \
  --set-string localCIDRs="10.244.0.0/16\,10.96.0.0/16"
```

Or from a checkout for development: `helm install dwx ./charts/datawerx-mesh …`.

CRDs in the `crds/` directory are installed automatically on first install.  Helm does not upgrade or delete CRDs — apply `config/crd/` manually when the schema changes.

## Premium Tier

```sh
helm install dwx ./charts/datawerx-mesh -n datawerx-system \
  --set premium.saasEndpoint=https://mesh.datawerx.io \
  --set premium.ssoTokenSecret.name=dwx-sso --set premium.ssoTokenSecret.key=token
```

## Key values

| Key | Description | Default |
|-----|-------------|---------|
| `image.repository` / `image.tag` | Agent image (tag defaults to chart `appVersion`) | `ghcr.io/datawerx/datawerx-mesh/mesh-agent` / chart appVersion |
| `clusterID` | This cluster's mesh ID (`DataWerx_CLUSTER_ID`) | `""` |
| `localCIDRs` | Local pod/service ranges for overlap detection | `10.244.0.0/16,10.96.0.0/16` |
| `clusterSetCIDR` | ClusterSetIP allocation range | `""` (→ `241.0.0.0/8`) |
| `clusterSetCIDR6` | IPv6 ClusterSetIP allocation range (dual-stack); empty disables | `""` |
| `wgInterface` | Managed link name | `""` (→ `dwx-mesh0`) |
| `dnsBind` | clusterset.local responder listen address | `:5353` |
| `wireguard.privateKey.existingSecret` / `…SecretKey` | Secret with the WG private key | `""` / `private-key` |
| `wireguard.privateKey.value` | Inline WG key (chart creates a Secret) | `""` |
| `premium.saasEndpoint` | Enables the premium tier | `""` |
| `premium.ssoTokenSecret.name` / `.key` | Enterprise SSO token Secret | `""` / `token` |
| `dnsService.enabled` | Create the clusterset.local Service | `true` |
| `metrics.service.enabled` | Expose a metrics Service | `false` |
| `metrics.serviceMonitor.enabled` | Create a Prometheus ServiceMonitor | `false` |
| `metrics.dashboard.enabled` | Ship the Grafana dashboard as a sidecar-discoverable ConfigMap | `false` |
| `serviceAccount.create` / `rbac.create` | Manage SA + RBAC | `true` |
| `securityContext` | Container security context | privileged + `NET_ADMIN`,`SYS_MODULE` |

See `values.yaml` for the full set.

## Wiring CoreDNS

After install, forward the `clusterset.local` zone from cluster CoreDNS to the
`<release>-datawerx-mesh-dns` Service — see `config/coredns/README.md`.
