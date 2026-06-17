// Package meshgraph turns a verify.Snapshot into the mesh's dependency graph:
// the cluster this snapshot was taken from, the clusters it peers with, and the
// services that flow between them. It is pure and deterministic — a function of
// the snapshot alone, with no Kubernetes and no model — so it is exhaustively
// table-tested like the rest of the pure tier (pkg/topology, pkg/verify,
// pkg/impact).
//
// The graph is ego-centric: a snapshot is one cluster's observed view, so the
// graph is that cluster ("local") at the center, its peer clusters, and the
// imported/exported services. It answers "who do we talk to, and for what?"
// from data only DataWerx holds in one place — the cross-cluster topology and
// the service dependency graph — and emits it as a stable JSON artifact plus
// Graphviz DOT and Mermaid renderings a human or a dashboard can draw.
//
// This is the free, structural half of the "service dependency map" roadmap
// item: the artifact is open core; the hosted, historical, drift-aware topology
// UI that consumes it is the premium counterpart.
package meshgraph

import (
	"encoding/json"
	"sort"

	"github.com/DataWerx/datawerx-mesh/pkg/verify"
)

const (
	// GraphAPIVersion identifies the graph schema. Like the snapshot, it is
	// versioned so consumers and a future hosted plane can branch on it as the
	// shape evolves.
	GraphAPIVersion = "mesh.datawerx.io/graph/v1alpha1"

	// GraphKind is the constant kind string carried on every graph.
	GraphKind = "MeshGraph"

	// LocalNodeID is the stable id of the node representing the cluster the
	// snapshot was taken from. The snapshot carries no identity for itself, so the
	// graph is ego-centric around this fixed node.
	LocalNodeID = "cluster/local"

	// localLabel is the human label for the local cluster node.
	localLabel = "local (this cluster)"
)

// NodeKind enumerates the kinds of node in the graph.
type NodeKind string

const (
	// NodeCluster is a cluster: the local cluster or a remote peer.
	NodeCluster NodeKind = "cluster"
	// NodeService is a service exported by, or imported into, the local cluster.
	NodeService NodeKind = "service"
)

// EdgeKind enumerates the kinds of relationship in the graph.
type EdgeKind string

const (
	// EdgePeering is a WireGuard peering between the local cluster and a remote.
	EdgePeering EdgeKind = "peering"
	// EdgeImports points from a service to the local cluster that imports it.
	EdgeImports EdgeKind = "imports"
	// EdgeServes points from a remote cluster to a service it contributes
	// backends to, for a service the local cluster imports.
	EdgeServes EdgeKind = "serves"
	// EdgeExports points from the local cluster to a service it exports to the
	// mesh.
	EdgeExports EdgeKind = "exports"
	// EdgeAllows points from a remote cluster to the local cluster when a
	// MeshNetworkPolicy permits that cluster as an ingress source.
	EdgeAllows EdgeKind = "allows"
)

// Node is a vertex in the mesh graph. Only the fields relevant to its Kind are
// populated; the rest are omitted from JSON.
type Node struct {
	ID    string   `json:"id"`
	Kind  NodeKind `json:"kind"`
	Label string   `json:"label"`

	// Cluster fields.
	Local     bool   `json:"local,omitempty"`
	ClusterID string `json:"clusterID,omitempty"`
	Phase     string `json:"phase,omitempty"`
	Endpoint  string `json:"endpoint,omitempty"`
	Conflict  bool   `json:"conflict,omitempty"`

	// Service fields.
	Namespace  string   `json:"namespace,omitempty"`
	Name       string   `json:"name,omitempty"`
	Imported   bool     `json:"imported,omitempty"`
	Exported   bool     `json:"exported,omitempty"`
	ImportType string   `json:"importType,omitempty"`
	IPs        []string `json:"ips,omitempty"`
}

// Edge is a directed relationship between two nodes.
type Edge struct {
	From string   `json:"from"`
	To   string   `json:"to"`
	Kind EdgeKind `json:"kind"`

	// Phase annotates a peering edge with the peer's MeshPeer phase.
	Phase string `json:"phase,omitempty"`
	// Conflict marks a peering edge whose peer is named in a topology conflict.
	Conflict bool `json:"conflict,omitempty"`
	// Policy names the MeshNetworkPolicy that authorizes an allows edge.
	Policy string `json:"policy,omitempty"`
}

