// Command dwx is the single, unified DataWerx CLI. Following the AWS-CLI model,
// every capability is a service noun under one binary:
//
//	dwx mesh   verify | snapshot | diagnose | graph | reach | slo | policy | join
//	dwx edge   enroll | profile | list
//	dwx signal "<question>"
//	dwx mcp                       # read-only MCP stdio server
//	dwx version
//
// The free (open-core) build ships the services above. Premium services are
// discovered at runtime, kubectl-style: an unknown command `X` is dispatched to
// a `dwx-X` executable found on PATH (e.g. `dwx cloud ...` -> `dwx-cloud`). The
// open core therefore never imports premium code — the plugin is exec'd, not
// linked — which preserves the clean-room build guarantee (design 0016).
//
// Backward compatibility: the legacy binaries `dwxctl`, `dwx-mcp`, and
// `dwx-signal` still work. They are shipped as thin shims (see cmd/dwxctl,
// cmd/dwx-mcp, cmd/dwx-signal), and this binary also honors them when invoked
// under those names via a symlink — so existing MCP client configs and the
// Homebrew cask keep functioning while the new form is adopted.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/DataWerx/datawerx-mesh/internal/cli/mcp"
	"github.com/DataWerx/datawerx-mesh/internal/cli/mesh"
	"github.com/DataWerx/datawerx-mesh/internal/cli/signalcli"
	"github.com/DataWerx/datawerx-mesh/pkg/logging"
)

func main() {
	os.Exit(dispatch(os.Args))
}

func dispatch(argv []string) int {
	// Multi-call: when invoked under a legacy alias name (e.g. via a symlink),
	// behave exactly as that tool did.
	switch aliasName(argv[0]) {
	case "dwxctl":
		deprecated("dwxctl", "dwx mesh")
		return mesh.Run("dwxctl", argv[1:])
	case "dwx-mcp":
		deprecated("dwx-mcp", "dwx mcp")
		return mcp.Run("dwx-mcp", argv[1:])
	case "dwx-signal":
		deprecated("dwx-signal", "dwx signal")
		return runSignal("dwx-signal", argv[1:])
	}

	if len(argv) < 2 {
		usage()
		return 2
	}

	cmd, rest := argv[1], argv[2:]
	switch cmd {
	case "mesh":
		return mesh.Run("dwx mesh", rest)
	case "edge":
		return mesh.RunEdge("dwx edge", rest)
	case "signal":
		return runSignal("dwx signal", rest)
	case "mcp":
		return mcp.Run("dwx mcp", rest)
	case "version", "--version", "-v":
		fmt.Printf("dwx %s\n", logging.Version)
		return 0
	case "help", "-h", "--help":
		usage()
		return 0
	default:
		// Unknown command: try a PATH plugin `dwx-<cmd>` (e.g. premium `dwx cloud`).
		if code, ok := runPlugin(cmd, rest); ok {
			return code
		}
		fmt.Fprintf(os.Stderr, "dwx: unknown command %q\n\n", cmd)
		usage()
		return 2
	}
}

// runSignal adapts the signal service (which reports via error) to an exit code.
func runSignal(prog string, args []string) int {
	if err := signalcli.Run(prog, args, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", prog, err)
		return 1
	}
	return 0
}

// runPlugin execs a `dwx-<cmd>` binary found on PATH, wiring it to the current
// stdio, and returns (exitCode, true). If no such plugin exists it returns
// (0, false) so the caller can report an unknown command.
func runPlugin(cmd string, args []string) (int, bool) {
	path, err := exec.LookPath("dwx-" + cmd)
	if err != nil {
		return 0, false
	}
	c := exec.Command(path, args...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), true
		}
		fmt.Fprintf(os.Stderr, "dwx: %s: %v\n", path, err)
		return 1, true
	}
	return 0, true
}

// aliasName reduces an argv[0] to a bare command name for multi-call dispatch,
// stripping any directory and a Windows ".exe" suffix.
func aliasName(arg0 string) string {
	return strings.TrimSuffix(filepath.Base(arg0), ".exe")
}

// deprecated prints a one-line hint steering users to the unified form. It goes
// to stderr so it never corrupts machine-readable stdout (snapshots, MCP frames).
func deprecated(old, replacement string) {
	fmt.Fprintf(os.Stderr, "note: %q is deprecated; use %q (run `dwx help`)\n", old, replacement)
}

func usage() {
	fmt.Fprintf(os.Stderr, `dwx — the DataWerx CLI

Usage:
  dwx mesh   <cmd> [flags]   Mesh read surface: verify, snapshot, diagnose,
                             graph, reach, slo, policy, join
  dwx edge   <cmd> [flags]   Edge devices: enroll, profile, list (design 0013)
  dwx signal "<question>"    Grounded natural-language answers about the mesh
  dwx mcp    [flags]         Read-only Model Context Protocol stdio server
  dwx version               Print the dwx version

Premium services (e.g. "dwx cloud") are provided by dwx-<service> plugins on
PATH when installed.

Run "dwx <service> -h" for a service's flags.
`)
}
