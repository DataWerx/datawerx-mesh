package reach

import (
	"encoding/json"
	"testing"

	"github.com/DataWerx/datawerx-mesh/pkg/meshfw"
	"github.com/DataWerx/datawerx-mesh/pkg/verify"
)

func find(m Matrix, cluster string) (Reachability, bool) {
	for _, r := range m.Reachabilities {
		if r.Cluster == cluster {
			return r, true
		}
	}
	return Reachability{}, false
}

// allowPolicy protects dest and allows the given source cluster IDs.
func allowPolicy(name, dest string, fromClusters ...string) meshfw.Policy {
	return meshfw.Policy{
		Name:         name,
		Destinations: []string{dest},
		Ingress:      []meshfw.IngressRule{{From: []meshfw.PeerSelector{{ClusterIDs: fromClusters}}}},
	}
}

func TestPlan_NotConnectedIsUnreachable(t *testing.T) {
	m := Plan(Input{
		Peers: []Peer{{ClusterID: "east", Connected: false, CIDRs: []string{"10.10.0.0/16"}}},
	})
	r, _ := find(m, "east")
	if r.Status != StatusUnreachable {
		t.Fatalf("want Unreachable, got %s (%s)", r.Status, r.Reason)
	}
}

func TestPlan_ConflictIsDegraded(t *testing.T) {
	m := Plan(Input{
		Peers: []Peer{{ClusterID: "east", Connected: true, Conflict: true, CIDRs: []string{"10.10.0.0/16"}}},
	})
	r, _ := find(m, "east")
	if r.Status != StatusDegraded {
		t.Fatalf("want Degraded, got %s (%s)", r.Status, r.Reason)
	}
}

func TestPlan_NoPolicyIsOpen(t *testing.T) {
	m := Plan(Input{
		Peers: []Peer{{ClusterID: "east", Connected: true, CIDRs: []string{"10.10.0.0/16"}}},
	})
	r, _ := find(m, "east")
	if r.Status != StatusReachable {
		t.Fatalf("want Reachable, got %s (%s)", r.Status, r.Reason)
	}
	if len(r.Dests) != 0 {
		t.Errorf("no policy should mean no per-dest breakdown, got %+v", r.Dests)
	}
}

func TestPlan_PolicyAllowsAndBlocks(t *testing.T) {
	cidrs := map[string][]string{"east": {"10.10.0.0/16"}, "west": {"10.20.0.0/16"}}
	in := Input{
		Peers: []Peer{
			{ClusterID: "east", Connected: true, CIDRs: cidrs["east"]},
			{ClusterID: "west", Connected: true, CIDRs: cidrs["west"]},
		},
		Policies:     []meshfw.Policy{allowPolicy("ledger", "10.0.0.0/24", "east")},
		ClusterCIDRs: cidrs,
	}
	m := Plan(in)

	east, _ := find(m, "east")
	if east.Status != StatusReachable {
		t.Errorf("east is allowed, want Reachable, got %s (%s)", east.Status, east.Reason)
	}
	west, _ := find(m, "west")
	if west.Status != StatusBlocked {
		t.Errorf("west is not allowed to the only protected dest, want Blocked, got %s (%s)", west.Status, west.Reason)
	}
	// Per-destination breakdown is present and correct.
	if len(east.Dests) != 1 || !east.Dests[0].Allowed || east.Dests[0].Dest != "10.0.0.0/24" {
		t.Errorf("east dest breakdown wrong: %+v", east.Dests)
	}
	if len(west.Dests) != 1 || west.Dests[0].Allowed {
		t.Errorf("west dest breakdown wrong: %+v", west.Dests)
	}
}

