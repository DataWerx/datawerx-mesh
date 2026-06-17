// Package reach computes the mesh's expected cross-cluster reachability: for
// each remote cluster, can it reach into this cluster, and if not, why? It is
// the deterministic answer to "why can't A reach B" — the question every
// multi-cluster operator (and every AI agent pointed at the mesh) asks.
//
// It is pure: a function of the observed topology and policy, with no Kubernetes
// and no probing, so it is exhaustively table-tested like the rest of the pure
// tier (pkg/topology, pkg/verify, pkg/meshgraph, pkg/impact). It composes the
// real firewall compiler (meshfw.BuildFirewall + meshfw.Interpret), so its
// policy verdicts are provably consistent with what the data plane programs
// rather than a re-implementation of the rules.
//
// This is the *expected* reachability derived from declared state. The
// complementary *observed* reachability — active synthetic probes that dial a
// per-node responder — is its runtime counterpart in pkg/probe (design 0012);
// the two are meant to be compared, and this is the contract that makes a probe
// result interpretable.
package reach

import (
	"encoding/json"
	"sort"

	"github.com/DataWerx/datawerx-mesh/pkg/meshfw"
)

const (
	// APIVersion identifies the reachability matrix schema. Versioned like the
	// snapshot and graph so consumers can branch on it.
	APIVersion = "mesh.datawerx.io/reachability/v1alpha1"
	// Kind is the constant kind string carried on every matrix.
	Kind = "MeshReachability"
)

// Status is a remote cluster's expected ability to reach into the local cluster.
type Status string

const (
	// StatusReachable means the peer is connected and mesh policy permits its
	// ingress (or no policy restricts it).
	StatusReachable Status = "Reachable"
	// StatusBlocked means the peer is connected but every protected destination
	// default-denies it — policy shuts it out of the governed services.
	StatusBlocked Status = "Blocked"
	// StatusDegraded means the peer is connected but a topology conflict (a CIDR
	// overlap) makes its routing ambiguous, so reachability is unreliable.
	StatusDegraded Status = "Degraded"
	// StatusUnreachable means the peer is not connected at the data plane, so
	// nothing flows regardless of policy.
	StatusUnreachable Status = "Unreachable"
)

// DestReach is whether a peer may reach one protected local destination.
type DestReach struct {
	Dest    string `json:"dest"`
	Allowed bool   `json:"allowed"`
}

// Reachability is one remote cluster's expected reach into the local cluster.
type Reachability struct {
	Cluster string `json:"cluster"`
	Status  Status `json:"status"`
	// Reason is a human- and AI-readable explanation grounded in the signal that
	// decided the status (peer phase, conflict, or policy).
	Reason string `json:"reason"`
	// Dests is the per-destination breakdown for the protected local CIDRs, when
	// the peer is connected. Empty when nothing is protected (all ingress open).
	Dests []DestReach `json:"dests,omitempty"`
}

// Matrix is the versioned, deterministic reachability matrix for one cluster.
type Matrix struct {
	APIVersion     string         `json:"apiVersion"`
	Kind           string         `json:"kind"`
	Reachabilities []Reachability `json:"reachabilities"`
}

// JSON renders the matrix as indented, stable JSON.
func (m Matrix) JSON() ([]byte, error) {
	return json.MarshalIndent(m, "", "  ")
}

// Peer is a remote cluster's reachability-relevant state.
type Peer struct {
	ClusterID string
	// Connected is true when the peer's tunnel is up (MeshPeer phase Connected).
	Connected bool
	// Conflict is true when the peer is named in a topology conflict (overlap).
	Conflict bool
	// CIDRs are the peer's advertised ranges, used to match it against the
	// firewall's resolved source rules.
	CIDRs []string
}

// Input is the pure state Plan reasons over.
type Input struct {
	Peers    []Peer
	Policies []meshfw.Policy
	// ClusterCIDRs maps a cluster ID to its CIDRs, used by the firewall compiler
	// to resolve cluster-ID selectors. Usually built from Peers.
	ClusterCIDRs map[string][]string
}

// anySources are the firewall source values that match every peer.
var anySources = map[string]bool{"": true, "0.0.0.0/0": true, "::/0": true}

// Plan computes the reachability matrix. The result is sorted by cluster ID so
// it is byte-stable for a given input.
func Plan(in Input) Matrix {
	decisions := meshfw.Interpret(meshfw.BuildFirewall(in.Policies, in.ClusterCIDRs))
	protected := dedupeSorted(decisions.Protected)

	out := make([]Reachability, 0, len(in.Peers))
	for _, p := range in.Peers {
		out = append(out, reachFor(p, decisions.Accepts, protected))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Cluster < out[j].Cluster })

	return Matrix{APIVersion: APIVersion, Kind: Kind, Reachabilities: out}
}

// reachFor classifies one peer's reach into the local cluster.
func reachFor(p Peer, accepts []meshfw.Access, protected []string) Reachability {
	r := Reachability{Cluster: p.ClusterID}

	switch {
	case !p.Connected:
		r.Status = StatusUnreachable
		r.Reason = "peer is not connected; the tunnel is not established, so no traffic flows regardless of policy"
		return r
	case p.Conflict:
		r.Status = StatusDegraded
		r.Reason = "peer is connected but a CIDR overlap makes its routing ambiguous; resolve the conflict (renumber or enable remap) before reachability is reliable"
		return r
	}

	if len(protected) == 0 {
		r.Status = StatusReachable
		r.Reason = "connected and no MeshNetworkPolicy restricts ingress on this cluster; open by default"
		return r
	}

	allowed := 0
	for _, dest := range protected {
		ok := peerAllowed(p.CIDRs, accepts, dest)
		r.Dests = append(r.Dests, DestReach{Dest: destLabel(dest), Allowed: ok})
		if ok {
			allowed++
		}
	}

	switch {
	case allowed == 0:
		r.Status = StatusBlocked
		r.Reason = "connected, but every protected destination is default-deny and no ingress rule permits this cluster"
	case allowed == len(protected):
		r.Status = StatusReachable
		r.Reason = "connected and mesh policy permits this cluster to every protected destination"
	default:
		r.Status = StatusReachable
		r.Reason = "connected and mesh policy permits this cluster to some protected destinations; see the per-destination breakdown for the rest"
	}
	return r
}

// peerAllowed reports whether an accept rule permits the peer's ranges to reach
// dest. meshfw resolves a cluster-ID selector to that cluster's CIDRs, so the
// firewall's source values are compared directly against the peer's advertised
// CIDRs — no re-derivation of selector semantics.
func peerAllowed(peerCIDRs []string, accepts []meshfw.Access, dest string) bool {
	for _, a := range accepts {
		if a.Dest != "" && a.Dest != dest {
			continue
		}
		if anySources[a.Source] || contains(peerCIDRs, a.Source) {
			return true
		}
	}
	return false
}

func destLabel(d string) string {
	if d == "" {
		return "<all mesh ingress>"
	}
	return d
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func dedupeSorted(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
