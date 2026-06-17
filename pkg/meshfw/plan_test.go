package meshfw_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/datawerx/datawerx/pkg/meshfw"
)

// joinRules renders a ruleset's FWChain into comparable "chain: args" lines.
func joinRules(rs meshfw.Ruleset) []string {
	out := make([]string, 0, len(rs.Rules))
	for _, r := range rs.Rules {
		out = append(out, r.Chain+": "+strings.Join(r.Args, " "))
	}
	return out
}

func contains(rs meshfw.Ruleset, substr string) bool {
	for _, line := range joinRules(rs) {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}

func TestBuildFirewall_Empty(t *testing.T) {
	rs := meshfw.BuildFirewall(nil, nil)
	// Only the conntrack fast-path; no drops → all traffic falls through.
	if len(rs.Rules) != 1 {
		t.Fatalf("expected only the conntrack rule, got %v", joinRules(rs))
	}
	if !contains(rs, "RELATED,ESTABLISHED -j ACCEPT") {
		t.Errorf("missing conntrack accept: %v", joinRules(rs))
	}
}

func TestBuildFirewall_AllowFromClusterToDestPort(t *testing.T) {
	policies := []meshfw.Policy{{
		Name:         "db",
		Destinations: []string{"10.96.5.0/24"},
		Ingress: []meshfw.IngressRule{{
			From:  []meshfw.PeerSelector{{ClusterIDs: []string{"cluster-b"}}},
			Ports: []meshfw.Port{{Protocol: "TCP", Port: 5432}},
		}},
	}}
	cidrs := map[string][]string{"cluster-b": {"10.40.0.0/16"}}

	rs := meshfw.BuildFirewall(policies, cidrs)

	if !contains(rs, "-s 10.40.0.0/16 -d 10.96.5.0/24 -p tcp -m tcp --dport 5432 -j ACCEPT") {
		t.Errorf("expected resolved allow rule, got:\n%s", strings.Join(joinRules(rs), "\n"))
	}
	// Protected dest must end with a DROP (default-deny once selected).
	if !contains(rs, "-d 10.96.5.0/24 -j DROP") {
		t.Errorf("expected default-deny DROP for protected dest, got:\n%s", strings.Join(joinRules(rs), "\n"))
	}
	// The DROP must come after the ACCEPT.
	lines := joinRules(rs)
	var acceptIdx, dropIdx = -1, -1
	for i, l := range lines {
		if strings.Contains(l, "--dport 5432 -j ACCEPT") {
			acceptIdx = i
		}
		if strings.Contains(l, "-d 10.96.5.0/24 -j DROP") {
			dropIdx = i
		}
	}
	if acceptIdx == -1 || dropIdx == -1 || acceptIdx > dropIdx {
		t.Errorf("ACCEPT (%d) must precede DROP (%d):\n%s", acceptIdx, dropIdx, strings.Join(lines, "\n"))
	}
}

func TestBuildFirewall_ProtectAllDefaultDeny(t *testing.T) {
	// Empty Destinations → protect everything; only the explicit allow gets through.
	policies := []meshfw.Policy{{
		Name: "lockdown",
		Ingress: []meshfw.IngressRule{{
			From: []meshfw.PeerSelector{{CIDRs: []string{"10.50.0.0/16"}}},
		}},
	}}
	rs := meshfw.BuildFirewall(policies, nil)

	if !contains(rs, "-s 10.50.0.0/16 -j ACCEPT") {
		t.Errorf("expected allow from explicit CIDR, got:\n%s", strings.Join(joinRules(rs), "\n"))
	}
	// Final catch-all DROP with no -d.
	last := rs.Rules[len(rs.Rules)-1]
	if !reflect.DeepEqual(last.Args, []string{"-j", "DROP"}) {
		t.Errorf("expected final catch-all DROP, got %v", last.Args)
	}
}

func TestBuildFirewall_MultipleClustersUnion(t *testing.T) {
	policies := []meshfw.Policy{{
		Name:         "api",
		Destinations: []string{"10.96.0.0/16"},
		Ingress: []meshfw.IngressRule{{
			From: []meshfw.PeerSelector{{ClusterIDs: []string{"a", "b"}}},
		}},
	}}
	cidrs := map[string][]string{"a": {"10.1.0.0/16"}, "b": {"10.2.0.0/16"}}
	rs := meshfw.BuildFirewall(policies, cidrs)
	if !contains(rs, "-s 10.1.0.0/16 -d 10.96.0.0/16 -j ACCEPT") {
		t.Errorf("missing cluster a allow:\n%s", strings.Join(joinRules(rs), "\n"))
	}
	if !contains(rs, "-s 10.2.0.0/16 -d 10.96.0.0/16 -j ACCEPT") {
		t.Errorf("missing cluster b allow:\n%s", strings.Join(joinRules(rs), "\n"))
	}
}

func TestBuildFirewall_UnknownClusterContributesNothing(t *testing.T) {
	policies := []meshfw.Policy{{
		Name:         "x",
		Destinations: []string{"10.96.0.0/16"},
		Ingress:      []meshfw.IngressRule{{From: []meshfw.PeerSelector{{ClusterIDs: []string{"ghost"}}}}},
	}}
	rs := meshfw.BuildFirewall(policies, map[string][]string{}) // ghost not known
	// No ACCEPT for a source, but the dest is still protected (default-deny).
	if contains(rs, "-j ACCEPT") && contains(rs, "-s ") {
		t.Errorf("unknown cluster must not yield a source ACCEPT:\n%s", strings.Join(joinRules(rs), "\n"))
	}
	if !contains(rs, "-d 10.96.0.0/16 -j DROP") {
		t.Errorf("dest must still be protected:\n%s", strings.Join(joinRules(rs), "\n"))
	}
}

func TestBuildFirewall_SkipsNonIPv4(t *testing.T) {
	policies := []meshfw.Policy{{
		Name:         "v6",
		Destinations: []string{"fd00::/64", "10.96.0.0/16"},
		Ingress:      []meshfw.IngressRule{{From: []meshfw.PeerSelector{{CIDRs: []string{"fd00:40::/64"}}}}},
	}}
	rs := meshfw.BuildFirewall(policies, nil)

	if len(rs.Skipped) == 0 {
		t.Fatal("expected skipped non-IPv4 inputs to be reported")
	}
	// The IPv4 dest is still protected; the v6 dest/source are absent.
	if !contains(rs, "-d 10.96.0.0/16 -j DROP") {
		t.Errorf("IPv4 dest should still be programmed:\n%s", strings.Join(joinRules(rs), "\n"))
	}
	if contains(rs, "fd00") {
		t.Errorf("no IPv6 literal should appear in v4 ruleset:\n%s", strings.Join(joinRules(rs), "\n"))
	}
}

func TestBuildFirewall_Deterministic(t *testing.T) {
	mk := func() []meshfw.Policy {
		return []meshfw.Policy{
			{Name: "p2", Destinations: []string{"10.96.2.0/24"}, Ingress: []meshfw.IngressRule{{From: []meshfw.PeerSelector{{CIDRs: []string{"10.2.0.0/16"}}}}}},
			{Name: "p1", Destinations: []string{"10.96.1.0/24"}, Ingress: []meshfw.IngressRule{{From: []meshfw.PeerSelector{{CIDRs: []string{"10.1.0.0/16"}}}}}},
		}
	}
	a := meshfw.BuildFirewall(mk(), nil)
	// Reverse policy order; result must be identical.
	pols := mk()
	pols[0], pols[1] = pols[1], pols[0]
	b := meshfw.BuildFirewall(pols, nil)
	if !reflect.DeepEqual(joinRules(a), joinRules(b)) {
		t.Errorf("planner not order-independent:\nA:\n%s\nB:\n%s", strings.Join(joinRules(a), "\n"), strings.Join(joinRules(b), "\n"))
	}
}
