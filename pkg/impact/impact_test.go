package impact_test

import (
	"strings"
	"testing"

	networkingv1alpha1 "github.com/datawerx/datawerx/pkg/apis/networking/v1alpha1"
	"github.com/datawerx/datawerx/pkg/impact"
	"github.com/datawerx/datawerx/pkg/meshfw"
	"github.com/datawerx/datawerx/pkg/topology"
)

func clusterCIDRs() map[string][]string {
	return map[string][]string{"b": {"10.50.0.0/16"}}
}

func TestAnalyzePolicyChange_NewExposureAndProtection(t *testing.T) {
	proposed := []meshfw.Policy{{
		Name:         "lock-db",
		Destinations: []string{"10.0.0.0/24"},
		Ingress: []meshfw.IngressRule{{
			From:  []meshfw.PeerSelector{{ClusterIDs: []string{"b"}}},
			Ports: []meshfw.Port{{Protocol: "tcp", Port: 5432}},
		}},
	}}

	imp := impact.AnalyzePolicyChange(nil, proposed, clusterCIDRs())

	if len(imp.NewlyProtected) != 1 || imp.NewlyProtected[0] != "10.0.0.0/24" {
		t.Errorf("NewlyProtected = %v, want [10.0.0.0/24]", imp.NewlyProtected)
	}
	if len(imp.NewlyExposed) == 0 {
		t.Fatalf("expected a newly exposed reachability, got none")
	}
	got := imp.NewlyExposed[0]
	if got.Source != "10.50.0.0/16" || got.Dest != "10.0.0.0/24" || got.Port != "5432" {
		t.Errorf("unexpected exposure: %s (%+v)", got, got)
	}
}

func TestAnalyzePolicyChange_OverBroadWarnings(t *testing.T) {
	proposed := []meshfw.Policy{
		{
			Name: "allow-all-into-db",
			// No destinations → protects ALL ingress (default-deny everything).
			Ingress: []meshfw.IngressRule{{
				From: []meshfw.PeerSelector{{CIDRs: []string{"0.0.0.0/0"}}},
			}},
		},
		{
			Name:         "typo-cluster",
			Destinations: []string{"10.0.0.0/24"},
			Ingress: []meshfw.IngressRule{{
				From: []meshfw.PeerSelector{{ClusterIDs: []string{"does-not-exist"}}},
			}},
		},
	}

	imp := impact.AnalyzePolicyChange(nil, proposed, clusterCIDRs())
	joined := strings.Join(imp.Warnings, "\n")
	if !strings.Contains(joined, "default-deny") {
		t.Errorf("expected a default-deny-everything warning, got: %v", imp.Warnings)
	}
	if !strings.Contains(joined, "any source") {
		t.Errorf("expected an any-source warning, got: %v", imp.Warnings)
	}
	if !strings.Contains(joined, "not known to the topology") {
		t.Errorf("expected an unknown-cluster warning, got: %v", imp.Warnings)
	}
}

func TestAnalyzePolicyChange_NewlyDenied(t *testing.T) {
	current := []meshfw.Policy{{
		Name:         "db",
		Destinations: []string{"10.0.0.0/24"},
		Ingress: []meshfw.IngressRule{{
			From: []meshfw.PeerSelector{{ClusterIDs: []string{"b"}}},
		}},
	}}
	// Proposed tightens it: same protection, but no allow rules at all.
	proposed := []meshfw.Policy{{
		Name:         "db",
		Destinations: []string{"10.0.0.0/24"},
	}}

	imp := impact.AnalyzePolicyChange(current, proposed, clusterCIDRs())
	if len(imp.NewlyDenied) == 0 {
		t.Fatalf("expected the removed allow to show as newly denied, got none")
	}
	if imp.NewlyDenied[0].Source != "10.50.0.0/16" {
		t.Errorf("newly denied source = %q, want 10.50.0.0/16", imp.NewlyDenied[0].Source)
	}
}

func TestAnalyzePeerChange_OverlapWithLocalIsWithheld(t *testing.T) {
	proposed := networkingv1alpha1.MeshPeerSpec{
		ClusterID: "remote", PublicKey: "key", Endpoint: "1.2.3.4:51820",
		PodCIDRs: []string{"10.244.0.0/16"},
	}
	imp := impact.AnalyzePeerChange(proposed, []string{"10.244.0.0/16"}, nil)

	if imp.Phase != "Error" {
		t.Errorf("phase = %q, want Error", imp.Phase)
	}
	if len(imp.Withheld) != 1 || imp.Withheld[0] != "10.244.0.0/16" {
		t.Errorf("Withheld = %v, want [10.244.0.0/16]", imp.Withheld)
	}
	if imp.Safe() {
		t.Errorf("a peer overlapping a local range should not be Safe")
	}
}

func TestAnalyzePeerChange_ConflictWithExistingPeer(t *testing.T) {
	existing := []topology.PeerIdentity{
		{ClusterID: "b", PublicKey: "kb", Endpoint: "2.2.2.2:51820", CIDRs: []string{"10.60.0.0/16"}},
	}
	proposed := networkingv1alpha1.MeshPeerSpec{
		ClusterID: "c", PublicKey: "kc", Endpoint: "3.3.3.3:51820",
		PodCIDRs: []string{"10.60.0.0/16"}, // overlaps existing peer b
	}
	imp := impact.AnalyzePeerChange(proposed, nil, existing)
	if len(imp.NewConflicts) == 0 {
		t.Fatalf("expected a new topology conflict against peer b, got none")
	}
	if !strings.Contains(strings.Join(imp.NewConflicts, " "), "overlaps") {
		t.Errorf("expected an overlap conflict, got %v", imp.NewConflicts)
	}
}

func TestAnalyzePeerChange_CleanPeerIsSafe(t *testing.T) {
	proposed := networkingv1alpha1.MeshPeerSpec{
		ClusterID: "remote", PublicKey: "key", Endpoint: "1.2.3.4:51820",
		PodCIDRs: []string{"10.70.0.0/16"},
	}
	imp := impact.AnalyzePeerChange(proposed, []string{"10.244.0.0/16"}, nil)
	if !imp.Safe() {
		t.Errorf("a non-overlapping peer should be Safe, got %+v", imp)
	}
	if imp.Phase != "Connected" {
		t.Errorf("phase = %q, want Connected", imp.Phase)
	}
}
