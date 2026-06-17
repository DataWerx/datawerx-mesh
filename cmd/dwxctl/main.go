// Command dwxctl is a small operator-facing CLI for DataWerx Mesh.
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
//
// The read-only commands mutate nothing and are safe to run anytime; `verify`
// exits non-zero if any check fails, so it doubles as a smoke test in the
// quickstart and CI.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/datawerx/datawerx/internal/meshstate"
	"github.com/datawerx/datawerx/pkg/logging"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "verify":
		os.Exit(runVerify(os.Args[2:]))
	case "snapshot":
		os.Exit(runSnapshot(os.Args[2:]))
	case "diagnose":
		os.Exit(runDiagnose(os.Args[2:]))
	case "graph":
		os.Exit(runGraph(os.Args[2:]))
	case "reach":
		os.Exit(runReach(os.Args[2:]))
	case "slo":
		os.Exit(runSLO(os.Args[2:]))
	case "policy":
		os.Exit(runPolicy(os.Args[2:]))
	case "join":
		os.Exit(runJoin(os.Args[2:]))
	case "version", "--version", "-v":
		fmt.Printf("dwxctl %s\n", logging.Version)
		os.Exit(0)
	case "-h", "--help", "help":
		usage()
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `dwxctl — DataWerx Mesh CLI

Usage:
  dwxctl verify [flags]      Read-only health check of an installed mesh
  dwxctl snapshot [flags]    Emit the versioned mesh state snapshot as JSON
  dwxctl diagnose [flags]    Rule-based "obvious cause" analysis of mesh health
  dwxctl graph [flags]       Render the mesh dependency graph (json|dot|mermaid)
  dwxctl reach [flags]       Expected cross-cluster reachability (why can't A reach B)
  dwxctl slo [flags]         Connectivity golden signals (expected vs. observed)
  dwxctl policy --dry-run    Impact analysis of a proposed MeshNetworkPolicy
  dwxctl join [sub] [flags]  Bootstrap a peering: export/import a join bundle
  dwxctl version             Print the dwxctl version

Run "dwxctl <command> -h" for flags.
`)
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
