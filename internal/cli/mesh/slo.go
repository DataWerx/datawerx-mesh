package mesh

import (
	"flag"
	"fmt"
	"os"

	"github.com/DataWerx/datawerx-mesh/internal/meshstate"
	"github.com/DataWerx/datawerx-mesh/pkg/slo"
)

// runSLO reports the connectivity golden signals: for each remote cluster, it
// reconciles what topology and policy *expect* (from pkg/reach) against the
// *observed* tunnel liveness (the WireGuard handshake) into a single verdict —
// Healthy, Impaired, Blocked, Degraded, or Down. Impaired is the one that
// matters most: the cluster should be reachable, but the tunnel is not passing
// traffic. It is read-only and built from the same snapshot every read command
// uses. It exits non-zero when any cluster is Impaired, so it can gate a probe.
func runSLO(args []string) int {
	fs := flag.NewFlagSet("slo", flag.ExitOnError)
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
	report := slo.FromSnapshot(snap)

	if *output == "json" {
		out, err := report.JSON()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 1
		}
		fmt.Println(string(out))
	} else {
		writeSLO(report)
	}
	if !report.Healthy() {
		return 1
	}
	return 0
}

// sloSymbol maps a verdict to a glyph for the text report.
func sloSymbol(v slo.Verdict) string {
	switch v {
	case slo.VerdictHealthy:
		return "✓"
	case slo.VerdictImpaired:
		return "✗"
	default:
		return "!"
	}
}

// writeSLO renders the connectivity report in human-readable form.
func writeSLO(r slo.Report) {
	if len(r.Signals) == 0 {
		fmt.Println("No mesh peers to assess.")
		return
	}
	fmt.Println("Connectivity golden signals (expected vs. observed, per remote cluster):")
	fmt.Println()
	for _, s := range r.Signals {
		fmt.Printf("  %s  %-20s %-10s (expected %s, tunnel live=%v)\n",
			sloSymbol(s.Verdict), s.Cluster, s.Verdict, s.Expected, s.TunnelLive)
		fmt.Printf("       %s\n", s.Reason)
	}
}
