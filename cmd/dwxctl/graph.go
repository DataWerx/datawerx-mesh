package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/DataWerx/datawerx-mesh/internal/meshstate"
	"github.com/DataWerx/datawerx-mesh/pkg/meshgraph"
)

// runGraph renders the mesh dependency graph: this cluster, the clusters it
// peers with, and the services that flow between them. It is read-only and built
// from the same snapshot every other read command uses, so the graph can never
// disagree with `dwxctl snapshot`. The default JSON output is the stable
// artifact; dot and mermaid are renderings for `dot -Tsvg` and inline docs.
func runGraph(args []string) int {
	fs := flag.NewFlagSet("graph", flag.ExitOnError)
	namespace := fs.String("namespace", meshstate.DefaultNamespace, "namespace the agent is installed in")
	daemonset := fs.String("daemonset", meshstate.DefaultDaemonSet, "agent DaemonSet name")
	format := fs.String("format", "json", "output format: json|dot|mermaid")
	schema := fs.Bool("schema", false, "print the graph's JSON Schema and exit (no cluster access)")
	kubeconfig := fs.String("kubeconfig", "", "path to kubeconfig (default: ambient)")
	kubecontext := fs.String("context", "", "kubeconfig context to use")
	_ = fs.Parse(args)

	if *schema {
		return printSchema(graphSchemaJSON)
	}

	snap, code := gatherSnapshot(*kubeconfig, *kubecontext, *namespace, *daemonset)
	if code != 0 {
		return code
	}
	graph := meshgraph.Build(snap)

	switch *format {
	case "json":
		out, err := graph.JSON()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return 1
		}
		fmt.Println(string(out))
	case "dot":
		fmt.Print(graph.DOT())
	case "mermaid":
		fmt.Print(graph.Mermaid())
	default:
		fmt.Fprintf(os.Stderr, "error: unknown format %q (want json|dot|mermaid)\n", *format)
		return 2
	}
	return 0
}
