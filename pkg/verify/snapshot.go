package verify

import (
	"encoding/json"
	"sort"
)

// The mesh snapshot is the machine-readable record of a mesh's observed state.
// It is a strict superset of the `dwx mesh verify` health report. The same checks
// plus the structured objects behind them: peers, conflicts, exports, imports,
// policies, recent events, metric pointers. It exists for two reasons that
// happen to want the same artifact — observability hook an operator can
// pipe into jq, and a stable contract a higher layer can reason over. It
// stays pure data here and the decision logic that consumes it (Diagnose) lives
// alongside but separate.
//
// Assembly is a pure function of SnapshotInputs (gathered by the CLI from the
// API), so the whole thing is unit-testable with no cluster, exactly like the
// rest of pkg/verify.

const (
	// SnapshotAPIVersion identifies the snapshot schema. It is bumped when the
	// shape changes incompatibly so consumers — and a future hosted plane — can
	// branch on it. The vN suffix tracks the wire contract, not the product.
	SnapshotAPIVersion = "mesh.datawerx.io/snapshot/v1alpha1"

	// SnapshotKind is the constant kind string carried on every snapshot.
	SnapshotKind = "MeshSnapshot"
)

// Snapshot is the versioned, machine-readable view of a mesh's observed state.
// Field order is the marshal order; every slice is sorted by BuildSnapshot so
// the JSON is deterministic regardless of the order the API returned objects in.
type Snapshot struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`

	// GeneratedAt is the Unix epoch (seconds) the snapshot was assembled, copied
	// from SnapshotInputs.Now. Zero when the caller supplied no clock (tests).
	GeneratedAt int64 `json:"generatedAt,omitempty"`

	// Health is the same report `dwx mesh verify` renders, embedded so a snapshot
	// is a strict superset and a consumer never needs both.
	Health Report `json:"health"`

	Peers     []PeerSnapshot   `json:"peers"`
	Conflicts []ConflictReport `json:"conflicts"`
	Exports   []ExportSnapshot `json:"exports"`
	Imports   []ImportSnapshot `json:"imports"`
	Policies  []PolicySnapshot `json:"policies"`
	Events    []EventSnapshot  `json:"events"`
	Metrics   []MetricPointer  `json:"metrics"`
}

// PeerSnapshot is a MeshPeer's observed state. PublicKey is truncated — the
// snapshot never carries full key material; secrets are never logged.
type PeerSnapshot struct {
	Name         string   `json:"name"`
	ClusterID    string   `json:"clusterID"`
	Endpoint     string   `json:"endpoint,omitempty"`
	Phase        string   `json:"phase,omitempty"`
	PublicKey    string   `json:"publicKey,omitempty"`
	PodCIDRs     []string `json:"podCIDRs,omitempty"`
	ServiceCIDRs []string `json:"serviceCIDRs,omitempty"`
	// LastHandshake is the Unix epoch (seconds) of the last WireGuard handshake;
	// 0 means none yet.
	LastHandshake int64 `json:"lastHandshake,omitempty"`
	// HandshakeAge is seconds since the last handshake, or -1 when it cannot be
	// computed (no clock supplied, or no handshake yet). A diagnosis keys off it.
	HandshakeAge int64 `json:"handshakeAge"`
	// LastProbeAttempt is the Unix epoch (seconds) of the last active probe of
	// this peer, any outcome; 0 when never probed. Raw input behind Probed.
	LastProbeAttempt int64 `json:"lastProbeAttempt,omitempty"`
	// LastProbe is the Unix epoch (seconds) of the last successful probe; 0 when
	// none has succeeded.
	LastProbe int64 `json:"lastProbe,omitempty"`
	// Probed is whether active synthetic probing recently observed this peer.
	// When true, ProbeAge is the authoritative liveness age and supersedes the
	// handshake; when false, probing is off or stale and liveness uses the
	// handshake. Derived in BuildSnapshot from LastProbeAttempt and the clock.
	Probed bool `json:"probed,omitempty"`
	// ProbeAge is seconds since the last successful probe, or -1 when none has
	// succeeded or it cannot be computed. Mirrors HandshakeAge; meaningful only
	// when Probed.
	ProbeAge           int64  `json:"probeAge,omitempty"`
	Message            string `json:"message,omitempty"`
	ObservedGeneration int64  `json:"observedGeneration,omitempty"`
}

// ConflictReport is one topology conflict (mirrors topology.TopologyConflict),
// e.g. a CIDR overlap or a duplicate cluster ID across the advertised peers.
type ConflictReport struct {
	ClusterID string `json:"clusterID"`
	Reason    string `json:"reason"`
}

// ExportSnapshot is a ServiceExport's observed state.
type ExportSnapshot struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Valid     bool   `json:"valid"`
	Conflict  bool   `json:"conflict,omitempty"`
	Message   string `json:"message,omitempty"`
}

// ImportSnapshot is a ServiceImport's observed state.
type ImportSnapshot struct {
	Namespace string         `json:"namespace"`
	Name      string         `json:"name"`
	Type      string         `json:"type"`
	IPs       []string       `json:"ips,omitempty"`
	Ports     []PortSnapshot `json:"ports,omitempty"`
	// Clusters are the contributing exporting cluster IDs.
	Clusters []string `json:"clusters,omitempty"`
}

// PortSnapshot is one imported service port.
type PortSnapshot struct {
	Name     string `json:"name,omitempty"`
	Protocol string `json:"protocol,omitempty"`
	Port     int32  `json:"port"`
}

// PolicySnapshot is a MeshNetworkPolicy's observed state. Ingress carries the
// resolved allow rules — the sources permitted to reach the destinations — so a
// consumer can answer "which clusters may reach this one?" without re-reading the
// CRD. IngressRules stays as the rule count for back-compatible consumers that
// only want the cardinality.
type PolicySnapshot struct {
	Name         string                  `json:"name"`
	Destinations []string                `json:"destinations,omitempty"`
	Phase        string                  `json:"phase,omitempty"`
	IngressRules int                     `json:"ingressRules"`
	Ingress      []PolicyIngressSnapshot `json:"ingress,omitempty"`
	Message      string                  `json:"message,omitempty"`
}

// PolicyIngressSnapshot is one allow rule: traffic from any of From on any of
// Ports is permitted to the policy's destinations.
type PolicyIngressSnapshot struct {
	From  []PolicySourceSnapshot `json:"from,omitempty"`
	Ports []PortSnapshot         `json:"ports,omitempty"`
}

// PolicySourceSnapshot is one allowed source selector: mesh cluster IDs and/or
// explicit CIDRs whose ranges the rule permits.
type PolicySourceSnapshot struct {
	ClusterIDs []string `json:"clusterIDs,omitempty"`
	CIDRs      []string `json:"cidrs,omitempty"`
}

// EventSnapshot is a recent Kubernetes event touching a mesh object — the trail
// a diagnosis (or a human) follows when a peer last changed phase or a sync
// failed.
type EventSnapshot struct {
	Type     string `json:"type,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Message  string `json:"message,omitempty"`
	Object   string `json:"object,omitempty"`
	Count    int32  `json:"count,omitempty"`
	LastSeen int64  `json:"lastSeen,omitempty"`
}

