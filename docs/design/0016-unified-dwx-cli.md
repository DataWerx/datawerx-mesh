# 0016 — Unified `dwx` CLI

Status: accepted (OSS half implemented)
Supersedes the standalone `dwxctl`, `dwx-mcp`, and `dwx-signal` binaries as the
primary entry points.

## Problem

The DataWerx suite had grown a scatter of `dwx-*` command-line tools —
`dwxctl`, `dwx-mcp`, `dwx-signal`, with `dwx-edge` only ever a *subcommand* of
`dwxctl` — and the premium repos aspire to more (`dwx-coordinator`,
`dwx-remote`). Each is a separate `package main` and a separate install target.
For a user the surface is fragmented: different binary names, no shared help, no
single thing to learn. We want the experience the `aws` CLI gives — **one
binary that fronts many loosely-coupled services** — without giving up the
open-core build seam that keeps premium code out of the free binary.

## Decision

One umbrella binary, **`dwx`**, with `aws`-style **service-noun namespacing**:

```
dwx mesh   verify | snapshot | diagnose | graph | reach | slo | policy | join
dwx edge   enroll | profile | list
dwx signal "<question>"
dwx mcp                       # read-only MCP stdio server
dwx version
```

`mesh`/`edge`/`signal`/`mcp` are the free services. `dwx version` reports the
single build version (`pkg/logging.Version`, ldflags-stamped).

### Premium is a runtime plugin, not a linked dependency

This is the load-bearing part. The open-core `dwx` never imports premium code.
Instead it resolves an **unknown command `X` to a `dwx-X` executable on `PATH`**
(the `kubectl`-plugin / `git`-subcommand model) and `exec`s it, forwarding
stdio and the exit code. So premium ships `dwx-cloud` (the `datawerx-admin`
SaaS surface) or the operator-side `dwx-coordinator` independently, and
`dwx cloud login` "just works" once installed — while `datawerx-mesh` still
builds clean-room with zero access to any premium module. This is the
`ControlPlaneClient`-seam philosophy (design 0013/0014) applied to the CLI.

Built-in service names take precedence over plugins; only unrecognized commands
fall through to PATH lookup.

### The agent is not part of this

`cmd/manager` (`dwx-manager`, the in-cluster DaemonSet) stays a separate
distroless `CGO_ENABLED=0` binary. The CLI *drives* the mesh; it is not the
mesh, and the agent must not carry CLI/AI/HTTP-client surface.

## Structure

The three former `package main` command sets were lifted into importable
packages, each exposing a single entry point:

| Package | Entry point | Was |
|---|---|---|
| `internal/cli/mesh` | `Run(prog string, args []string) int`, `RunEdge(...)` | `cmd/dwxctl` |
| `internal/cli/mcp` | `Run(prog string, args []string) int` | `cmd/dwx-mcp` |
| `internal/cli/signalcli` | `Run(prog string, args []string, stdout, stderr io.Writer) error` | `cmd/dwx-signal` |

`prog` is the invocation prefix used in usage/version text, so the same code
prints `dwx mesh …` under the unified CLI and `dwxctl …` under the alias.

`cmd/dwx/main.go` is the umbrella: it routes service nouns, runs the PATH-plugin
fallthrough, and supports **multi-call** dispatch — when `argv[0]` is `dwxctl` /
`dwx-mcp` / `dwx-signal` (e.g. via a symlink) it behaves as that legacy tool.

## Backward compatibility

`dwxctl`, `dwx-mcp`, and `dwx-signal` continue to exist and work:

- They ship as thin shim binaries (`cmd/dwxctl`, `cmd/dwx-mcp`,
  `cmd/dwx-signal`) that delegate to the shared `internal/cli/*` packages.
- `dwxctl` and `dwx-signal` print a one-line deprecation hint to **stderr**
  (never stdout — it must not corrupt snapshot JSON or MCP frames). `dwx-mcp`
  prints nothing: some MCP clients choke on stderr chatter.
- The Homebrew cask installs `dwx` plus both aliases, so existing MCP client
  configs and operator scripts are untouched.

Plan of record: keep the aliases for a minor or two, then drop them.

## Release / packaging

`.goreleaser.yaml` builds `dwx` (primary) plus the `dwxctl`/`dwx-mcp` alias
binaries; all three go in the one archive and the Homebrew cask
(`binaries: [dwx, dwxctl, dwx-mcp]`). `dwx-signal` was never released
standalone, so it is *not* a release artifact — it is reached as `dwx signal`
(the `cmd/dwx-signal` shim remains for `go build` / dev use). The Windows
cross-platform CI guard now builds `./cmd/dwx`, which transitively pulls in all
three services and is the real portability canary.

## Alternatives considered

- **Fold `signal`/`mcp` into `dwxctl`, keep the `-ctl` name.** Solves today's
  stray binaries but keeps the kubernetes-flavored "controls one cluster"
  framing and doesn't give a home for `cloud` or premium services. We'd
  re-architect at the next service.
- **One monolith that links premium in.** Simplest dispatch, but breaks the
  clean-room open-core build. The PATH-plugin seam is exactly what avoids it.
- **Pure multi-call symlinks, no shim mains.** Cleaner tree, but makes
  goreleaser archive symlinks fiddly across OSes (Windows zips). Shim mains are
  a few lines each and keep packaging boring. (The multi-call path is still
  supported for anyone who *does* symlink.)

## Follow-ups

- Premium repos expose their surfaces as `dwx-cloud` / `dwx-coordinator`
  plugins.
- Sweep remaining user-facing docs (README quickstart, install.md) from
  `dwxctl …` to `dwx mesh …`.
- Consider `dwx plugin list` to enumerate discovered `dwx-*` plugins.
