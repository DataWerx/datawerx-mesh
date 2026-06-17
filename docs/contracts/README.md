# Published data contracts

DataWerx Mesh's read surfaces emit two versioned, machine-readable artifacts.
This directory publishes their JSON Schemas so a consumer — a dashboard, a
Backstage plugin, a CI gate, a hosted plane — can validate the JSON it receives.

| Contract | Schema | Produced by | Design |
|----------|--------|-------------|--------|
| Mesh snapshot | [`mesh-snapshot.schema.json`](mesh-snapshot.schema.json) | `dwxctl snapshot`, `dwx-mcp` `mesh_snapshot` | [0005](../design/0005-mesh-snapshot.md) |
| Mesh dependency graph | [`mesh-graph.schema.json`](mesh-graph.schema.json) | `dwxctl graph`, `dwx-mcp` `mesh_graph` | [0009](../design/0009-mesh-dependency-graph.md) |

Both schemas declare JSON Schema draft 2020-12.

## They cannot drift

The schemas are **generated from the Go structs that produce the JSON**, by the
reflective generator in `pkg/contract`. The committed copies here are kept
identical to the generator's output by a golden test (`cmd/dwxctl`), so a change
to the contract types that isn't reflected in these files fails CI. There is no
separate hand-maintained schema to forget to update.

Emit the live schema from the CLI (no cluster access needed):

```sh
dwxctl snapshot --schema    # the mesh snapshot schema
dwxctl graph --schema       # the dependency graph schema
```

Regenerate the committed copies after a contract change:

```sh
go test ./cmd/dwxctl -update-golden
```

## Versioning

Each artifact also carries an `apiVersion` field
(`mesh.datawerx.io/snapshot/v1alpha1`, `mesh.datawerx.io/graph/v1alpha1`). The
schemas evolve additively within a version; a breaking shape change bumps the
`apiVersion` so consumers can branch on it.
