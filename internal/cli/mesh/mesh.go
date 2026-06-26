// Package mesh implements the `mesh` service of the unified `dwx` CLI — the
// operator-facing read surface over a DataWerx mesh. It is also what the
// deprecated `dwxctl` alias dispatches into (see cmd/dwx, design 0016).
//
// Every subcommand is read-only except `join`, which authors a MeshPeer:
//
//   - verify   — health check of an installed mesh (CRDs, agent, peers, exports).
//   - snapshot — emit the versioned machine-readable mesh state snapshot (JSON).
//   - diagnose — rule-based "obvious cause" analysis of why the mesh is unhealthy.
//   - graph    — render the mesh dependency graph (JSON, Graphviz DOT, Mermaid).
//   - reach    — expected cross-cluster reachability: can A reach this cluster, and why not.
//   - slo      — connectivity golden signals: expected reachability vs. observed tunnel liveness.
//   - policy   — dry-run the impact of a proposed MeshNetworkPolicy before apply.
//   - join     — author a MeshPeer for a remote cluster from a bootstrap bundle.
//   - edge     — enroll/render/list edge devices (the EdgeDevice contract, design 0013).
//
// The read-only commands mutate nothing and are safe to run anytime; `verify`
// exits non-zero if any check fails, so it doubles as a smoke test in the
// quickstart and CI.
package mesh

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/DataWerx/datawerx-mesh/internal/meshstate"
	"github.com/DataWerx/datawerx-mesh/pkg/logging"
)

// Run dispatches a `mesh`-service subcommand. prog is the invocation prefix used
// in usage/version text (e.g. "dwx mesh" or the legacy "dwxctl"); args are the
// arguments after that prefix, so args[0] is the subcommand. It returns a
// process exit code.
func Run(prog string, args []string) int {
	if len(args) < 1 {
		usage(prog)
		return 2
	}
	switch args[0] {
	case "verify":
		return runVerify(args[1:])
	case "snapshot":
		return runSnapshot(args[1:])
	case "diagnose":
		return runDiagnose(args[1:])
	case "graph":
		return runGraph(args[1:])
	case "reach":
		return runReach(args[1:])
	case "slo":
		return runSLO(args[1:])
	case "policy":
		return runPolicy(args[1:])
	case "join":
		return runJoin(args[1:])
	case "edge":
		// Reachable here for the `dwxctl edge` alias; the unified CLI also
		// exposes it top-level as `dwx edge` via RunEdge.
		return runEdge(args[1:])
	case "version", "--version", "-v":
		fmt.Printf("%s %s\n", prog, logging.Version)
		return 0
	case "-h", "--help", "help":
		usage(prog)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", args[0])
		usage(prog)
		return 2
	}
}

// RunEdge is the top-level `dwx edge` entry point. Edge-device management lives
// in this package alongside the rest of the read surface, but the unified CLI
// surfaces it as its own service noun (design 0016).
func RunEdge(prog string, args []string) int {
	return runEdge(args)
}

func usage(prog string) {
	fmt.Fprintf(os.Stderr, `%[1]s — DataWerx Mesh

Usage:
  %[1]s verify [flags]      Read-only health check of an installed mesh
  %[1]s snapshot [flags]    Emit the versioned mesh state snapshot as JSON
  %[1]s diagnose [flags]    Rule-based "obvious cause" analysis of mesh health
  %[1]s graph [flags]       Render the mesh dependency graph (json|dot|mermaid)
  %[1]s reach [flags]       Expected cross-cluster reachability (why can't A reach B)
  %[1]s slo [flags]         Connectivity golden signals (expected vs. observed)
  %[1]s policy --dry-run    Impact analysis of a proposed MeshNetworkPolicy
  %[1]s join [sub] [flags]  Bootstrap a peering: export/import a join bundle
  %[1]s edge [sub] [flags]  Edge devices: enroll/profile/list (design 0013)
  %[1]s version             Print the version

Run "%[1]s <command> -h" for flags.
`, prog)
}

func runVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	namespace := fs.String("namespace", meshstate.DefaultNamespace, "namespace the agent is installed in")
	daemonset := fs.String("daemonset", meshstate.DefaultDaemonSet, "agent DaemonSet name")
	output := fs.String("output", "text", "output format: text|json")
	kubeconfig := fs.String("kubeconfig", "", "path to kubeconfig (default: ambient)")
	kubecontext := fs.String("context", "", "kubeconfig context to use")
	_ = fs.Parse(args)

	// The health report is just the snapshot's embedded Health block, so verify
	// gathers exactly the same state every other read command does.
	snap, code := gatherSnapshot(*kubeconfig, *kubecontext, *namespace, *daemonset)
	if code != 0 {
		return code
	}
	report := snap.Health

	if *output == "json" {
		b, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 1
		}
		fmt.Println(string(b))
	} else {
		fmt.Printf("DataWerx Mesh health (namespace %q):\n\n", *namespace)
		report.Write(os.Stdout)
	}
	if report.Failed() {
		return 1
	}
	return 0
}

// newClient builds the shared controller-runtime client. It delegates to
// meshstate so every command (and the MCP server) registers the same scheme.
func newClient(kubeconfig, kubecontext string) (client.Client, error) {
	return meshstate.NewClient(kubeconfig, kubecontext)
}
