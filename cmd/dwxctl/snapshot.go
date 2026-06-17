package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/DataWerx/datawerx-mesh/internal/meshstate"
	"github.com/DataWerx/datawerx-mesh/pkg/verify"
)

// runSnapshot emits the versioned, machine-readable mesh state snapshot as JSON.
// It is the keystone free artifact.  It is an observability hook and the input
// contract every higher layer consumes.
func runSnapshot(args []string) int {
	fs := flag.NewFlagSet("snapshot", flag.ExitOnError)
	namespace := fs.String("namespace", meshstate.DefaultNamespace, "namespace the agent is installed in")
	daemonset := fs.String("daemonset", meshstate.DefaultDaemonSet, "agent DaemonSet name")
	schema := fs.Bool("schema", false, "print the snapshot's JSON Schema and exit (no cluster access)")
	kubeconfig := fs.String("kubeconfig", "", "path to kubeconfig (default: ambient)")
	kubecontext := fs.String("context", "", "kubeconfig context to use")
	_ = fs.Parse(args)

	if *schema {
		return printSchema(snapshotSchemaJSON)
	}

	snap, code := gatherSnapshot(*kubeconfig, *kubecontext, *namespace, *daemonset)
	if code != 0 {
		return code
	}
	out, err := snap.JSON()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	fmt.Println(string(out))
	return 0
}

// runDiagnose runs the rule-based "obvious cause" checker over a fresh snapshot
// and prints the grounded findings. It exits non-zero when a critical cause is
// found so it can gate a pipeline.
func runDiagnose(args []string) int {
	fs := flag.NewFlagSet("diagnose", flag.ExitOnError)
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
	findings := verify.Diagnose(snap)
	if *output == "json" {
		out, err := jsonMarshal(findings)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 1
		}
		fmt.Println(out)
	} else {
		writeFindings(findings)
	}
	for _, f := range findings {
		if f.Severity == verify.SeverityCritical {
			return 1
		}
	}
	return 0
}

// gatherSnapshot loads a client and assembles the snapshot, returning a non-zero
// process code on failure (already logged to stderr).
func gatherSnapshot(kubeconfig, kubecontext, namespace, daemonset string) (verify.Snapshot, int) {
	c, err := newClient(kubeconfig, kubecontext)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return verify.Snapshot{}, 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	snap, err := meshstate.Snapshot(ctx, c, namespace, daemonset)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return verify.Snapshot{}, 1
	}
	return snap, 0
}
