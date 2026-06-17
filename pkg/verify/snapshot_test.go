package verify_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/DataWerx/datawerx-mesh/pkg/verify"
)

func fullSnapshotInputs() verify.SnapshotInputs {
	return verify.SnapshotInputs{
		Now:          1_000_000,
		RequiredCRDs: verify.RequiredCRDs(),
		PresentCRDs:  allCRDsPresent(),
		AgentFound:   true, AgentDesired: 3, AgentReady: 3,
		Peers: []verify.PeerSnapshot{
			{Name: "cluster-b", ClusterID: "b", Phase: "Connected", PublicKey: "ABCDEFGHIJKLMNOP", LastHandshake: 1_000_000 - 30, PodCIDRs: []string{"10.50.0.0/16"}},
			{Name: "cluster-a", ClusterID: "a", Phase: "Error", Message: "CIDR overlap with local cluster requires NAT remap"},
		},
		Conflicts: []verify.ConflictReport{{ClusterID: "a", Reason: `CIDR 10.244.0.0/16 overlaps cluster "b"`}},
		Exports:   []verify.ExportSnapshot{{Namespace: "prod", Name: "payments", Valid: true}},
		Imports:   []verify.ImportSnapshot{{Namespace: "prod", Name: "ledger", Type: "ClusterSetIP", IPs: []string{"241.0.0.5"}}},
		Policies:  []verify.PolicySnapshot{{Name: "lock-db", Destinations: []string{"10.0.0.0/24"}, Phase: "Ready", IngressRules: 1}},
		Metrics:   []verify.MetricPointer{{Name: "dwx_remap_active_entries", Value: 2}},
	}
}

func TestBuildSnapshot_VersionedAndDeterministic(t *testing.T) {
	snap := verify.BuildSnapshot(fullSnapshotInputs())

	if snap.APIVersion != verify.SnapshotAPIVersion || snap.Kind != verify.SnapshotKind {
		t.Fatalf("missing version/kind: %q %q", snap.APIVersion, snap.Kind)
	}
	if snap.GeneratedAt != 1_000_000 {
		t.Errorf("GeneratedAt = %d, want 1000000", snap.GeneratedAt)
	}
	// Peers sorted by clusterID regardless of input order.
	if snap.Peers[0].ClusterID != "a" || snap.Peers[1].ClusterID != "b" {
		t.Errorf("peers not sorted by clusterID: %+v", snap.Peers)
	}
	// Embedded health must reflect the Error peer as a failed peers check.
	if !snap.Health.Failed() {
		t.Errorf("embedded health should fail with an Error peer: %+v", snap.Health.Checks)
	}

	// Two builds of the same input produce byte-identical JSON.
	a, err := snap.JSON()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := verify.BuildSnapshot(fullSnapshotInputs()).JSON()
	if string(a) != string(b) {
		t.Errorf("snapshot JSON is not deterministic")
	}
}

func TestBuildSnapshot_TruncatesPublicKey(t *testing.T) {
	snap := verify.BuildSnapshot(fullSnapshotInputs())
	for _, p := range snap.Peers {
		if strings.Contains(p.PublicKey, "IJKLMNOP") {
			t.Errorf("peer %s leaked full public key: %q", p.ClusterID, p.PublicKey)
		}
	}
	out, _ := snap.JSON()
	if strings.Contains(string(out), "ABCDEFGHIJKLMNOP") {
		t.Errorf("snapshot JSON leaked full public key material")
	}
}

func TestBuildSnapshot_HandshakeAge(t *testing.T) {
	snap := verify.BuildSnapshot(fullSnapshotInputs())
	byID := map[string]verify.PeerSnapshot{}
	for _, p := range snap.Peers {
		byID[p.ClusterID] = p
	}
	if got := byID["b"].HandshakeAge; got != 30 {
		t.Errorf("peer b handshake age = %d, want 30", got)
	}
	// Error peer never handshaked → unknown age (-1).
	if got := byID["a"].HandshakeAge; got != -1 {
		t.Errorf("peer a handshake age = %d, want -1", got)
	}
}

func TestBuildSnapshot_ProbeAgeAndProbed(t *testing.T) {
	const now int64 = 1_000_000
	in := verify.SnapshotInputs{
		Now: now,
		Peers: []verify.PeerSnapshot{
			// Recently probed and succeeded → Probed, fresh ProbeAge.
			{ClusterID: "fresh", LastProbeAttempt: now - 10, LastProbe: now - 10},
			// Recently attempted but never succeeded → Probed, ProbeAge -1.
			{ClusterID: "failing", LastProbeAttempt: now - 10, LastProbe: 0},
			// Probing stopped long ago → not Probed, falls back to handshake.
			{ClusterID: "stale-probe", LastProbeAttempt: now - (verify.StaleHandshakeSeconds + 60), LastProbe: now - (verify.StaleHandshakeSeconds + 60)},
			// Never probed → not Probed.
			{ClusterID: "unprobed"},
		},
	}
	snap := verify.BuildSnapshot(in)
	byID := map[string]verify.PeerSnapshot{}
	for _, p := range snap.Peers {
		byID[p.ClusterID] = p
	}

	if p := byID["fresh"]; !p.Probed || p.ProbeAge != 10 {
		t.Errorf("fresh: Probed=%v ProbeAge=%d, want true/10", p.Probed, p.ProbeAge)
	}
	if p := byID["failing"]; !p.Probed || p.ProbeAge != -1 {
		t.Errorf("failing: Probed=%v ProbeAge=%d, want true/-1", p.Probed, p.ProbeAge)
	}
	if p := byID["stale-probe"]; p.Probed {
		t.Errorf("stale-probe: Probed=%v, want false (probe attempt aged out)", p.Probed)
	}
	if p := byID["unprobed"]; p.Probed {
		t.Errorf("unprobed: Probed=%v, want false", p.Probed)
	}
}

func TestStatusMarshalsAsName(t *testing.T) {
	snap := verify.BuildSnapshot(fullSnapshotInputs())
	out, _ := json.Marshal(snap.Health)
	if !strings.Contains(string(out), `"status":"FAIL"`) {
		t.Errorf("status should marshal as its name, got %s", out)
	}
}
