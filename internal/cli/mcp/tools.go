package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/DataWerx/datawerx-mesh/pkg/meshgraph"
	"github.com/DataWerx/datawerx-mesh/pkg/reach"
	"github.com/DataWerx/datawerx-mesh/pkg/slo"
	"github.com/DataWerx/datawerx-mesh/pkg/verify"
)

// tool is one read-only MCP tool. run derives its answer purely from a gathered
// snapshot — there is no tool that takes an action.
type tool struct {
	description string
	run         func(verify.Snapshot) (string, error)
}

// tools is the read-only tool set. Every tool reads from the snapshot, so they
// share one source of truth with `dwx mesh snapshot` and can never drift from it.
var tools = map[string]tool{
	"mesh_snapshot": {
		description: "Returns the full versioned mesh state snapshot as JSON (peers, conflicts, service exports/imports, policies, health, recent events).",
		run: func(s verify.Snapshot) (string, error) {
			return jsonString(s)
		},
	},
	"mesh_health": {
		description: "Returns the mesh health report: the pass/warn/fail checks for CRDs, the agent DaemonSet, peers, handshakes, and service exports/imports.",
		run: func(s verify.Snapshot) (string, error) {
			return jsonString(s.Health)
		},
	},
	"mesh_diagnose": {
		description: "Run the rule-based 'obvious cause' checker over the mesh and return the grounded findings (most severe first), each citing the concrete signal it read.",
		run: func(s verify.Snapshot) (string, error) {
			return jsonString(verify.Diagnose(s))
		},
	},
	"mesh_graph": {
		description: "Returns the mesh dependency graph as JSON: nodes for this cluster, its peers, and the imported/exported services, with edges for peerings and service flows. Derived from the same snapshot, so it never disagrees with mesh_snapshot.",
		run: func(s verify.Snapshot) (string, error) {
			return jsonString(meshgraph.Build(s))
		},
	},
	"mesh_reachability": {
		description: "Answers 'why can't cluster A reach this one?'. For each remote cluster returns its expected reachability into this cluster (Reachable, Blocked, Degraded, or Unreachable) with the grounded reason — peer phase, a CIDR conflict, or a default-deny policy — plus a per-destination breakdown. Composes the real firewall compiler, so verdicts match the data plane.",
		run: func(s verify.Snapshot) (string, error) {
			return jsonString(reach.FromSnapshot(s))
		},
	},
	"mesh_connectivity": {
		description: "Connectivity golden signals: reconciles each cluster's expected reachability against the observed tunnel liveness (WireGuard handshake) into one verdict — Healthy, Impaired, Blocked, Degraded, or Down. Impaired is the key fault: the cluster should be reachable, but the tunnel is not passing traffic. Tells you not just the config but whether it is working.",
		run: func(s verify.Snapshot) (string, error) {
			return jsonString(slo.FromSnapshot(s))
		},
	},
	"list_peers": {
		description: "List the mesh peers with their phase, endpoint, advertised CIDRs, and handshake age.",
		run: func(s verify.Snapshot) (string, error) {
			return jsonString(s.Peers)
		},
	},
	"list_service_imports": {
		description: "List the services imported into this cluster from the mesh (name, type, ClusterSetIP(s), ports, contributing clusters).",
		run: func(s verify.Snapshot) (string, error) {
			return jsonString(s.Imports)
		},
	},
	"list_service_exports": {
		description: "List the services this cluster exports to the mesh and whether each is valid or in conflict.",
		run: func(s verify.Snapshot) (string, error) {
			return jsonString(s.Exports)
		},
	},
}

// toolDescriptors renders the tool set for tools/list, sorted by name so the
// listing is stable. Every tool takes no arguments (an empty object schema).
func toolDescriptors() []map[string]any {
	names := make([]string, 0, len(tools))
	for n := range tools {
		names = append(names, n)
	}
	sort.Strings(names)

	out := make([]map[string]any, 0, len(names))
	for _, n := range names {
		out = append(out, map[string]any{
			"name":        n,
			"description": tools[n].description,
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		})
	}
	return out
}

// jsonString renders v as indented JSON. HTML escaping is disabled so CIDRs and
// messages read naturally in the tool output.
func jsonString(v any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return "", fmt.Errorf("encoding tool result: %w", err)
	}
	return buf.String(), nil
}