// MetricPointer names a Prometheus series relevant to the mesh so a consumer
// knows where to look; Value carries the scraped value when the CLI gathered it.
type MetricPointer struct {
	Name  string  `json:"name"`
	Help  string  `json:"help,omitempty"`
	Value float64 `json:"value,omitempty"`
}

// SnapshotInputs is the read-only state the CLI gathers from the cluster. It
// carries both the primitives the health report needs (CRD/agent presence) and
// the structured objects, so BuildSnapshot is the single place that turns raw
// state into the contract — the CLI gathers once.
type SnapshotInputs struct {
	// Now is the current Unix time (seconds); drives handshake-age computation
	// and stamps GeneratedAt. Left 0 by tests that don't exercise staleness.
	Now int64

	RequiredCRDs []string
	PresentCRDs  map[string]bool
	AgentFound   bool
	AgentDesired int
	AgentReady   int

	Peers     []PeerSnapshot
	Conflicts []ConflictReport
	Exports   []ExportSnapshot
	Imports   []ImportSnapshot
	Policies  []PolicySnapshot
	Events    []EventSnapshot
	Metrics   []MetricPointer
}

// BuildSnapshot assembles a deterministic Snapshot from gathered state. It
// derives the embedded health Report from the same inputs, so the snapshot and
// `dwx mesh verify` never disagree, computes per-peer handshake age, truncates
// key material, and sorts every collection for stable output.
func BuildSnapshot(in SnapshotInputs) Snapshot {
	snap := Snapshot{
		APIVersion:  SnapshotAPIVersion,
		Kind:        SnapshotKind,
		GeneratedAt: in.Now,
		Health:      Build(in.healthInputs()),
		Peers:       append([]PeerSnapshot(nil), in.Peers...),
		Conflicts:   append([]ConflictReport(nil), in.Conflicts...),
		Exports:     append([]ExportSnapshot(nil), in.Exports...),
		Imports:     append([]ImportSnapshot(nil), in.Imports...),
		Policies:    append([]PolicySnapshot(nil), in.Policies...),
		Events:      append([]EventSnapshot(nil), in.Events...),
		Metrics:     append([]MetricPointer(nil), in.Metrics...),
	}

	for i := range snap.Peers {
		p := &snap.Peers[i]
		p.PublicKey = shortKey(p.PublicKey)
		p.HandshakeAge = handshakeAge(in.Now, p.LastHandshake)
		p.ProbeAge = handshakeAge(in.Now, p.LastProbe)
		// Trust the probe only while its last attempt is recent. If probing is
		// turned off, the peer reverts to handshake-based liveness on its own
		// once the last attempt ages past the stale window.
		p.Probed = in.Now > 0 && p.LastProbeAttempt > 0 && in.Now-p.LastProbeAttempt <= StaleHandshakeSeconds
	}

	sort.Slice(snap.Peers, func(i, j int) bool { return snap.Peers[i].ClusterID < snap.Peers[j].ClusterID })
	sort.Slice(snap.Conflicts, func(i, j int) bool { return lessConflict(snap.Conflicts[i], snap.Conflicts[j]) })
	sort.Slice(snap.Exports, func(i, j int) bool {
		return lessNamespaced(snap.Exports[i].Namespace, snap.Exports[i].Name, snap.Exports[j].Namespace, snap.Exports[j].Name)
	})
	sort.Slice(snap.Imports, func(i, j int) bool {
		return lessNamespaced(snap.Imports[i].Namespace, snap.Imports[i].Name, snap.Imports[j].Namespace, snap.Imports[j].Name)
	})
	sort.Slice(snap.Policies, func(i, j int) bool { return snap.Policies[i].Name < snap.Policies[j].Name })
	sort.Slice(snap.Events, func(i, j int) bool { return lessEvent(snap.Events[i], snap.Events[j]) })
	sort.Slice(snap.Metrics, func(i, j int) bool { return snap.Metrics[i].Name < snap.Metrics[j].Name })

	return snap
}

