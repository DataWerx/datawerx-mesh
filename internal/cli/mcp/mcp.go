// Package mcp implements the `mcp` service of the unified `dwx` CLI — a
// read-only Model Context Protocol server for a DataWerx mesh. It is also what
// the deprecated `dwx-mcp` alias dispatches into (see cmd/dwx, design 0016).
//
// It exposes the mesh's observed state - exactly the same versioned
// snapshot `dwx mesh snapshot` emits — as MCP tools, so any MCP-speaking agent
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
package mcp

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/DataWerx/datawerx-mesh/pkg/logging"
)

// Run starts the read-only MCP stdio server. prog is the invocation prefix used
// in version/error text (e.g. "dwx mcp" or the legacy "dwx-mcp"); args are the
// arguments after that prefix. It blocks serving JSON-RPC on stdin/stdout and
// returns a process exit code.
func Run(prog string, args []string) int {
	fs := flag.NewFlagSet(prog, flag.ContinueOnError)
	namespace := fs.String("namespace", "", "namespace the agent is installed in (default: datawerx-system)")
	daemonset := fs.String("daemonset", "", "agent DaemonSet name (default: dwx-mesh-agent)")
	kubeconfig := fs.String("kubeconfig", "", "path to kubeconfig (default: ambient)")
	kubecontext := fs.String("context", "", "kubeconfig context to use")
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *showVersion {
		fmt.Printf("%s %s\n", prog, logging.Version)
		return 0
	}

	srv, err := newServer(serverConfig{
		namespace:   *namespace,
		daemonset:   *daemonset,
		kubeconfig:  *kubeconfig,
		kubecontext: *kubecontext,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", prog, err)
		return 1
	}

	if err := srv.Serve(context.Background(), os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", prog, err)
		return 1
	}
	return 0
}
