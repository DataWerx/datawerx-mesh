package slo

import (
	"encoding/json"
	"testing"

	"github.com/DataWerx/datawerx-mesh/pkg/reach"
	"github.com/DataWerx/datawerx-mesh/pkg/verify"
)

const stale = verify.StaleHandshakeSeconds // 300

func sig(r Report, cluster string) (Signal, bool) {
	for _, s := range r.Signals {
		if s.Cluster == cluster {
			return s, true
		}
	}
	return Signal{}, false
}

func matrix(rs ...reach.Reachability) reach.Matrix {
	return reach.Matrix{APIVersion: reach.APIVersion, Kind: reach.Kind, Reachabilities: rs}
}

func TestAssess_ReachableAndLiveIsHealthy(t *testing.T) {
	m := matrix(reach.Reachability{Cluster: "east", Status: reach.StatusReachable})
	r := Assess(m, map[string]Liveness{"east": {HandshakeAge: 10}}, stale)
	s, _ := sig(r, "east")
	if s.Verdict != VerdictHealthy || !s.TunnelLive {
		t.Fatalf("want Healthy+live, got %+v", s)
	}
	if !r.Healthy() {
		t.Error("report should be Healthy")
	}
}

func TestAssess_ReachableButStaleIsImpaired(t *testing.T) {
	// The headline discrepancy: policy permits it, but the tunnel is dead.
	// A handshake older than the stale window, or never (-1), means not live.
	m := matrix(reach.Reachability{Cluster: "east", Status: reach.StatusReachable})
	for _, age := range []int64{stale + 1, -1} {
		r := Assess(m, map[string]Liveness{"east": {HandshakeAge: age}}, stale)
		s, _ := sig(r, "east")
		if s.Verdict != VerdictImpaired {
			t.Errorf("handshake age %d: want Impaired, got %s", age, s.Verdict)
		}
		if r.Healthy() {
			t.Errorf("handshake age %d: an Impaired report must not be Healthy", age)
		}
	}
	// Age 0 means a handshake just happened — that is live, so Healthy.
	r := Assess(m, map[string]Liveness{"east": {HandshakeAge: 0}}, stale)
	if s, _ := sig(r, "east"); s.Verdict != VerdictHealthy {
		t.Errorf("handshake age 0 (just handshaked) should be Healthy, got %s", s.Verdict)
	}
}

func TestAssess_MissingLivenessIsImpaired(t *testing.T) {
	// No liveness entry at all (peer never handshaked / not in the map).
	m := matrix(reach.Reachability{Cluster: "east", Status: reach.StatusReachable})
	r := Assess(m, nil, stale)
	s, _ := sig(r, "east")
	if s.Verdict != VerdictImpaired {
		t.Fatalf("absent liveness should be Impaired, got %s", s.Verdict)
	}
}

func TestAssess_BlockedIsIntended(t *testing.T) {
	// A blocked cluster is not a fault even with a live tunnel.
	m := matrix(reach.Reachability{Cluster: "west", Status: reach.StatusBlocked})
	r := Assess(m, map[string]Liveness{"west": {HandshakeAge: 5}}, stale)
	s, _ := sig(r, "west")
	if s.Verdict != VerdictBlocked {
		t.Fatalf("want Blocked, got %s", s.Verdict)
	}
	if !r.Healthy() {
		t.Error("a Blocked cluster is intended; report should be Healthy")
	}
}

func TestAssess_DegradedAndDownPassThrough(t *testing.T) {
	m := matrix(
		reach.Reachability{Cluster: "south", Status: reach.StatusDegraded, Reason: "overlap"},
		reach.Reachability{Cluster: "north", Status: reach.StatusUnreachable, Reason: "not connected"},
	)
	r := Assess(m, nil, stale)
	if s, _ := sig(r, "south"); s.Verdict != VerdictDegraded {
		t.Errorf("south: want Degraded, got %s", s.Verdict)
	}
	if s, _ := sig(r, "north"); s.Verdict != VerdictDown {
		t.Errorf("north: want Down, got %s", s.Verdict)
	}
}