// JSON renders the snapshot as indented, stable JSON.
func (s Snapshot) JSON() ([]byte, error) {
	return json.MarshalIndent(s, "", "  ")
}

// healthInputs projects the snapshot inputs onto the narrower Inputs the health
// Report is built from, so there is one source of truth for both surfaces.
func (in SnapshotInputs) healthInputs() Inputs {
	hi := Inputs{
		RequiredCRDs: in.RequiredCRDs,
		PresentCRDs:  in.PresentCRDs,
		AgentFound:   in.AgentFound,
		AgentDesired: in.AgentDesired,
		AgentReady:   in.AgentReady,
		Now:          in.Now,
	}
	for _, p := range in.Peers {
		hi.Peers = append(hi.Peers, PeerInfo{ClusterID: p.ClusterID, Phase: p.Phase, LastHandshake: p.LastHandshake})
	}
	for _, e := range in.Exports {
		hi.Exports = append(hi.Exports, ExportInfo{Namespace: e.Namespace, Name: e.Name, Valid: e.Valid})
	}
	for _, im := range in.Imports {
		switch im.Type {
		case "ClusterSetIP":
			hi.ImportsClusterSetIP++
		case "Headless":
			hi.ImportsHeadless++
		}
	}
	return hi
}

// handshakeAge returns seconds since the last handshake, or -1 when it cannot be
// computed (no clock, or never handshaked). Clamped at 0 so clock skew that puts
// the handshake slightly in the future never reads as negative (which means
// "unknown").
func handshakeAge(now, last int64) int64 {
	if now <= 0 || last <= 0 {
		return -1
	}
	if age := now - last; age > 0 {
		return age
	}
	return 0
}

// shortKey truncates a WireGuard public key so full key material never lands in
// the snapshot (mirrors topology.shortKey / pkg/wg.shortKey).
func shortKey(k string) string {
	if len(k) <= 8 {
		return k
	}
	return k[:8] + "…"
}

func lessConflict(a, b ConflictReport) bool {
	if a.ClusterID != b.ClusterID {
		return a.ClusterID < b.ClusterID
	}
	return a.Reason < b.Reason
}

func lessNamespaced(ns1, n1, ns2, n2 string) bool {
	if ns1 != ns2 {
		return ns1 < ns2
	}
	return n1 < n2
}

func lessEvent(a, b EventSnapshot) bool {
	if a.Object != b.Object {
		return a.Object < b.Object
	}
	if a.Reason != b.Reason {
		return a.Reason < b.Reason
	}
	return a.Message < b.Message
}
