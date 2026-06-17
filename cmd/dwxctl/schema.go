package main

import (
	"fmt"
	"os"

	"github.com/datawerx/datawerx/pkg/contract"
	"github.com/datawerx/datawerx/pkg/meshgraph"
	"github.com/datawerx/datawerx/pkg/verify"
)

// The mesh's two published data contracts and their JSON Schema titles. The
// schemas are generated from the very Go structs that produce the JSON, so they
// can never drift from the wire format; the committed copies under
// docs/contracts are kept in sync by a golden test.
const (
	snapshotSchemaTitle = "MeshSnapshot"
	graphSchemaTitle    = "MeshGraph"
)

// snapshotSchemaJSON returns the JSON Schema for the mesh snapshot contract.
func snapshotSchemaJSON() ([]byte, error) {
	return contract.JSON(verify.Snapshot{}, snapshotSchemaTitle)
}

// graphSchemaJSON returns the JSON Schema for the mesh dependency graph contract.
func graphSchemaJSON() ([]byte, error) {
	return contract.JSON(meshgraph.Graph{}, graphSchemaTitle)
}

// printSchema renders a schema to stdout, returning a process exit code.
func printSchema(gen func() ([]byte, error)) int {
	out, err := gen()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	fmt.Println(string(out))
	return 0
}
