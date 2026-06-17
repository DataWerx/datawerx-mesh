# Examples & ecosystem starters

Drop-in starting points for adopting DataWerx Mesh in a real platform. Copy what
you need into your own repos and adjust for your environment.

- **[terraform/](terraform/)** — a Terraform module that installs the agent from
  the signed OCI Helm chart, parameterized by cluster ID and WireGuard key.
- **[backstage/](backstage/)** — a Backstage `catalog-info.yaml` that registers
  the mesh, its CRD/MCS API, and the agent DaemonSet in your software catalog.

These are validated in CI by the `ecosystem` workflow (GoReleaser config,
`terraform fmt`/`validate`, and the Artifact Hub / Backstage manifests), so they
stay installable as the project moves.

For the `MeshPeer` and `MeshNetworkPolicy` samples themselves, see
[`config/samples/`](../config/samples/). For installation and the two-cluster
quickstart, see [`docs/`](../docs/).
