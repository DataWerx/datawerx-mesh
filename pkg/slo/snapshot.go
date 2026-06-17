package slo

import (
	"github.com/DataWerx/datawerx-mesh/pkg/reach"
	"github.com/DataWerx/datawerx-mesh/pkg/verify"
)

// FromSnapshot builds the connectivity report from a mesh snapshot. The expected
// reachability comes from reach.FromSnapshot (topology + policy); the observed
// liveness comes from each peer's observed age. When the active prober has a
// recent observation for a peer (Probed), its probe age is authoritative — it
// proves a packet actually crossed; otherwise liveness falls back to the
// WireGuard handshake age the agent always records. Both halves derive from the
// one snapshot every read surface shares, so the report can never disagree with
// snapshot / reach / diagnose.
func FromSnapshot(snap verify.Snapshot) Report {
	liveness := make(map[string]Liveness, len(snap.Peers))
	for _, p := range snap.Peers {
		age := p.HandshakeAge
		if p.Probed {
			age = p.ProbeAge
		}
		liveness[p.ClusterID] = Liveness{HandshakeAge: age}
	}
	return Assess(reach.FromSnapshot(snap), liveness, verify.StaleHandshakeSeconds)
}
