# Releasing DataWerx Mesh

Releases are cut from an annotated semver tag. Pushing a `vX.Y.Z` tag triggers
two workflows that together produce everything a user installs.

## What a tag produces

| Artifact | Built by | Published to |
|----------|----------|--------------|
| Multi-arch agent image (signed, with SBOM) | `.github/workflows/release.yml` | `ghcr.io/datawerx/datawerx-mesh/mesh-agent` |
| Helm chart (OCI) | `.github/workflows/release.yml` | `oci://ghcr.io/datawerx/datawerx-mesh/charts` |
| `dwxctl` + `dwx-mcp` archives (checksummed, SBOM'd, cosign-signed) | GoReleaser (`.goreleaser.yaml`) | the GitHub Release |
| Homebrew cask | GoReleaser | `datawerx/homebrew-tap` |

The image and chart job runs in `append` mode so GoReleaser and the chart upload
attach to the same Release without clobbering each other.

## Cutting a release

1. **Land the work.** Every change is on `main`, green (gofmt, vet, race tests,
   CodeQL, govulncheck, Scorecard).
2. **Update the changelog.** Move the `[Unreleased]` entries under a new
   `[X.Y.Z]` heading in `CHANGELOG.md`, date it, and refresh the compare links at
   the bottom.
3. **Bump the chart.** Set `version` and `appVersion` in
   `charts/datawerx-mesh/Chart.yaml` to `X.Y.Z`, and update the
   `artifacthub.io/images` annotation tag to match.
4. **Commit on a branch and open a PR.** Title it `Release vX.Y.Z`. Merge once
   green.
5. **Tag `main`.**

   ```sh
   git checkout main && git pull
   git tag -a vX.Y.Z -m "vX.Y.Z"
   git push origin vX.Y.Z
   ```

6. **Watch the workflows.** `release.yml` and the GoReleaser job both run off the
   tag. When they finish, the GitHub Release carries the image reference, the
   chart, and the CLI archives.

## Versioning

Pre-1.0, the API may change between minor versions, but the `ControlPlaneClient`
seam and the `MeshPeer` contract are stable per `COMMITMENT.md`. The CLI version
is stamped into the binary via the GoReleaser `ldflags`
(`pkg/logging.Version`), so `dwxctl version` reports the released tag.

## Verifying a release

The whole archive set is verifiable from the one cosign-signed `checksums.txt`,
and the image carries a keyless cosign signature and an SBOM attestation. The
verification commands live in [docs/install.md](docs/install.md).
