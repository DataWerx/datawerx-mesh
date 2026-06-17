# Logging

The DataWerx Mesh agent emits structured logs using `logr` over Uber's `zap`.
Every line is a set of key/value fields, not a prose sentence, so you can grep,
`jq`, or ship them to Loki/Elastic and query by field. This page is the operator
guide: how to configure output, what the levels mean, and a few recipes.

## Quick configuration

Everything is tunable by environment variable (container-native) or the
equivalent `--zap-*` CLI flag. An explicit flag always wins over the env var, which wins over the built-in default.

| Env var | Flag | Default | Values |
|---|---|---|---|
| `DataWerx_LOG_LEVEL` | `--zap-log-level` | `info` | `error`, `warn`, `info`, `debug`, `trace`, or an integer N |
| `DataWerx_LOG_FORMAT` | `--zap-encoder` | `json` | `json`, `console` |
| `DataWerx_LOG_TIME` | `--zap-time-encoding` | `iso8601` | `iso8601`, `rfc3339`, `rfc3339nano`, `millis`, `nano`, `epoch` |
| `DataWerx_LOG_STACKTRACE` | `--zap-stacktrace-level` | `error` | `info`, `warn`, `error`, `none` |
| `DataWerx_LOG_CALLER` | — | `true` | `true`, `false` |
| `DataWerx_LOG_DEVELOPMENT` | `--zap-devel` | `false` | `true`, `false` |

The defaults are tuned for production: machine-parseable JSON, human-readable
ISO8601 timestamps, source-location (`caller`) annotation on, and stacktraces
only on `Error`.

## Reading a line

```json
{"level":"info","ts":"2026-06-15T03:19:37.482Z","logger":"meshpeer","caller":"controllers/meshpeer_controller.go:259","msg":"finalized MeshPeer; peer torn down","meshpeer":"/cluster-eu"}
```

- `logger` — the component that emitted the line (`meshpeer`, `wireguard`,
  `nat`, `topology-syncer`, `dnsserver`, ...). Think of it as Serilog's
  `SourceContext`. Filter with `jq 'select(.logger=="wireguard")'`.
- `caller` — the `file:line` that logged it.
- `msg` — a short, **stable** event name (no interpolated values).
- everything else — structured context (`meshpeer`, `peer`, `cidr`, `family`,
  `revision`, `clusterID`, ...).

The very first line on boot is the build banner, the canonical thing to quote in
a bug report:

```json
{"level":"info","logger":"setup","msg":"starting DataWerx Mesh agent","version":"v1.4.2","goVersion":"go1.24.0","platform":"linux/amd64"}
```

## Verbosity levels

Verbosity encodes **intent**, so the default `info` stream is a clean feed of
things that actually changed. In Serilog terms:

| `DataWerx_LOG_LEVEL` | logr | Shows | Use it to |
|---|---|---|---|
| `info` (default) | `V(0)` | Lifecycle and **state-change** events: a peer programmed for the first time, a key rotation, a link created, a stale peer pruned, a degradation (`Info` + an `err` field). | Run in production. A healthy peer is quiet between changes. |
| `debug` | `V(1)` | Adds steady-state, per-reconcile confirmations (`peer configured`, `clusterset NAT synced`, `meshpeer reconciled`). | Watch a peer converge, confirm the reconcile loop is doing work. |
| `trace` | `V(2)` | Adds per-item, high-frequency detail. | Deep-dive a specific data-path issue. |

`logr` has no **Warn** level: an expected, recoverable problem is logged at
`info` with an `"err"` field (e.g. `ip6tables unavailable; IPv6 NAT disabled`).
A line at `level":"error"` is an *unexpected* failure and carries a stacktrace.

> Raising the level is safe and reversible — set `DataWerx_LOG_LEVEL=debug` on
> one DaemonSet pod, reproduce, then revert.

## Secrets

WireGuard keys, SSO tokens, and private keys are **never** logged in full. Keys
are truncated to a short prefix (`abc12345…`) for correlation. If you are adding
a log line, keep it that way — use `logging.ShortKey` (or `shortKey` in
`pkg/wg`).

## Recipes

Human-readable local tail:

```sh
kubectl -n datawerx-system set env ds/datawerx-mesh \
  DataWerx_LOG_FORMAT=console DataWerx_LOG_LEVEL=debug
kubectl -n datawerx-system logs -f ds/datawerx-mesh
```

Just one component, as JSON, with `jq`:

```sh
kubectl -n datawerx-system logs ds/datawerx-mesh \
  | jq -c 'select(.logger=="wireguard")'
```

Everything a single peer did (note `msg` is stable, so grep on it):

```sh
kubectl -n datawerx-system logs ds/datawerx-mesh \
  | jq -c 'select(.meshpeer=="/cluster-eu")'
```

Turn stacktraces off (e.g. when a known-noisy error floods them):

```sh
DataWerx_LOG_STACKTRACE=none
```

## For developers

Construction lives in [`pkg/logging`](../pkg/logging); the agent builds its
logger in `pkg/agent`. When adding logs, follow these conventions:
pick the level by intent, pass context as key/value pairs (reuse the canonical
keys), never interpolate values into `msg`, and never log secrets.