func TestPlan_PartialReachableWhenAllowedToSomeDests(t *testing.T) {
	cidrs := map[string][]string{"east": {"10.10.0.0/16"}}
	in := Input{
		Peers: []Peer{{ClusterID: "east", Connected: true, CIDRs: cidrs["east"]}},
		Policies: []meshfw.Policy{
			allowPolicy("a", "10.0.0.0/24", "east"), // east allowed here
			allowPolicy("b", "10.1.0.0/24", "west"), // east NOT allowed here (protected, denies east)
		},
		ClusterCIDRs: cidrs,
	}
	r, _ := find(Plan(in), "east")
	if r.Status != StatusReachable {
		t.Fatalf("partial allow should still be Reachable, got %s", r.Status)
	}
	allowed, denied := 0, 0
	for _, d := range r.Dests {
		if d.Allowed {
			allowed++
		} else {
			denied++
		}
	}
	if allowed != 1 || denied != 1 {
		t.Errorf("expected one allowed and one denied dest, got %+v", r.Dests)
	}
}

func TestPlan_AnySourceAllowReachesEveryone(t *testing.T) {
	cidrs := map[string][]string{"east": {"10.10.0.0/16"}}
	in := Input{
		Peers: []Peer{{ClusterID: "east", Connected: true, CIDRs: cidrs["east"]}},
		Policies: []meshfw.Policy{{
			Name:         "open",
			Destinations: []string{"10.0.0.0/24"},
			Ingress:      []meshfw.IngressRule{{From: []meshfw.PeerSelector{{CIDRs: []string{"0.0.0.0/0"}}}}},
		}},
		ClusterCIDRs: cidrs,
	}
	r, _ := find(Plan(in), "east")
	if r.Status != StatusReachable {
		t.Errorf("an any-source allow should make east Reachable, got %s", r.Status)
	}
}

func TestPlan_DeterministicAndSorted(t *testing.T) {
	in := Input{Peers: []Peer{
		{ClusterID: "west", Connected: true},
		{ClusterID: "east", Connected: true},
	}}
	m := Plan(in)
	if m.Reachabilities[0].Cluster != "east" || m.Reachabilities[1].Cluster != "west" {
		t.Errorf("reachabilities not sorted by cluster: %+v", m.Reachabilities)
	}
	a, _ := json.Marshal(Plan(in))
	b, _ := json.Marshal(Plan(in))
	if string(a) != string(b) {
		t.Error("Plan is not deterministic")
	}
	if m.APIVersion != APIVersion || m.Kind != Kind {
		t.Errorf("bad envelope: %s/%s", m.APIVersion, m.Kind)
	}
}

func TestMatrix_JSONRoundTrips(t *testing.T) {
	m := Plan(Input{Peers: []Peer{{ClusterID: "east", Connected: true}}})
	out, err := m.JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	var round Matrix
	if err := json.Unmarshal(out, &round); err != nil {
		t.Fatalf("matrix JSON does not round-trip: %v", err)
	}
	if round.Kind != Kind || len(round.Reachabilities) != 1 {
		t.Errorf("round-trip lost data: %+v", round)
	}
}

func TestFromSnapshot_EndToEnd(t *testing.T) {
	snap := verify.Snapshot{
		Peers: []verify.PeerSnapshot{
			{ClusterID: "east", Phase: "Connected", PodCIDRs: []string{"10.10.0.0/16"}},
			{ClusterID: "west", Phase: "Pending", PodCIDRs: []string{"10.20.0.0/16"}},
			{ClusterID: "south", Phase: "Connected", PodCIDRs: []string{"10.30.0.0/16"}},
		},
		Conflicts: []verify.ConflictReport{{ClusterID: "south", Reason: "overlap"}},
		Policies: []verify.PolicySnapshot{{
			Name:         "ledger",
			Destinations: []string{"10.0.0.0/24"},
			Ingress: []verify.PolicyIngressSnapshot{
				{From: []verify.PolicySourceSnapshot{{ClusterIDs: []string{"east"}}}},
			},
		}},
	}
	m := FromSnapshot(snap)

	east, _ := find(m, "east")
	if east.Status != StatusReachable {
		t.Errorf("east connected + allowed: want Reachable, got %s (%s)", east.Status, east.Reason)
	}
	west, _ := find(m, "west")
	if west.Status != StatusUnreachable {
		t.Errorf("west pending: want Unreachable, got %s", west.Status)
	}
	south, _ := find(m, "south")
	if south.Status != StatusDegraded {
		t.Errorf("south overlaps: want Degraded, got %s", south.Status)
	}
}
