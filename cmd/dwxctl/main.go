// Command dwxctl is a deprecated alias for `dwx mesh`. It is retained so the
// Homebrew cask and existing operator muscle memory keep working; new usage
// should prefer the unified `dwx` CLI (design 0016). It shares all logic with
// `dwx` via internal/cli/mesh.
package main

import (
	"fmt"
	"os"

	"github.com/DataWerx/datawerx-mesh/internal/cli/mesh"
)

func main() {
	fmt.Fprintln(os.Stderr, `note: "dwxctl" is deprecated; use "dwx mesh" (run `+"`dwx help`"+`)`)
	os.Exit(mesh.Run("dwxctl", os.Args[1:]))
}
