package meshstate

import (
	"context"
	"fmt"
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	mcsv1alpha1 "github.com/DataWerx/datawerx-mesh/pkg/apis/multicluster/v1alpha1"
	networkingv1alpha1 "github.com/DataWerx/datawerx-mesh/pkg/apis/networking/v1alpha1"
	"github.com/DataWerx/datawerx-mesh/pkg/topology"
	"github.com/DataWerx/datawerx-mesh/pkg/verify"
)

// maxEvents caps how many recent Warning events the snapshot carries. The events
// block is corroborating signal for a diagnosis, not an audit log, so the most
// recent handful is enough and keeps the artifact small.
const maxEvents = 50

// Snapshot gathers the cluster's observed mesh state and assembles the versioned
// verify.Snapshot. The reads it issues are authoritative for the core objects
// (CRDs, the agent, peers, exports, imports, policies); the recent-events block
// is best-effort, because an operator may legitimately lack cluster-wide event
// read permission and a missing event trail must not fail an otherwise valid
// snapshot. Assembly, sorting, key truncation, and the embedded health report
// all happen in the pure verify.BuildSnapshot.
func Snapshot(ctx context.Context, c client.Client, namespace, daemonset string) (verify.Snapshot, error) {
	in := verify.SnapshotInputs{
		Now:          time.Now().Unix(),
		RequiredCRDs: verify.RequiredCRDs(),
		Metrics:      metricPointers(),
	}

	present, err := gatherCRDs(ctx, c, in.RequiredCRDs)
	if err != nil {
		return verify.Snapshot{}, err
	}
	in.PresentCRDs = present

	if err := gatherAgent(ctx, c, namespace, daemonset, &in); err != nil {
		return verify.Snapshot{}, err
	}

	peers, identities, err := gatherPeers(ctx, c)
	if err != nil {
		return verify.Snapshot{}, err
	}
	in.Peers = peers
	in.Conflicts = topologyConflicts(identities)

	if in.Exports, err = gatherExports(ctx, c); err != nil {
		return verify.Snapshot{}, err
	}
	if in.Imports, err = gatherImports(ctx, c); err != nil {
		return verify.Snapshot{}, err
	}
	if in.Policies, err = gatherPolicies(ctx, c); err != nil {
		return verify.Snapshot{}, err
	}

	// Events are corroborating, not load-bearing: ignore a read failure so a
	// restricted RBAC scope still yields a usable snapshot of everything else.
	in.Events = gatherEvents(ctx, c)

	return verify.BuildSnapshot(in), nil
}

// gatherCRDs probes each required CRD by name and records its presence. A
// NotFound is an absent CRD (recorded false), not an error; any other failure is
// a real read problem and propagates.
func gatherCRDs(ctx context.Context, c client.Client, required []string) (map[string]bool, error) {
	present := make(map[string]bool, len(required))
	for _, name := range required {
		var crd apiextensionsv1.CustomResourceDefinition
		switch err := c.Get(ctx, types.NamespacedName{Name: name}, &crd); {
		case apierrors.IsNotFound(err):
			present[name] = false
		case err != nil:
			return nil, fmt.Errorf("reading CRD %q: %w", name, err)
		default:
			present[name] = true
		}
	}
	return present, nil
}

// gatherAgent reads the agent DaemonSet's desired/ready replica counts. A
// missing DaemonSet is reported as "not found" rather than an error so the
// health check can render the failure instead of the command aborting.
func gatherAgent(ctx context.Context, c client.Client, namespace, daemonset string, in *verify.SnapshotInputs) error {
	var ds appsv1.DaemonSet
	switch err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: daemonset}, &ds); {
	case apierrors.IsNotFound(err):
		in.AgentFound = false
	case err != nil:
		return fmt.Errorf("reading agent DaemonSet %s/%s: %w", namespace, daemonset, err)
	default:
		in.AgentFound = true
		in.AgentDesired = int(ds.Status.DesiredNumberScheduled)
		in.AgentReady = int(ds.Status.NumberReady)
	}
	return nil
}

// gatherPeers lists the MeshPeers and projects them onto both the PeerSnapshot
// records the snapshot carries and the topology.PeerIdentity set conflict
// detection runs over. Full key material is passed through; BuildSnapshot
// truncates it, so the snapshot never carries a whole key.
func gatherPeers(ctx context.Context, c client.Client) ([]verify.PeerSnapshot, []topology.PeerIdentity, error) {
	var list networkingv1alpha1.MeshPeerList
	if err := c.List(ctx, &list); err != nil {
		return nil, nil, fmt.Errorf("listing MeshPeers: %w", err)
	}
	peers := make([]verify.PeerSnapshot, 0, len(list.Items))
	identities := make([]topology.PeerIdentity, 0, len(list.Items))
	for i := range list.Items {
		p := &list.Items[i]
		peers = append(peers, verify.PeerSnapshot{
			Name:               p.Name,
			ClusterID:          p.Spec.ClusterID,
			Endpoint:           p.Spec.Endpoint,
			Phase:              string(p.Status.Phase),
			PublicKey:          p.Spec.PublicKey,
			PodCIDRs:           p.Spec.PodCIDRs,
			ServiceCIDRs:       p.Spec.ServiceCIDRs,
			LastHandshake:      p.Status.LastHandshakeTime,
			LastProbeAttempt:   p.Status.LastProbeAttempt,
			LastProbe:          p.Status.LastProbeTime,
			Message:            p.Status.Message,
			ObservedGeneration: p.Status.ObservedGeneration,
		})
		identities = append(identities, topology.PeerIdentity{
			ClusterID: p.Spec.ClusterID,
			PublicKey: p.Spec.PublicKey,
			Endpoint:  p.Spec.Endpoint,
			CIDRs:     p.Spec.AllCIDRs(),
		})
	}
	return peers, identities, nil
}

