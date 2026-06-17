package meshgraph

import (
	"fmt"
	"strings"
)

// DOT renders the graph as Graphviz DOT, so an operator can pipe it straight
// into `dot -Tsvg` or paste it into any Graphviz viewer. The layout is
// left-to-right; clusters are boxes (the local one filled), services are
// ellipses, and peering edges to a pending/error/conflicted peer are colored so
// trouble is visible at a glance. The rendering is a pure function of the graph,
// which is itself deterministic, so the same snapshot always produces the same
// DOT.
func (g Graph) DOT() string {
	var b strings.Builder
	b.WriteString("digraph mesh {\n")
	b.WriteString("  rankdir=LR;\n")
	b.WriteString("  node [fontname=\"Helvetica\"];\n")
	b.WriteString("  edge [fontname=\"Helvetica\", fontsize=10];\n")

	for _, n := range g.Nodes {
		fmt.Fprintf(&b, "  %s [%s];\n", dotID(n.ID), dotNodeAttrs(n))
	}
	for _, e := range g.Edges {
		fmt.Fprintf(&b, "  %s -> %s [%s];\n", dotID(e.From), dotID(e.To), dotEdgeAttrs(e))
	}

	b.WriteString("}\n")
	return b.String()
}

// dotNodeAttrs builds the attribute list for a node.
func dotNodeAttrs(n Node) string {
	attrs := []string{fmt.Sprintf("label=%s", dotQuote(nodeLabel(n)))}
	switch n.Kind {
	case NodeCluster:
		attrs = append(attrs, "shape=box", "style=\"rounded,filled\"")
		switch {
		case n.Local:
			attrs = append(attrs, "fillcolor=\"#d8e8ff\"")
		case n.Conflict:
			attrs = append(attrs, "fillcolor=\"#ffd8d8\"")
		default:
			attrs = append(attrs, "fillcolor=\"#f0f0f0\"")
		}
	case NodeService:
		attrs = append(attrs, "shape=ellipse", "style=filled", "fillcolor=\"#e8ffe8\"")
	}
	return strings.Join(attrs, ", ")
}

// dotEdgeAttrs builds the attribute list for an edge.
func dotEdgeAttrs(e Edge) string {
	attrs := []string{fmt.Sprintf("label=%s", dotQuote(edgeLabel(e)))}
	switch e.Kind {
	case EdgePeering:
		switch {
		case e.Conflict || e.Phase == "Error":
			attrs = append(attrs, "color=\"#cc0000\"", "penwidth=2")
		case e.Phase == "Connected":
			attrs = append(attrs, "color=\"#1a7f1a\"")
		default:
			attrs = append(attrs, "color=\"#999999\"", "style=dashed")
		}
	case EdgeServes:
		attrs = append(attrs, "color=\"#1a7f1a\"", "style=dotted")
	case EdgeImports:
		attrs = append(attrs, "color=\"#333333\"")
	case EdgeExports:
		attrs = append(attrs, "color=\"#1a4f9f\"")
	case EdgeAllows:
		attrs = append(attrs, "color=\"#8a4fbf\"", "style=dashed")
	}
	return strings.Join(attrs, ", ")
}

// nodeLabel is the multi-line label drawn inside a node.
func nodeLabel(n Node) string {
	switch n.Kind {
	case NodeCluster:
		if n.Local {
			return n.Label
		}
		label := n.Label
		if n.Phase != "" {
			label += "\n" + n.Phase
		}
		return label
	case NodeService:
		label := n.Label
		switch {
		case n.Imported && n.Exported:
			label += "\nimport+export"
		case n.Imported:
			if n.ImportType != "" {
				label += "\n" + n.ImportType
			} else {
				label += "\nimport"
			}
		case n.Exported:
			label += "\nexport"
		}
		return label
	default:
		return n.Label
	}
}

// edgeLabel is the short label drawn on an edge.
func edgeLabel(e Edge) string {
	switch e.Kind {
	case EdgePeering:
		return "peers"
	case EdgeServes:
		return "serves"
	case EdgeImports:
		return "imported by"
	case EdgeExports:
		return "exports"
	case EdgeAllows:
		if e.Policy != "" {
			return "allows (" + e.Policy + ")"
		}
		return "allows"
	default:
		return string(e.Kind)
	}
}

// dotID renders a node id as a quoted Graphviz identifier.
func dotID(id string) string { return dotQuote(id) }

// dotQuote wraps s in double quotes, escaping the characters DOT treats
// specially inside a quoted string.
func dotQuote(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	s = strings.ReplaceAll(s, "\n", "\\n")
	return "\"" + s + "\""
}
