# Contributing

Thanks for your interest in DataWerx Mesh. This guide covers the dev loop,
conventions, and how to run each test layer. By participating you agree to the
[Code of Conduct](CODE_OF_CONDUCT.md). For the design model and the docs, see
[ARCHITECTURE.md](ARCHITECTURE.md) and [docs/](docs/README.md).

## Prerequisites

- **Go 1.24+**
- For the kernel/data-plane and e2e suites: a Linux host with root, the
  `wireguard` and `iptable_nat` modules, `kind`, `kubectl`, `helm`, and
  `wireguard-tools`.

## Build, test, lint

```sh
go build ./...                                # compile
CGO_ENABLED=0 go build -o dwx-manager ./cmd/manager
CGO_ENABLED=0 go build -o dwxctl ./cmd/dwxctl

go test ./...                                 # unit tests (hermetic, root-free)
go test -race -count=1 ./...                  # what CI runs
go vet ./...
gofmt -l .                                    # must be empty
```

> **Dependency gotcha:** the WireGuard control library host
> (`golang.zx2c4.com`) is blocked in some sandboxes/CI. When fetching or
> tidying, force the module proxy with no `direct` fallback:
> `GOPROXY=https://proxy.golang.org go mod tidy`. Plain `go build`/`go test`
> work once modules are cached.

## The tagged test layers

```sh
# Controller integration tests against a real API server (envtest binaries).
KUBEBUILDER_ASSETS=$(setup-envtest use -p path 1.30.0) \
  go test -tags integration ./test/integration/...

# Real kernel data plane (root; runs each test in a throwaway netns).
sudo modprobe wireguard iptable_nat
sudo -E env "PATH=$PATH" go test -tags dataplane ./pkg/wg/... ./pkg/nat/...

# Two-cluster e2e on kind.
hack/e2e/kind-up.sh
E2E_CONTEXT_A=kind-dwx-a E2E_CONTEXT_B=kind-dwx-b go test -tags e2e ./test/e2e/...
hack/e2e/kind-down.sh
```

## Conventions (please follow)

- **Pure logic stays pure.** New "compute desired state" logic belongs in
  `pkg/topology`, `pkg/dns`, `pkg/nat`, etc. — no Kubernetes client, no kernel.
  Reconcilers and managers call into it. This is what keeps the bulk of the
  logic unit-testable. See `ARCHITECTURE.md`.
- **Keep premium behind the seam.** Tier-specific behavior goes behind
  `ControlPlaneClient` (or a new injected interface) — never an `if premium`
  inside the reconcile loop or data plane.
- **Hand-maintained codegen.** There is no controller-gen step. If you add or
  change a CRD field, update the hand-written `zz_generated.deepcopy.go` *and*
  the CRD YAML in `config/crd/` (and the chart's `crds/`) to match.
- **Data-plane code needs `-race`.** Anything touching the manager mutexes or
  the reconciler's `keyIndex` must pass `go test -race`.
- **Never log full WireGuard keys** — truncate via `shortKey`.
- **Default `go test ./...` must stay hermetic.** Anything needing root, a real
  API server, or external binaries goes behind a build tag.

## Pull requests

- Branch from `main`; **base your PR on `main`** (don't stack PRs on other
  feature branches — merging the base first leaves the child merging into a dead
  branch).
- CI must be green: `gofmt`, `go vet`, static build, `go test -race`.
- Sign off commits (DCO): `git commit -s`. Contributions are accepted under
  Apache-2.0 (see `LICENSE` and `COMMITMENT.md`).
- Don't commit secrets, generated binaries, or vendored model identifiers.
- Note user-visible changes in `CHANGELOG.md` under `[Unreleased]`.

## Releasing

Maintainers cut releases from a semver tag; the process and what a tag produces
are in [`RELEASING.md`](RELEASING.md).

## Reporting security issues

Privately — see `SECURITY.md`.