// topologyConflicts runs the same pure detector the syncer uses, so the
// snapshot's conflict list and the data plane's view of the topology are
// computed by one function.
func topologyConflicts(identities []topology.PeerIdentity) []verify.ConflictReport {
	conflicts := topology.DetectTopologyConflicts(identities)
	out := make([]verify.ConflictReport, 0, len(conflicts))
	for _, c := range conflicts {
		out = append(out, verify.ConflictReport{ClusterID: c.ClusterID, Reason: c.Reason})
	}
	return out
}

// gatherExports lists the ServiceExports and reads their Valid/Conflict
// conditions. An export is valid only when its Valid condition is True; the
// message prefers the reason the export is not valid, then any conflict detail.
func gatherExports(ctx context.Context, c client.Client) ([]verify.ExportSnapshot, error) {
	var list mcsv1alpha1.ServiceExportList
	if err := c.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("listing ServiceExports: %w", err)
	}
	out := make([]verify.ExportSnapshot, 0, len(list.Items))
	for i := range list.Items {
		e := &list.Items[i]
		valid := apimeta.IsStatusConditionTrue(e.Status.Conditions, mcsv1alpha1.ServiceExportValid)
		conflict := apimeta.IsStatusConditionTrue(e.Status.Conditions, mcsv1alpha1.ServiceExportConflict)
		out = append(out, verify.ExportSnapshot{
			Namespace: e.Namespace,
			Name:      e.Name,
			Valid:     valid,
			Conflict:  conflict,
			Message:   exportMessage(e, valid, conflict),
		})
	}
	return out, nil
}

// exportMessage surfaces the most useful condition detail: why the export is not
// valid, otherwise the conflict reason, otherwise nothing.
func exportMessage(e *mcsv1alpha1.ServiceExport, valid, conflict bool) string {
	if !valid {
		if cond := apimeta.FindStatusCondition(e.Status.Conditions, mcsv1alpha1.ServiceExportValid); cond != nil {
			return cond.Message
		}
	}
	if conflict {
		if cond := apimeta.FindStatusCondition(e.Status.Conditions, mcsv1alpha1.ServiceExportConflict); cond != nil {
			return cond.Message
		}
	}
	return ""
}

// gatherImports lists the resolved ServiceImports and projects their type,
// ClusterSetIP(s), ports, and contributing clusters.
func gatherImports(ctx context.Context, c client.Client) ([]verify.ImportSnapshot, error) {
	var list mcsv1alpha1.ServiceImportList
	if err := c.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("listing ServiceImports: %w", err)
	}
	out := make([]verify.ImportSnapshot, 0, len(list.Items))
	for i := range list.Items {
		im := &list.Items[i]
		ports := make([]verify.PortSnapshot, 0, len(im.Spec.Ports))
		for _, p := range im.Spec.Ports {
			ports = append(ports, verify.PortSnapshot{
				Name:     p.Name,
				Protocol: string(p.Protocol),
				Port:     p.Port,
			})
		}
		clusters := make([]string, 0, len(im.Status.Clusters))
		for _, cs := range im.Status.Clusters {
			clusters = append(clusters, cs.Cluster)
		}
		out = append(out, verify.ImportSnapshot{
			Namespace: im.Namespace,
			Name:      im.Name,
			Type:      string(im.Spec.Type),
			IPs:       im.Spec.IPs,
			Ports:     ports,
			Clusters:  clusters,
		})
	}
	return out, nil
}

// gatherPolicies lists the MeshNetworkPolicies and projects their destinations,
// phase, ingress rule count, and message.
func gatherPolicies(ctx context.Context, c client.Client) ([]verify.PolicySnapshot, error) {
	var list networkingv1alpha1.MeshNetworkPolicyList
	if err := c.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("listing MeshNetworkPolicies: %w", err)
	}
	out := make([]verify.PolicySnapshot, 0, len(list.Items))
	for i := range list.Items {
		p := &list.Items[i]
		out = append(out, verify.PolicySnapshot{
			Name:         p.Name,
			Destinations: p.Spec.Destinations,
			Phase:        string(p.Status.Phase),
			IngressRules: len(p.Spec.Ingress),
			Ingress:      policyIngress(p.Spec.Ingress),
			Message:      p.Status.Message,
		})
	}
	return out, nil
}

