// Command dwx-mcp is a deprecated alias for `dwx mcp`. It is retained so
// existing MCP client configurations that launch `dwx-mcp` keep working; new
// configs should prefer `dwx mcp` (design 0016). It shares all logic with `dwx`
// via internal/cli/mcp.
//
// No deprecation note is printed here: this binary speaks JSON-RPC over stdio to
// an MCP client, and stderr chatter can confuse some clients. The hint lives in
// the docs and in `dwx help` instead.
package main

import (
	"os"

	"github.com/DataWerx/datawerx-mesh/internal/cli/mcp"
)

func main() {
	os.Exit(mcp.Run("dwx-mcp", os.Args[1:]))
}
