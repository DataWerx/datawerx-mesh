package topology_test

import (
	"strings"
	"testing"

	"github.com/DataWerx/datawerx-mesh/pkg/topology"
)

func reasons(cs []topology.TopologyConflict) string {
	var b strings.Builder
	for _, c := range cs {
		b.WriteString(c.String())
		b.WriteString("\n")
	}
	return b.String()
}

func hasConflict(cs []topology.TopologyConflict, cluster, substr string) bool {
	for _, c := range cs {
		if c.ClusterID == cluster && strings.Contains(c.Reason, substr) {
			return true
		}
	}
	return false
}

func TestDetectTopologyConflicts_Clean(t *testing.T) {
	peers := []topology.PeerIdentity{
		{ClusterID: "a", PublicKey: "ka", Endpoint: "1.1.1.1:51820", CIDRs: []string{"10.1.0.0/16"}},
		{ClusterID: "b", PublicKey: "kb", Endpoint: "2.2.2.2:51820", CIDRs: []string{"10.2.0.0/16"}},
	}
	if got := topology.DetectTopologyConflicts(peers); len(got) != 0 {
		t.Errorf("expected no conflicts, got:\n%s", reasons(got))
	}
}

func TestDetectTopologyConflicts_MissingFields(t *testing.T) {
	peers := []topology.PeerIdentity{
		{ClusterID: "", PublicKey: "", Endpoint: ""},
	}
	got := topology.DetectTopologyConflicts(peers)
	if !hasConflict(got, "<peer #0>", "missing cluster ID") {
		t.Errorf("missing cluster ID not flagged:\n%s", reasons(got))
	}
	if !hasConflict(got, "", "missing public key") {
		t.Errorf("missing public key not flagged:\n%s", reasons(got))
	}
	if !hasConflict(got, "", "missing endpoint") {
		t.Errorf("missing endpoint not flagged:\n%s", reasons(got))
	}
}

func TestDetectTopologyConflicts_DuplicateID(t *testing.T) {
	peers := []topology.PeerIdentity{
		{ClusterID: "dup", PublicKey: "k1", Endpoint: "1.1.1.1:1"},
		{ClusterID: "dup", PublicKey: "k2", Endpoint: "2.2.2.2:1"},
	}
	got := topology.DetectTopologyConflicts(peers)
	if !hasConflict(got, "dup", "duplicate cluster ID") {
		t.Errorf("duplicate cluster ID not flagged:\n%s", reasons(got))
	}
}

func TestDetectTopologyConflicts_DuplicateKey(t *testing.T) {
	peers := []topology.PeerIdentity{
		{ClusterID: "a", PublicKey: "shared-key-1234567890", Endpoint: "1.1.1.1:1"},
		{ClusterID: "b", PublicKey: "shared-key-1234567890", Endpoint: "2.2.2.2:1"},
	}
	got := topology.DetectTopologyConflicts(peers)
	if !hasConflict(got, "b", "also used by cluster \"a\"") {
		t.Errorf("duplicate public key not flagged:\n%s", reasons(got))
	}
	// The full key must never appear in the reason; only the truncated form.
	if strings.Contains(reasons(got), "shared-key-1234567890") {
		t.Errorf("full public key leaked into conflict reason:\n%s", reasons(got))
	}
}

func TestDetectTopologyConflicts_OverlappingCIDRs(t *testing.T) {
	peers := []topology.PeerIdentity{
		{ClusterID: "a", PublicKey: "ka", Endpoint: "1.1.1.1:1", CIDRs: []string{"10.0.0.0/8"}},
		{ClusterID: "b", PublicKey: "kb", Endpoint: "2.2.2.2:1", CIDRs: []string{"10.244.0.0/16"}},
	}
	got := topology.DetectTopologyConflicts(peers)
	if !hasConflict(got, "a", "overlaps cluster \"b\"") {
		t.Errorf("overlapping CIDRs not flagged:\n%s", reasons(got))
	}
}

func TestDetectTopologyConflicts_InvalidCIDR(t *testing.T) {
	peers := []topology.PeerIdentity{
		{ClusterID: "a", PublicKey: "ka", Endpoint: "1.1.1.1:1", CIDRs: []string{"not-a-cidr"}},
	}
	got := topology.DetectTopologyConflicts(peers)
	if !hasConflict(got, "a", "invalid CIDR") {
		t.Errorf("invalid CIDR not flagged:\n%s", reasons(got))
	}
}

func TestDetectTopologyConflicts_Deterministic(t *testing.T) {
	// Same input in different order yields the same (sorted) result.
	a := []topology.PeerIdentity{
		{ClusterID: "z", PublicKey: "kz", Endpoint: "1:1", CIDRs: []string{"10.0.0.0/8"}},
		{ClusterID: "a", PublicKey: "ka", Endpoint: "2:1", CIDRs: []string{"10.0.0.0/8"}},
	}
	b := []topology.PeerIdentity{a[1], a[0]}
	if reasons(topology.DetectTopologyConflicts(a)) != reasons(topology.DetectTopologyConflicts(b)) {
		t.Error("conflict detection is not order-independent")
	}
}
