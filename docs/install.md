# Installing the DataWerx CLIs

DataWerx ships two operator-facing command-line tools:

- **`dwxctl`** — the read-only operator CLI (`verify`, `snapshot`, `diagnose`,
  `graph`, `policy --dry-run`) plus `join` for zero-friction peering.
- **`dwx-mcp`** — a read-only [Model Context Protocol](https://modelcontextprotocol.io)
  server that exposes the mesh's state to any MCP-speaking agent.

The agent itself (the DaemonSet) is installed separately via the Helm chart or
`deploy/agent.yaml`; see the [quickstart](quickstart.md). This page is about the
CLIs you run from a laptop or CI.

## Homebrew (macOS / Linux)

```sh
brew install datawerx/tap/dwxctl
```

The formula installs both `dwxctl` and `dwx-mcp`.

## Direct download

Grab the archive for your platform from the
[latest release](https://github.com/DataWerx/datawerx-mesh/releases/latest),
extract it, and put the binaries on your `PATH`:

```sh
tar -xzf datawerx_<version>_<os>_<arch>.tar.gz
sudo install dwxctl dwx-mcp /usr/local/bin/
dwxctl version
```

## Verify the download (cosign)

Every release is signed keylessly with [cosign](https://docs.sigstore.dev/).
The `checksums.txt` is signed, so verifying it covers every archive:

```sh
cosign verify-blob \
  --certificate checksums.txt.pem \
  --signature checksums.txt.sig \
  --certificate-identity-regexp 'https://github.com/DataWerx/datawerx-mesh' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  checksums.txt

# then confirm your archive's checksum is listed
sha256sum -c checksums.txt --ignore-missing
```

Each archive also ships an SBOM (`*.sbom.json`, SPDX) for supply-chain auditing.

## From source

```sh
CGO_ENABLED=0 go build -o dwxctl  ./cmd/dwxctl
CGO_ENABLED=0 go build -o dwx-mcp ./cmd/dwx-mcp
```

## Use `dwx-mcp` with an MCP host

`dwx-mcp` speaks MCP over stdio, so any MCP host can launch it. It reads the
mesh through your kubeconfig (read-only — it exposes no mutating tools).

**Claude Desktop / Claude Code** — add to your MCP config
(`claude_desktop_config.json`, or `.mcp.json` for Claude Code):

```json
{
  "mcpServers": {
    "datawerx-mesh": {
      "command": "dwx-mcp",
      "args": ["--context", "my-cluster"]
    }
  }
}
```

Then ask the agent things like *"is the mesh healthy, and why not?"* or *"what
does this cluster import, and from which clusters?"* — answered from the live
[snapshot](design/0005-mesh-snapshot.md) and
[dependency graph](design/0009-mesh-dependency-graph.md).

Available tools (all read-only): `mesh_snapshot`, `mesh_health`,
`mesh_diagnose`, `mesh_graph`, `list_peers`, `list_service_imports`,
`list_service_exports`.
