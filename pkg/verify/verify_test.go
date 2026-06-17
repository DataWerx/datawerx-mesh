package verify_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/DataWerx/datawerx-mesh/pkg/verify"
)

func allCRDsPresent() map[string]bool {
	m := map[string]bool{}
	for _, c := range verify.RequiredCRDs() {
		m[c] = true
	}
	return m
}

func findCheck(r verify.Report, name string) (verify.Check, bool) {
	for _, c := range r.Checks {
		if c.Name == name {
			return c, true
		}
	}
	return verify.Check{}, false
}

func TestBuild_HealthyMesh(t *testing.T) {
	in := verify.Inputs{
		RequiredCRDs: verify.RequiredCRDs(),
		PresentCRDs:  allCRDsPresent(),
		AgentFound:   true, AgentDesired: 3, AgentReady: 3,
		Peers:               []verify.PeerInfo{{ClusterID: "b", Phase: "Connected"}},
		Exports:             []verify.ExportInfo{{Namespace: "prod", Name: "payments", Valid: true}},
		ImportsClusterSetIP: 1,
	}
	r := verify.Build(in)
	if r.Failed() {
		t.Fatalf("healthy mesh should not fail: %+v", r.Checks)
	}
	if c, _ := findCheck(r, "Mesh peers"); c.Status != verify.StatusPass {
		t.Errorf("peers status = %v, want PASS", c.Status)
	}
}

func TestBuild_HandshakeLiveness(t *testing.T) {
	const now int64 = 1_000_000
	tests := []struct {
		name  string
		peers []verify.PeerInfo
		now   int64
		want  verify.Status
		found bool
	}{
		{
			name:  "recent handshakes pass",
			peers: []verify.PeerInfo{{ClusterID: "b", Phase: "Connected", LastHandshake: now - 30}},
			now:   now, want: verify.StatusPass, found: true,
		},
		{
			name:  "stale handshake warns",
			peers: []verify.PeerInfo{{ClusterID: "b", Phase: "Connected", LastHandshake: now - verify.StaleHandshakeSeconds - 10}},
			now:   now, want: verify.StatusWarn, found: true,
		},
		{
			name:  "never-handshaked connected peer warns",
			peers: []verify.PeerInfo{{ClusterID: "b", Phase: "Connected", LastHandshake: 0}},
			now:   now, want: verify.StatusWarn, found: true,
		},
		{
			// Non-Connected peers are covered by the phase check; not assessed here.
			name:  "only pending peers: no handshake check",
			peers: []verify.PeerInfo{{ClusterID: "b", Phase: "Pending"}},
			now:   now, found: false,
		},
		{
			name:  "no clock: check skipped",
			peers: []verify.PeerInfo{{ClusterID: "b", Phase: "Connected", LastHandshake: 0}},
			now:   0, found: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := verify.Build(verify.Inputs{
				RequiredCRDs: verify.RequiredCRDs(), PresentCRDs: allCRDsPresent(),
				AgentFound: true, AgentDesired: 1, AgentReady: 1,
				Peers: tt.peers, Now: tt.now,
			})
			c, ok := findCheck(r, "Tunnel handshakes")
			if ok != tt.found {
				t.Fatalf("handshake check present = %v, want %v (checks=%+v)", ok, tt.found, r.Checks)
			}
			if tt.found && c.Status != tt.want {
				t.Errorf("status = %v, want %v (detail=%q)", c.Status, tt.want, c.Detail)
			}
		})
	}
}

func TestBuild_MissingCRDsFails(t *testing.T) {
	present := allCRDsPresent()
	delete(present, "serviceimports.multicluster.x-k8s.io")
	r := verify.Build(verify.Inputs{
		RequiredCRDs: verify.RequiredCRDs(), PresentCRDs: present,
		AgentFound: true, AgentDesired: 1, AgentReady: 1,
	})
	if !r.Failed() {
		t.Fatal("missing CRD should fail")
	}
	c, _ := findCheck(r, "CRDs installed")
	if c.Status != verify.StatusFail || !strings.Contains(c.Detail, "serviceimports") {
		t.Errorf("unexpected CRD check: %+v", c)
	}
}

func TestBuild_AgentStates(t *testing.T) {
	base := verify.Inputs{RequiredCRDs: verify.RequiredCRDs(), PresentCRDs: allCRDsPresent()}

	tests := []struct {
		name       string
		found      bool
		desired    int
		ready      int
		wantStatus verify.Status
	}{
		{"not found", false, 0, 0, verify.StatusFail},
		{"zero desired", true, 0, 0, verify.StatusWarn},
		{"all ready", true, 3, 3, verify.StatusPass},
		{"partial", true, 3, 1, verify.StatusFail},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := base
			in.AgentFound, in.AgentDesired, in.AgentReady = tt.found, tt.desired, tt.ready
			c, _ := findCheck(verify.Build(in), "Agent DaemonSet")
			if c.Status != tt.wantStatus {
				t.Errorf("agent status = %v, want %v (%s)", c.Status, tt.wantStatus, c.Detail)
			}
		})
	}
}

func TestBuild_PeerPhases(t *testing.T) {
	base := verify.Inputs{RequiredCRDs: verify.RequiredCRDs(), PresentCRDs: allCRDsPresent(), AgentFound: true, AgentDesired: 1, AgentReady: 1}

	tests := []struct {
		name  string
		peers []verify.PeerInfo
		want  verify.Status
	}{
		{"none", nil, verify.StatusWarn},
		{"all connected", []verify.PeerInfo{{Phase: "Connected"}, {Phase: "Connected"}}, verify.StatusPass},
		{"one pending", []verify.PeerInfo{{Phase: "Connected"}, {Phase: "Pending"}}, verify.StatusWarn},
		{"one error", []verify.PeerInfo{{Phase: "Connected"}, {Phase: "Error"}}, verify.StatusFail},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := base
			in.Peers = tt.peers
			c, _ := findCheck(verify.Build(in), "Mesh peers")
			if c.Status != tt.want {
				t.Errorf("peers status = %v, want %v", c.Status, tt.want)
			}
		})
	}
}

func TestBuild_InvalidExportsWarn(t *testing.T) {
	r := verify.Build(verify.Inputs{
		RequiredCRDs: verify.RequiredCRDs(), PresentCRDs: allCRDsPresent(),
		AgentFound: true, AgentDesired: 1, AgentReady: 1,
		Exports: []verify.ExportInfo{
			{Namespace: "prod", Name: "ok", Valid: true},
			{Namespace: "prod", Name: "ghost", Valid: false},
		},
	})
	if r.Failed() {
		t.Error("invalid exports should warn, not fail")
	}
	c, _ := findCheck(r, "Service exports")
	if c.Status != verify.StatusWarn || !strings.Contains(c.Detail, "prod/ghost") {
		t.Errorf("unexpected export check: %+v", c)
	}
}

func TestReport_Write(t *testing.T) {
	r := verify.Build(verify.Inputs{
		RequiredCRDs: verify.RequiredCRDs(), PresentCRDs: allCRDsPresent(),
		AgentFound: true, AgentDesired: 1, AgentReady: 1,
		Peers: []verify.PeerInfo{{Phase: "Connected"}},
	})
	var buf bytes.Buffer
	r.Write(&buf)
	out := buf.String()
	if !strings.Contains(out, "passed") || !strings.Contains(out, "Agent DaemonSet") {
		t.Errorf("unexpected report output:\n%s", out)
	}
}
