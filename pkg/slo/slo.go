// Package slo reconciles the mesh's *expected* reachability (what topology and
// policy permit, from pkg/reach) against its *observed* connectivity (whether
// the tunnel is actually passing traffic) into per-cluster golden-signal
// verdicts. It is where intent meets reality: it flags the cases that matter
// most — a cluster that *should* be reachable but whose tunnel is dead, or one
// that policy *blocks* — so an operator (or an AI) sees not just the
// configuration but whether it is working.
//
// It is pure: a function of a reachability matrix and an observed-liveness
// signal, with no Kubernetes and no probing, so it is exhaustively table-tested
// like the rest of the pure tier. The observed signal is abstracted (Liveness)
// so this engine is independent of *how* liveness is measured: today it is the
// WireGuard handshake age the agent already records; the active synthetic
// prober (a per-cluster probe responder, design 0011) is a drop-in second
// provider of the same signal, reconciled by exactly this logic.
package slo

import (
	"encoding/json"
	"sort"

	"github.com/DataWerx/datawerx-mesh/pkg/reach"
)

const (
	// APIVersion identifies the connectivity report schema. Versioned like the
	// snapshot, graph, and reachability matrix.
	APIVersion = "mesh.datawerx.io/connectivity/v1alpha1"
	// Kind is the constant kind string carried on every report.
	Kind = "MeshConnectivity"
)

// Verdict is the reconciled golden-signal state for one cluster.
type Verdict string

const (
	// VerdictHealthy means policy and topology permit the cluster and the tunnel
	// is passing traffic — intent and reality agree.
	VerdictHealthy Verdict = "Healthy"
	// VerdictImpaired means the cluster *should* be reachable but the tunnel is
	// not live (a stale or absent handshake): the most important discrepancy —
	// configuration is correct, but traffic is not flowing.
	VerdictImpaired Verdict = "Impaired"
	// VerdictBlocked means mesh policy default-denies the cluster; unreachability
	// is intended, not a fault.
	VerdictBlocked Verdict = "Blocked"
	// VerdictDegraded means a CIDR conflict makes the cluster's routing ambiguous.
	VerdictDegraded Verdict = "Degraded"
	// VerdictDown means the peer is not connected at all.
	VerdictDown Verdict = "Down"
)

// Liveness is the observed connectivity signal for one cluster.
type Liveness struct {
	// HandshakeAge is seconds since the last WireGuard handshake; negative means
	// unknown or never (no clock, or no handshake yet).
	HandshakeAge int64
}

// live reports whether the tunnel looks like it is passing traffic: a handshake
// within staleAfter seconds. An unknown or absent handshake is not live.
func (l Liveness) live(staleAfter int64) bool {
	return l.HandshakeAge >= 0 && l.HandshakeAge <= staleAfter
}

// Signal is one cluster's reconciled connectivity state.
type Signal struct {
	Cluster string  `json:"cluster"`
	Verdict Verdict `json:"verdict"`
	// Expected is the reachability the topology and policy imply (from pkg/reach).
	Expected reach.Status `json:"expected"`
	// TunnelLive is whether a recent handshake was observed.
	TunnelLive bool `json:"tunnelLive"`
	// Reason is the grounded explanation of the verdict.
	Reason string `json:"reason"`
}

// Report is the versioned, deterministic connectivity report for one cluster.
type Report struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Signals    []Signal `json:"signals"`
}

// Healthy reports whether every cluster's verdict is non-faulty. Impaired is the
// only verdict that signals a real fault (Blocked/Degraded/Down are intended or
// already covered elsewhere), so this is true unless something should work and
// doesn't.
func (r Report) Healthy() bool {
	for _, s := range r.Signals {
		if s.Verdict == VerdictImpaired {
			return false
		}
	}
	return true
}

// JSON renders the report as indented, stable JSON.
func (r Report) JSON() ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}

// Assess reconciles the expected reachability matrix against observed liveness.
// staleAfter is how long since a handshake before the tunnel is considered not
// live (verify.StaleHandshakeSeconds in practice). The result is sorted by
// cluster for stable output.
func Assess(matrix reach.Matrix, liveness map[string]Liveness, staleAfter int64) Report {
	out := make([]Signal, 0, len(matrix.Reachabilities))
	for _, r := range matrix.Reachabilities {
		// A cluster absent from the liveness map is unknown, not live — distinct
		// from a present entry whose handshake age is 0 (just handshaked).
		lv, known := liveness[r.Cluster]
		out = append(out, signalFor(r, known && lv.live(staleAfter)))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Cluster < out[j].Cluster })
	return Report{APIVersion: APIVersion, Kind: Kind, Signals: out}
}

// signalFor reconciles one cluster's expected reachability with its tunnel
// liveness.
func signalFor(r reach.Reachability, live bool) Signal {
	s := Signal{Cluster: r.Cluster, Expected: r.Status, TunnelLive: live}
	switch r.Status {
	case reach.StatusUnreachable:
		s.Verdict = VerdictDown
		s.Reason = r.Reason
	case reach.StatusDegraded:
		s.Verdict = VerdictDegraded
		s.Reason = r.Reason
	case reach.StatusBlocked:
		s.Verdict = VerdictBlocked
		s.Reason = "mesh policy default-denies this cluster; its unreachability is intended, not a fault"
	case reach.StatusReachable:
		if live {
			s.Verdict = VerdictHealthy
			s.Reason = "policy and topology permit this cluster and the tunnel is passing traffic (recent handshake)"
		} else {
			s.Verdict = VerdictImpaired
			s.Reason = "policy and topology permit this cluster, but the WireGuard handshake is stale or absent — traffic is likely not flowing; check endpoint reachability, keys, and firewalls"
		}
	default:
		s.Verdict = VerdictDown
		s.Reason = r.Reason
	}
	return s
}
