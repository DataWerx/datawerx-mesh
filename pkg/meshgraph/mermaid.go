package meshgraph

import (
	"fmt"
	"sort"
	"strings"
)

// Mermaid renders the graph as a Mermaid flowchart, the format GitHub, GitLab,
// Backstage, and most wikis render inline — so a mesh diagram can live in a
// README or a runbook with no Graphviz toolchain. Like DOT it is a pure,
// deterministic function of the graph. Node ids are sanitized to Mermaid-safe
// identifiers; a stable alias map keeps the output byte-stable.
func (g Graph) Mermaid() string {
	var b strings.Builder
	b.WriteString("flowchart LR\n")

	alias := mermaidAliases(g.Nodes)
	for _, n := range g.Nodes {
		fmt.Fprintf(&b, "  %s%s\n", alias[n.ID], mermaidNodeShape(n))
	}
	for _, e := range g.Edges {
		fmt.Fprintf(&b, "  %s -->|%s| %s\n", alias[e.From], mermaidLabel(edgeLabel(e)), alias[e.To])
	}

	// Class styling for the local cluster and conflicted peers, so the diagram
	// reads at a glance the same way the DOT does.
	var locals, conflicts []string
	for _, n := range g.Nodes {
		switch {
		case n.Kind == NodeCluster && n.Local:
			locals = append(locals, alias[n.ID])
		case n.Kind == NodeCluster && n.Conflict:
			conflicts = append(conflicts, alias[n.ID])
		}
	}
	b.WriteString("  classDef local fill:#d8e8ff,stroke:#1a4f9f;\n")
	b.WriteString("  classDef conflict fill:#ffd8d8,stroke:#cc0000;\n")
	if len(locals) > 0 {
		fmt.Fprintf(&b, "  class %s local;\n", strings.Join(locals, ","))
	}
	if len(conflicts) > 0 {
		fmt.Fprintf(&b, "  class %s conflict;\n", strings.Join(conflicts, ","))
	}

	return b.String()
}

// mermaidAliases assigns each node a short, Mermaid-safe identifier (n0, n1, …)
// in sorted id order, so the mapping is deterministic.
func mermaidAliases(nodes []Node) map[string]string {
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.ID)
	}
	sort.Strings(ids)
	alias := make(map[string]string, len(ids))
	for i, id := range ids {
		alias[id] = fmt.Sprintf("n%d", i)
	}
	return alias
}

// mermaidNodeShape renders a node's shape and label: rounded box for the local
// cluster, stadium for a service, and a plain box for a peer.
func mermaidNodeShape(n Node) string {
	label := mermaidLabel(strings.ReplaceAll(nodeLabel(n), "\n", " — "))
	switch {
	case n.Kind == NodeService:
		return "([" + label + "])"
	case n.Kind == NodeCluster && n.Local:
		return "(" + label + ")"
	default:
		return "[" + label + "]"
	}
}

// mermaidLabel quotes a label so Mermaid treats reserved characters literally.
func mermaidLabel(s string) string {
	s = strings.ReplaceAll(s, "\"", "'")
	return "\"" + s + "\""
}