func TestAssess_DeterministicAndSorted(t *testing.T) {
	m := matrix(
		reach.Reachability{Cluster: "west", Status: reach.StatusReachable},
		reach.Reachability{Cluster: "east", Status: reach.StatusReachable},
	)
	r := Assess(m, map[string]Liveness{"east": {HandshakeAge: 1}, "west": {HandshakeAge: 1}}, stale)
	if r.Signals[0].Cluster != "east" || r.Signals[1].Cluster != "west" {
		t.Errorf("signals not sorted: %+v", r.Signals)
	}
	a, _ := json.Marshal(r)
	b, _ := json.Marshal(Assess(m, map[string]Liveness{"east": {HandshakeAge: 1}, "west": {HandshakeAge: 1}}, stale))
	if string(a) != string(b) {
		t.Error("Assess is not deterministic")
	}
}

func TestFromSnapshot_EndToEnd(t *testing.T) {
	snap := verify.Snapshot{
		Peers: []verify.PeerSnapshot{
			// Connected, recent handshake, no policy → Healthy.
			{ClusterID: "east", Phase: "Connected", HandshakeAge: 12, PodCIDRs: []string{"10.10.0.0/16"}},
			// Connected, stale handshake, no policy → Reachable-but-Impaired.
			{ClusterID: "west", Phase: "Connected", HandshakeAge: stale + 100, PodCIDRs: []string{"10.20.0.0/16"}},
			// Pending → Down.
			{ClusterID: "south", Phase: "Pending", HandshakeAge: -1},
		},
	}
	r := FromSnapshot(snap)

	if s, _ := sig(r, "east"); s.Verdict != VerdictHealthy {
		t.Errorf("east: want Healthy, got %s (%s)", s.Verdict, s.Reason)
	}
	if s, _ := sig(r, "west"); s.Verdict != VerdictImpaired {
		t.Errorf("west: connected but stale → want Impaired, got %s", s.Verdict)
	}
	if s, _ := sig(r, "south"); s.Verdict != VerdictDown {
		t.Errorf("south: want Down, got %s", s.Verdict)
	}
	if r.Healthy() {
		t.Error("report has an Impaired cluster; must not be Healthy")
	}
}

// TestFromSnapshot_ProbeSupersedesHandshake is the writeback payoff: when the
// active prober has a recent observation, it decides liveness over the
// handshake. A peer whose tunnel handshook fine but whose probe is failing comes
// out Impaired, and a peer with a stale handshake but a fresh successful probe
// comes out Healthy — the sharper, application-layer verdict.
func TestFromSnapshot_ProbeSupersedesHandshake(t *testing.T) {
	snap := verify.Snapshot{
		Peers: []verify.PeerSnapshot{
			// Handshake fresh, but the probe is failing (Probed, ProbeAge -1).
			{ClusterID: "broken-app", Phase: "Connected", HandshakeAge: 5, Probed: true, ProbeAge: -1, PodCIDRs: []string{"10.10.0.0/16"}},
			// Handshake stale, but a fresh successful probe proves traffic flows.
			{ClusterID: "probe-ok", Phase: "Connected", HandshakeAge: stale + 100, Probed: true, ProbeAge: 8, PodCIDRs: []string{"10.20.0.0/16"}},
			// Not probed → handshake still decides (fresh → Healthy).
			{ClusterID: "no-probe", Phase: "Connected", HandshakeAge: 5, PodCIDRs: []string{"10.30.0.0/16"}},
		},
	}
	r := FromSnapshot(snap)

	if s, _ := sig(r, "broken-app"); s.Verdict != VerdictImpaired {
		t.Errorf("broken-app: fresh handshake but failing probe → want Impaired, got %s", s.Verdict)
	}
	if s, _ := sig(r, "probe-ok"); s.Verdict != VerdictHealthy {
		t.Errorf("probe-ok: stale handshake but fresh probe → want Healthy, got %s", s.Verdict)
	}
	if s, _ := sig(r, "no-probe"); s.Verdict != VerdictHealthy {
		t.Errorf("no-probe: unprobed, fresh handshake → want Healthy, got %s", s.Verdict)
	}
}

func TestReport_JSONRoundTrips(t *testing.T) {
	r := Assess(matrix(reach.Reachability{Cluster: "east", Status: reach.StatusReachable}),
		map[string]Liveness{"east": {HandshakeAge: 1}}, stale)
	out, err := r.JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	var round Report
	if err := json.Unmarshal(out, &round); err != nil {
		t.Fatalf("does not round-trip: %v", err)
	}
	if round.Kind != Kind || len(round.Signals) != 1 {
		t.Errorf("round-trip lost data: %+v", round)
	}
}
