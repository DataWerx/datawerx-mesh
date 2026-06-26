# Configuration

Everything you can tune, in one place. Set values via the **Helm chart**
(our recommendation) or directly as **environment variables** on the agent.

## Core settings

| Helm value | Env var | Default | Purpose |
|---|---|---|---|
| `clusterID` | `DataWerx_CLUSTER_ID` | _(unset)_ | This cluster's mesh ID. Stamps exports; strongly recommended. |
| `localCIDRs` | `DataWerx_LOCAL_CIDRS` | _(none)_ | This cluster's pod+service ranges (comma-separated), for overlap detection. |
| `dataplane` | `DataWerx_DATAPLANE` | `wireguard` | `wireguard` (own a device) or `routed` ([BYO overlay](byo-overlay.md)). |
| `dnsBind` | `DataWerx_DNS_BIND` | `:5353` | Listen address of the `clusterset.local` responder. |

## WireGuard mode

| Helm value | Env var | Default | Purpose |
|---|---|---|---|
| `wireguard.privateKey.existingSecret` | `DataWerx_WG_PRIVATE_KEY` | _(ephemeral)_ | Node WireGuard key (base64), normally a projected Secret. Ephemeral keys don't survive restart. |
| `wgInterface` | `DataWerx_WG_INTERFACE` | `dwx-mesh0` | Managed link name. |
| `wgListenPort` | `DataWerx_WG_LISTEN_PORT` | `51820` | UDP listen port. |
| `wgKeepalive` | `DataWerx_WG_KEEPALIVE` | `25s` | Persistent-keepalive for NAT traversal. |

## Routed (BYO overlay) mode

| Helm value | Env var | Default | Purpose |
|---|---|---|---|
| `overlayInterface` | `DataWerx_OVERLAY_INTERFACE` | _(kernel resolves)_ | Overlay device for route output (e.g. `tailscale0`). Required to enable MeshNetworkPolicy in routed mode. |

## Cluster-set VIPs & overlapping CIDRs

| Helm value | Env var | Default | Purpose |
|---|---|---|---|
| `clusterSetCIDR` | `DataWerx_CLUSTERSET_CIDR` | `241.0.0.0/8` | Range cluster-set VIPs are allocated from. |
| `clusterSetCIDR6` | `DataWerx_CLUSTERSET_CIDR6` | _(disabled)_ | IPv6 VIP range for dual-stack services. |
| `remapCIDR` | `DataWerx_REMAP_CIDR` | _(off)_ | Enable overlapping-CIDR remap. `true` uses `172.16.0.0/12`; or set a custom pool. Enable on **all** mesh clusters. |
| `remapBackend` | `DataWerx_REMAP_BACKEND` | `iptables` | Remap data plane: `iptables` (free) or `ebpf` (premium build). |

## Active mesh probing

Off by default. Enable on every agent to observe true application-layer
reachability between clusters, feeding the `dwx mesh slo` verdict. See
**[Active mesh probing](active-probing.md)**.

| Env var | Default | Purpose |
|---|---|---|
| `DataWerx_PROBE_ENABLE` | `false` | Enable the per-node responder and prober. |
| `DataWerx_PROBE_RESPONDER_ADDR` | `:9998` | Responder listen address. |
| `DataWerx_PROBE_PORT` | _(responder port)_ | Port the prober dials on each peer's endpoint host. |
| `DataWerx_PROBE_INTERVAL` | `30s` | Probe cadence. |
| `DataWerx_PROBE_TIMEOUT` | `5s` | Per-dial timeout. |

## Premium tier (optional)

Setting `premium.saasEndpoint` switches the agent to the managed control plane;
unset, it runs the free self-hosted path. Both are additive — see
[ROADMAP.md](../ROADMAP.md).

| Helm value | Env var | Purpose |
|---|---|---|
| `premium.saasEndpoint` | `DataWerx_SAAS_ENDPOINT` | Managed control-plane URL (presence → premium tier). |
| `premium.ssoTokenSecret.name` | `DataWerx_ENTERPRISE_SSO_TOKEN` | OIDC/SSO machine token (Secret reference). |
| — | `DataWerx_SYNC_INTERVAL` (`30s`) | Premium topology poll cadence. |

## Logging

Structured (`logr`/`zap`) output, configurable by env var or the equivalent
`--zap-*` flag (the flag wins). See **[Logging](logging.md)** for the field
conventions, verbosity levels, and ready-to-paste recipes.

| Env var | Default | Purpose |
|---|---|---|
| `DataWerx_LOG_LEVEL` | `info` | `error`/`warn`/`info`/`debug` (reveals `V(1)`)/`trace` (`V(2)`), or an integer N. |
| `DataWerx_LOG_FORMAT` | `json` | `json` for machines, `console` for humans. |
| `DataWerx_LOG_TIME` | `iso8601` | `iso8601`/`rfc3339`/`rfc3339nano`/`millis`/`nano`/`epoch`. |
| `DataWerx_LOG_STACKTRACE` | `error` | Level at/above which stacktraces attach; `none` disables. |
| `DataWerx_LOG_CALLER` | `true` | Annotate each line with the emitting `file:line`. |
| `DataWerx_LOG_DEVELOPMENT` | `false` | Console + debug developer defaults (≈ `--zap-devel`). |

## Health check (`dwx`)

```sh
dwx mesh verify --context <kube-context>   # MeshPeer phases, exports/imports, DNS, data plane
```

A fast, read-only sanity check for an installed mesh — handy in CI and on call.

## Metrics

The agent serves Prometheus metrics on `--metrics-bind-address` (default `:8080`,
path `/metrics`). Enable scraping and the bundled Grafana dashboard via the chart:

```sh
helm upgrade dwx ./charts/datawerx-mesh -n datawerx-system \
  --set metrics.service.enabled=true \
  --set metrics.serviceMonitor.enabled=true \   # needs the Prometheus Operator
  --set metrics.dashboard.enabled=true          # ships a Grafana-sidecar ConfigMap
```

Key series: `dwx_meshpeers{phase}`,
`dwx_meshpeer_last_handshake_timestamp_seconds`, `dwx_serviceimports{type}`,
`dwx_serviceexports{valid}`, `dwx_dns_queries_total{rcode}`,
`dwx_clusterset_nat_syncs_total{result}`, `dwx_remap_active_entries`.

See the full chart reference in
[charts/datawerx-mesh/README.md](../charts/datawerx-mesh/README.md).

## Permissions

The data plane programs the host network, so the agent needs `NET_ADMIN`.  In WireGuard mode, the `wireguard` kernel module. The chart sets this up, but for production, prefer the minimal capability set over `privileged: true`.