// policyIngress projects a policy's ingress allow rules, sorting the selector
// sets so the snapshot is deterministic regardless of the order the API returns
// the set-typed clusterIDs/CIDRs in.
func policyIngress(rules []networkingv1alpha1.MeshIngressRule) []verify.PolicyIngressSnapshot {
	if len(rules) == 0 {
		return nil
	}
	out := make([]verify.PolicyIngressSnapshot, 0, len(rules))
	for _, r := range rules {
		var rule verify.PolicyIngressSnapshot
		for _, f := range r.From {
			rule.From = append(rule.From, verify.PolicySourceSnapshot{
				ClusterIDs: sortedCopy(f.ClusterIDs),
				CIDRs:      sortedCopy(f.CIDRs),
			})
		}
		for _, pt := range r.Ports {
			rule.Ports = append(rule.Ports, verify.PortSnapshot{Protocol: pt.Protocol, Port: pt.Port})
		}
		out = append(out, rule)
	}
	return out
}

// sortedCopy returns a sorted copy of s, leaving the input untouched.
func sortedCopy(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

// meshObjectKinds is the set of involved-object kinds whose Warning events the
// snapshot surfaces — the mesh's own objects, so unrelated cluster noise stays
// out of the diagnosis trail.
var meshObjectKinds = map[string]bool{
	"MeshPeer":          true,
	"MeshNetworkPolicy": true,
	"ServiceExport":     true,
	"ServiceImport":     true,
	"EndpointExport":    true,
}

// gatherEvents collects recent Warning events touching mesh objects. It is
// best-effort: any read failure yields no events rather than an error, since the
// trail is corroborating signal and an operator may lack cluster-wide event
// access. The newest maxEvents are kept; BuildSnapshot does the final ordering.
func gatherEvents(ctx context.Context, c client.Client) []verify.EventSnapshot {
	var list corev1.EventList
	if err := c.List(ctx, &list); err != nil {
		return nil
	}
	out := make([]verify.EventSnapshot, 0)
	for i := range list.Items {
		e := &list.Items[i]
		if e.Type != corev1.EventTypeWarning || !meshObjectKinds[e.InvolvedObject.Kind] {
			continue
		}
		out = append(out, verify.EventSnapshot{
			Type:     e.Type,
			Reason:   e.Reason,
			Message:  e.Message,
			Object:   eventObject(e.InvolvedObject),
			Count:    e.Count,
			LastSeen: eventLastSeen(e),
		})
	}
	return mostRecentEvents(out, maxEvents)
}

// eventObject renders an involved object as Kind/namespace/name, dropping the
// namespace segment for cluster-scoped objects (MeshPeer, MeshNetworkPolicy).
func eventObject(ref corev1.ObjectReference) string {
	if ref.Namespace == "" {
		return ref.Kind + "/" + ref.Name
	}
	return ref.Kind + "/" + ref.Namespace + "/" + ref.Name
}

// eventLastSeen returns the Unix time of the most recent occurrence, preferring
// LastTimestamp and falling back to the event series' or the creation time so a
// modern eventTime-only event still carries a timestamp.
func eventLastSeen(e *corev1.Event) int64 {
	switch {
	case !e.LastTimestamp.IsZero():
		return e.LastTimestamp.Unix()
	case e.Series != nil && !e.Series.LastObservedTime.IsZero():
		return e.Series.LastObservedTime.Unix()
	case !e.EventTime.IsZero():
		return e.EventTime.Unix()
	default:
		return e.CreationTimestamp.Unix()
	}
}

// mostRecentEvents keeps the newest limit events by LastSeen. It trims before
// BuildSnapshot, which re-sorts by object for stable output, so the cap selects
// recency while the final order stays deterministic.
func mostRecentEvents(events []verify.EventSnapshot, limit int) []verify.EventSnapshot {
	if len(events) <= limit {
		return events
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].LastSeen > events[j].LastSeen })
	return events[:limit]
}

// metricPointers names the Prometheus series relevant to the mesh so a snapshot
// consumer knows where to look. Per design 0005 the snapshot carries pointers,
// not scraped values; the series are exposed by the agent's /metrics endpoint.
func metricPointers() []verify.MetricPointer {
	return []verify.MetricPointer{
		{Name: "dwx_meshpeers", Help: "Number of MeshPeers by status phase."},
		{Name: "dwx_meshpeer_last_handshake_timestamp_seconds", Help: "Unix time of each MeshPeer's last WireGuard handshake."},
		{Name: "dwx_serviceexports", Help: "Number of ServiceExports by Valid condition."},
		{Name: "dwx_serviceimports", Help: "Number of ServiceImports by type."},
		{Name: "dwx_endpointexports", Help: "Number of EndpointExport objects."},
		{Name: "dwx_dns_queries_total", Help: "Total clusterset.local DNS queries answered, by response code."},
		{Name: "dwx_clusterset_nat_syncs_total", Help: "Total ClusterSetIP NAT data-plane syncs, by result."},
		{Name: "dwx_remap_syncs_total", Help: "Total overlap NETMAP data-plane syncs, by result."},
		{Name: "dwx_remap_active_entries", Help: "Number of overlapping-CIDR NETMAP entries currently programmed."},
	}
}
