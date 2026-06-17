package verify_test

import (
	"strings"
	"testing"

	"github.com/datawerx/datawerx/pkg/verify"
)

func findFinding(fs []verify.Finding, titleContains string) (verify.Finding, bool) {
	for _, f := range fs {
		if strings.Contains(f.Title, titleContains) {
			return f, true
		}
	}
	return verify.Finding{}, false
}

func TestDiagnose_CIDROverlapIsCriticalAndCited(t *testing.T) {
	snap := verify.BuildSnapshot(fullSnapshotInputs())
	fs := verify.Diagnose(snap)

	f, ok := findFinding(fs, "Topology conflict on cluster a")
	if !ok {
		t.Fatalf("expected a topology conflict finding, got %+v", fs)
	}
	if f.Severity != verify.SeverityCritical {
		t.Errorf("overlap should be critical, got %v", f.Severity)
	}
	if !strings.Contains(f.Signal, "overlaps cluster") {
		t.Errorf("finding must cite the overlap signal, got %q", f.Signal)
	}
	if !strings.Contains(f.Remedy, "DataWerx_REMAP_CIDR") {
		t.Errorf("overlap remedy should mention the remap option, got %q", f.Remedy)
	}

	// The Error peer itself is also explained.
	if _, ok := findFinding(fs, "Peer a in Error"); !ok {
		t.Errorf("expected the Error peer to be diagnosed, got %+v", fs)
	}
}

func TestDiagnose_StaleHandshake(t *testing.T) {
	in := fullSnapshotInputs()
	// Make peer b a Connected peer whose last handshake is well past the threshold.
	in.Peers = []verify.PeerSnapshot{
		{Name: "cluster-b", ClusterID: "b", Phase: "Connected", LastHandshake: in.Now - (verify.StaleHandshakeSeconds + 60)},
	}
	in.Conflicts = nil
	fs := verify.Diagnose(verify.BuildSnapshot(in))

	f, ok := findFinding(fs, "Tunnel to b may be down")
	if !ok {
		t.Fatalf("expected a stale-tunnel finding, got %+v", fs)
	}
	if f.Severity != verify.SeverityWarning {
		t.Errorf("stale tunnel should warn, got %v", f.Severity)
	}
	if !strings.Contains(f.Signal, "last handshake") {
		t.Errorf("finding must cite the handshake age, got %q", f.Signal)
	}
}

func TestDiagnose_NeverHandshakedConnectedPeer(t *testing.T) {
	in := fullSnapshotInputs()
	in.Peers = []verify.PeerSnapshot{{ClusterID: "b", Phase: "Connected", LastHandshake: 0}}
	in.Conflicts = nil
	fs := verify.Diagnose(verify.BuildSnapshot(in))
	f, ok := findFinding(fs, "Tunnel to b may be down")
	if !ok {
		t.Fatalf("expected a stale-tunnel finding for never-handshaked peer, got %+v", fs)
	}
	if !strings.Contains(f.Signal, "no handshake recorded") {
		t.Errorf("want no-handshake signal, got %q", f.Signal)
	}
}

func TestDiagnose_HealthyMeshIsQuiet(t *testing.T) {
	in := fullSnapshotInputs()
	in.Peers = []verify.PeerSnapshot{
		{ClusterID: "b", Phase: "Connected", LastHandshake: in.Now - 30},
	}
	in.Conflicts = nil
	fs := verify.Diagnose(verify.BuildSnapshot(in))
	for _, f := range fs {
		if f.Severity >= verify.SeverityWarning {
			t.Errorf("healthy mesh should produce no warnings/criticals, got %+v", f)
		}
	}
}

func TestDiagnose_CriticalsSortFirst(t *testing.T) {
	in := fullSnapshotInputs()
	in.Events = []verify.EventSnapshot{{Type: "Warning", Reason: "SyncFailed", Message: "boom", Object: "MeshPeer/a"}}
	fs := verify.Diagnose(verify.BuildSnapshot(in))
	if len(fs) == 0 {
		t.Fatal("expected findings")
	}
	if fs[0].Severity != verify.SeverityCritical {
		t.Errorf("most severe finding should sort first, got %v", fs[0].Severity)
	}
}

func TestDiagnose_InvalidExport(t *testing.T) {
	in := fullSnapshotInputs()
	in.Exports = []verify.ExportSnapshot{{Namespace: "prod", Name: "payments", Valid: false, Message: "no matching Service"}}
	fs := verify.Diagnose(verify.BuildSnapshot(in))
	f, ok := findFinding(fs, "Export invalid: prod/payments")
	if !ok {
		t.Fatalf("expected invalid-export finding, got %+v", fs)
	}
	if !strings.Contains(f.Signal, "valid=false") {
		t.Errorf("want valid=false signal, got %q", f.Signal)
	}
}
