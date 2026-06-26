// Command dwx-signal is a deprecated alias for `dwx signal`. It is retained for
// backward compatibility; new usage should prefer the unified `dwx` CLI (design
// 0016). It shares all logic with `dwx` via internal/cli/signalcli.
package main

import (
	"fmt"
	"os"

	"github.com/DataWerx/datawerx-mesh/internal/cli/signalcli"
)

func main() {
	fmt.Fprintln(os.Stderr, `note: "dwx-signal" is deprecated; use "dwx signal" (run `+"`dwx help`"+`)`)
	if err := signalcli.Run("dwx-signal", os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "dwx-signal: %v\n", err)
		os.Exit(1)
	}
}
