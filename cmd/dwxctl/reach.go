package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/DataWerx/datawerx-mesh/internal/meshstate"
	"github.com/DataWerx/datawerx-mesh/pkg/reach"
)

// runReach reports the expected cross-cluster reachability into this cluster:
// for each remote cluster, can it reach in, and if not, why? It answers
// "why can't A reach B" deterministically from the same snapshot every other
// read command uses, composing the real firewall compiler so the verdicts match
// what the data plane programs. It is read-only.
func runReach(args []string) int {
	fs := flag.NewFlagSet("reach", flag.ExitOnError)
	namespace := fs.String("namespace", meshstate.DefaultNamespace, "namespace the agent is installed in")
	daemonset := fs.String("daemonset", meshstate.DefaultDaemonSet, "agent DaemonSet name")
	output := fs.String("output", "text", "output format: text|json")
	kubeconfig := fs.String("kubeconfig", "", "path to kubeconfig (default: ambient)")
	kubecontext := fs.String("context", "", "kubeconfig context to use")
	_ = fs.Parse(args)

	snap, code := gatherSnapshot(*kubeconfig, *kubecontext, *namespace, *daemonset)
	if code != 0 {
		return code
	}
	matrix := reach.FromSnapshot(snap)

	if *output == "json" {
		out, err := matrix.JSON()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 1
		}
		fmt.Println(string(out))
		return 0
	}
	writeReach(matrix)
	return 0
}

// reachSymbol maps a status to a glyph for the text report.
func reachSymbol(s reach.Status) string {
	switch s {
	case reach.StatusReachable:
		return "✓"
	case reach.StatusBlocked:
		return "✗"
	case reach.StatusDegraded, reach.StatusUnreachable:
		return "!"
	default:
		return "?"
	}
}

// writeReach renders the matrix in human-readable form.
func writeReach(m reach.Matrix) {
	if len(m.Reachabilities) == 0 {
		fmt.Println("No mesh peers to assess.")
		return
	}
	fmt.Println("Expected reachability into this cluster (per remote cluster):")
	fmt.Println()
	for _, r := range m.Reachabilities {
		fmt.Printf("  %s  %-20s %s\n", reachSymbol(r.Status), r.Cluster, r.Status)
		fmt.Printf("       %s\n", r.Reason)
		for _, d := range r.Dests {
			mark := "allowed"
			if !d.Allowed {
				mark = "denied"
			}
			fmt.Printf("         - %s: %s\n", d.Dest, mark)
		}
	}
}
