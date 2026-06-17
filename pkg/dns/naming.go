// Package dns contains the pure, side-effect-free logic behind DataWerx Mesh's
// cross-cluster service discovery (MCS): clusterset.local name construction and
// the aggregation of many clusters' ServiceExports into a single desired
// ServiceImport.
//
// Like pkg/topology, nothing here touches the Kubernetes API or the network.
// This is the layer that carries the unit-test coverage; the export/import
// controllers are thin shells that gather inputs, call these functions, and
// perform side effects.
package dns

import (
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"

	mcsv1alpha1 "github.com/DataWerx/datawerx-mesh/pkg/apis/multicluster/v1alpha1"
)

// ClusterSetDomain is the DNS zone served for exported services, per the MCS
// API. A service "payments" in namespace "prod" is reachable mesh-wide at
// payments.prod.svc.clusterset.local.
const ClusterSetDomain = "clusterset.local"

// FQDN returns the fully-qualified clusterset.local name with trailing dot
// for an exported service. Inputs are assumed to already be valid DNS labels;
// callers validate at the API boundary.
func FQDN(name, namespace string) string {
	return fmt.Sprintf("%s.%s.svc.%s.", name, namespace, ClusterSetDomain)
}

// ExportedEndpoint is one cluster's contribution to an imported service: its
// declared type, ports, and reachable IPs - the remote ClusterIP for a
// ClusterSetIP service, or backing pod IPs for a headless one. It is the pure
// input to PlanServiceImport — the export controller produces these and shares
// them across the mesh via the ControlPlaneClient seam.
type ExportedEndpoint struct {
	Cluster string
	Type    mcsv1alpha1.ServiceImportType
	Ports   []mcsv1alpha1.ServicePort
	IPs     []string
	// ExportTime is the Unix-epoch-seconds creation time of the owning
	// ServiceExport. MCS conflict resolution is "oldest export wins", so this is
	// the primary sort key; 0 (unknown) sorts oldest and falls back to cluster ID.
	ExportTime int64
}

// ImportPlan is the deterministic desired state for a ServiceImport, computed
// from every cluster's ExportedEndpoint for a single namespaced name. It is
// fully comparable for table-driven tests.
type ImportPlan struct {
	// Exists is false when no cluster exports the service. The controller then
	// deletes any local ServiceImport.
	Exists bool
	// Type is the resolved consumption model. On a type disagreement the
	// lowest-cluster-ID export wins and the rest are reported in Conflicts.
	Type mcsv1alpha1.ServiceImportType
	// Ports is the merged, sorted port set across contributing clusters.
	Ports []mcsv1alpha1.ServicePort
	// AggregatedIPs is the sorted, de-duplicated union of advertised IPs from
	// contributing clusters. For a ClusterSetIP import the controller allocates
	// a single virtual IP instead; for headless these flow via EndpointSlices.
	// It is retained as a deterministic diagnostic/input to those steps.
	AggregatedIPs []string
	// Clusters is the sorted list of clusters whose type matched the winner and
	// therefore contribute to the import.
	Clusters []string
	// Conflicts holds human-readable descriptions of MCS conflicts type or
	// port-name disagreements for surfacing on ServiceExport status.
	Conflicts []string
}

// HasConflicts reports whether any export conflicted with the resolved plan.
func (p ImportPlan) HasConflicts() bool { return len(p.Conflicts) > 0 }