// Graph is the versioned, machine-readable mesh dependency graph. Nodes and
// edges are sorted by Build so the JSON and the renderings are byte-stable for a
// given snapshot regardless of input order.
type Graph struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`

	// GeneratedAt is copied from the snapshot's GeneratedAt so a graph can be
	// correlated with the snapshot it was derived from.
	GeneratedAt int64 `json:"generatedAt,omitempty"`

	Nodes []Node `json:"nodes"`
	Edges []Edge `json:"edges"`
}

// builder accumulates nodes and edges, deduplicating nodes by id and edges by
// their (from, to, kind) identity so repeated references merge cleanly.
type builder struct {
	nodes     map[string]*Node
	edgeSeen  map[string]bool
	edges     []Edge
	conflicts map[string]bool
}

// Build assembles the dependency graph from a snapshot. It is pure: the same
// snapshot always yields the same graph.
func Build(snap verify.Snapshot) Graph {
	b := &builder{
		nodes:     map[string]*Node{},
		edgeSeen:  map[string]bool{},
		conflicts: conflictClusters(snap.Conflicts),
	}

	// The local cluster always exists, even on an empty mesh, so the graph is
	// never nodeless.
	local := b.cluster(LocalNodeID)
	local.Local = true
	local.Label = localLabel

	for _, p := range snap.Peers {
		b.addPeer(p)
	}
	for _, im := range snap.Imports {
		b.addImport(im)
	}
	for _, ex := range snap.Exports {
		b.addExport(ex)
	}
	for _, p := range snap.Policies {
		b.addPolicy(p)
	}

	return Graph{
		APIVersion:  GraphAPIVersion,
		Kind:        GraphKind,
		GeneratedAt: snap.GeneratedAt,
		Nodes:       b.sortedNodes(),
		Edges:       b.sortedEdges(),
	}
}

// addPeer adds a peer cluster node and the local→peer peering edge.
func (b *builder) addPeer(p verify.PeerSnapshot) {
	n := b.cluster(clusterID(p.ClusterID))
	n.ClusterID = p.ClusterID
	n.Label = peerLabel(p.ClusterID)
	n.Phase = p.Phase
	n.Endpoint = p.Endpoint
	if b.conflicts[p.ClusterID] {
		n.Conflict = true
	}
	b.edge(Edge{From: LocalNodeID, To: n.ID, Kind: EdgePeering, Phase: p.Phase, Conflict: n.Conflict})
}

// addImport adds a service node imported into the local cluster, the
// service→local imports edge, and a serves edge from every contributing cluster.
func (b *builder) addImport(im verify.ImportSnapshot) {
	svc := b.service(im.Namespace, im.Name)
	svc.Imported = true
	svc.ImportType = im.Type
	svc.IPs = im.IPs
	b.edge(Edge{From: svc.ID, To: LocalNodeID, Kind: EdgeImports})
	for _, cid := range im.Clusters {
		c := b.cluster(clusterID(cid))
		if c.Label == "" {
			c.ClusterID = cid
			c.Label = peerLabel(cid)
		}
		b.edge(Edge{From: c.ID, To: svc.ID, Kind: EdgeServes})
	}
}

// addExport adds a service node exported by the local cluster and the
// local→service exports edge.
func (b *builder) addExport(ex verify.ExportSnapshot) {
	svc := b.service(ex.Namespace, ex.Name)
	svc.Exported = true
	b.edge(Edge{From: LocalNodeID, To: svc.ID, Kind: EdgeExports})
}

// addPolicy draws an allows edge from each cluster a MeshNetworkPolicy permits
// as an ingress source into the local cluster, annotated with the policy name.
// CIDR-only sources are not mesh clusters, so they produce no node; a selector
// naming a cluster with no MeshPeer still gets a node so the edge is not
// dangling — the same way a service provider does — and its missing peering
// edge signals that the allowed source is not actually peered.
func (b *builder) addPolicy(p verify.PolicySnapshot) {
	for _, rule := range p.Ingress {
		for _, src := range rule.From {
			for _, cid := range src.ClusterIDs {
				c := b.cluster(clusterID(cid))
				if c.Label == "" {
					c.ClusterID = cid
					c.Label = peerLabel(cid)
				}
				b.edge(Edge{From: c.ID, To: LocalNodeID, Kind: EdgeAllows, Policy: p.Name})
			}
		}
	}
}

// cluster returns the cluster node with the given id, creating it if absent.
func (b *builder) cluster(id string) *Node {
	if n, ok := b.nodes[id]; ok {
		return n
	}
	n := &Node{ID: id, Kind: NodeCluster}
	b.nodes[id] = n
	return n
}

// service returns the service node for namespace/name, creating it if absent.
func (b *builder) service(namespace, name string) *Node {
	id := serviceID(namespace, name)
	if n, ok := b.nodes[id]; ok {
		return n
	}
	n := &Node{ID: id, Kind: NodeService, Label: namespace + "/" + name, Namespace: namespace, Name: name}
	b.nodes[id] = n
	return n
}

// edge appends an edge unless one with the same (from, to, kind) is already
// present, so repeated references collapse to a single edge.
func (b *builder) edge(e Edge) {
	key := string(e.Kind) + "\x00" + e.From + "\x00" + e.To + "\x00" + e.Policy
	if b.edgeSeen[key] {
		return
	}
	b.edgeSeen[key] = true
	b.edges = append(b.edges, e)
}

// sortedNodes returns the nodes sorted by id for deterministic output.
func (b *builder) sortedNodes() []Node {
	out := make([]Node, 0, len(b.nodes))
	for _, n := range b.nodes {
		out = append(out, *n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// sortedEdges returns the edges sorted by kind, then endpoints, for stable
// output.
func (b *builder) sortedEdges() []Edge {
	out := append([]Edge(nil), b.edges...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		if out[i].From != out[j].From {
			return out[i].From < out[j].From
		}
		if out[i].To != out[j].To {
			return out[i].To < out[j].To
		}
		return out[i].Policy < out[j].Policy
	})
	return out
}

// conflictClusters indexes the cluster IDs named directly by a topology
// conflict, so a peering edge to a conflicted peer can be flagged. The detector
// canonicalizes an overlap to one of the two cluster IDs, so this catches the
// canonical side; the snapshot and `dwxctl diagnose` remain authoritative for
// the full conflict detail.
func conflictClusters(conflicts []verify.ConflictReport) map[string]bool {
	out := make(map[string]bool, len(conflicts))
	for _, c := range conflicts {
		if c.ClusterID != "" {
			out[c.ClusterID] = true
		}
	}
	return out
}

// JSON renders the graph as indented, stable JSON.
func (g Graph) JSON() ([]byte, error) {
	return json.MarshalIndent(g, "", "  ")
}

func clusterID(id string) string       { return "cluster/" + id }
func serviceID(ns, name string) string { return "svc/" + ns + "/" + name }

func peerLabel(id string) string {
	if id == "" {
		return "<unknown>"
	}
	return id
}
