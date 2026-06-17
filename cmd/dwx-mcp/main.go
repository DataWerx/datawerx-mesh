// Command dwx-mcp is a read-only Model Context Protocol server for a DataWerx
// mesh. It exposes the mesh's observed state - exactly the same versioned
// snapshot `dwxctl snapshot` emits — as MCP tools, so any MCP-speaking agent
// can ask "what does cluster B import?" or "is the mesh healthy and why not?"
// against live cluster state.
//
// It is deliberately read-only. There is no tool here that mutates
// the mesh in the free tier. Reading mesh state is the free, however acting
// on the mesh (such as apply a fix, change a policy, rotate a key) is the
// governed, audited, SSO/RBAC-gated premium surface and lives in the hosted
// plane, not in this binary.
//
// The transport is the standard MCP stdio framing: newline-delimited JSON-RPC
// 2.0 messages on stdin/stdout. It is implemented directly (no SDK) to keep the
// OSS binary small and dependency-light.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/datawerx/datawerx/pkg/logging"
)

func main() {
	namespace := flag.String("namespace", "", "namespace the agent is installed in (default: datawerx-system)")
	daemonset := flag.String("daemonset", "", "agent DaemonSet name (default: dwx-mesh-agent)")
	kubeconfig := flag.String("kubeconfig", "", "path to kubeconfig (default: ambient)")
	kubecontext := flag.String("context", "", "kubeconfig context to use")
	showVersion := flag.Bool("version", false, "print the dwx-mcp version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("dwx-mcp %s\n", logging.Version)
		return
	}

	srv, err := newServer(serverConfig{
		namespace:   *namespace,
		daemonset:   *daemonset,
		kubeconfig:  *kubeconfig,
		kubecontext: *kubecontext,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "dwx-mcp: %v\n", err)
		os.Exit(1)
	}

	if err := srv.Serve(context.Background(), os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "dwx-mcp: %v\n", err)
		os.Exit(1)
	}
}