// PlanServiceImport aggregates the per-cluster exports of one namespaced
// service into a single desired ServiceImport, applying MCS conflict rules
// deterministically:
//
//   - Type must agree across clusters. Per the MCS spec the oldest export wins
//     (earliest ServiceExport creation time), with the lowest cluster ID as the
//     tie-breaker; exports with a different type are dropped and recorded as
//     conflicts.
//   - Ports are merged as a set keyed by port, protocol. A key seen with two
//     different names is a conflict; the same oldest-then-cluster order decides
//     which name wins.
//
// The result is order-independent with respect to the input slice.
func PlanServiceImport(exports []ExportedEndpoint) ImportPlan {
	if len(exports) == 0 {
		return ImportPlan{}
	}

	// Canonical MCS ordering: oldest export first, lowest cluster ID to break a
	// tie (including the common case where no export time is known, time 0).
	sorted := append([]ExportedEndpoint(nil), exports...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].ExportTime != sorted[j].ExportTime {
			return sorted[i].ExportTime < sorted[j].ExportTime
		}
		return sorted[i].Cluster < sorted[j].Cluster
	})

	plan := ImportPlan{Exists: true, Type: sorted[0].Type}

	portByKey := map[string]mcsv1alpha1.ServicePort{}
	var portKeys []string
	ipSet := map[string]struct{}{}

	for _, e := range sorted {
		if e.Type != plan.Type {
			plan.Conflicts = append(plan.Conflicts,
				fmt.Sprintf("cluster %q: type %q conflicts with resolved type %q", e.Cluster, e.Type, plan.Type))
			continue
		}
		plan.Clusters = append(plan.Clusters, e.Cluster)
		plan.mergePorts(e, portByKey, &portKeys)
		for _, ip := range e.IPs {
			ipSet[ip] = struct{}{}
		}
	}

	plan.Ports = sortedPorts(portByKey, portKeys)
	plan.AggregatedIPs = sortedIPs(ipSet)
	return plan
}

// mergePorts folds one export's ports into the shared dedup map, recording a
// conflict when a port/protocol key reappears under a different name. The
// first occurrence by cluster ID wins.
func (p *ImportPlan) mergePorts(e ExportedEndpoint, portByKey map[string]mcsv1alpha1.ServicePort, portKeys *[]string) {
	for _, port := range e.Ports {
		key := portKey(port)
		if existing, ok := portByKey[key]; ok {
			if existing.Name != port.Name {
				p.Conflicts = append(p.Conflicts,
					fmt.Sprintf("cluster %q: port %d/%s name %q conflicts with %q",
						e.Cluster, port.Port, protocolOf(port), port.Name, existing.Name))
			}
			continue
		}
		portByKey[key] = normalizePort(port)
		*portKeys = append(*portKeys, key)
	}
}

// sortedPorts materializes the merged ports in port, protocol order, or nil
// when there are none.
func sortedPorts(portByKey map[string]mcsv1alpha1.ServicePort, portKeys []string) []mcsv1alpha1.ServicePort {
	if len(portKeys) == 0 {
		return nil
	}
	ports := make([]mcsv1alpha1.ServicePort, 0, len(portKeys))
	for _, k := range portKeys {
		ports = append(ports, portByKey[k])
	}
	sort.Slice(ports, func(i, j int) bool {
		if ports[i].Port != ports[j].Port {
			return ports[i].Port < ports[j].Port
		}
		return protocolOf(ports[i]) < protocolOf(ports[j])
	})
	return ports
}

// sortedIPs returns the union of the aggregated IPs in sorted order, or nil
// when empty.
func sortedIPs(ipSet map[string]struct{}) []string {
	if len(ipSet) == 0 {
		return nil
	}
	ips := make([]string, 0, len(ipSet))
	for ip := range ipSet {
		ips = append(ips, ip)
	}
	sort.Strings(ips)
	return ips
}

// protocolOf returns the port's protocol, defaulting empty to TCP (the
// K8s default) so comparisons and keys are stable.
func protocolOf(p mcsv1alpha1.ServicePort) string {
	if strings.TrimSpace(string(p.Protocol)) == "" {
		return "TCP"
	}
	return string(p.Protocol)
}

// portKey is the de-duplication key for a port: its number and protocol.
func portKey(p mcsv1alpha1.ServicePort) string {
	return fmt.Sprintf("%d/%s", p.Port, protocolOf(p))
}

// normalizePort returns a copy of p with an explicit protocol so persisted
// ServiceImport ports never rely on the empty-string default.
func normalizePort(p mcsv1alpha1.ServicePort) mcsv1alpha1.ServicePort {
	out := p
	out.Protocol = corev1.Protocol(protocolOf(p))
	return out
}
