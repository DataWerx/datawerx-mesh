package nat_test

import (
	"reflect"
	"strconv"
	"testing"

	"github.com/DataWerx/datawerx-mesh/pkg/nat"
)

// argValuesAfter returns, in rule order, the argument immediately following each
// occurrence of flag across all rules' Args.
func argValuesAfter(rules []nat.Rule, flag string) []string {
	var out []string
	for _, r := range rules {
		for i, a := range r.Args {
			if a == flag && i+1 < len(r.Args) {
				out = append(out, r.Args[i+1])
			}
		}
	}
	return out
}

// expected chain names are derived via the same hashing the planner uses, so
// the tests assert structure/wiring without hard-coding opaque hashes.
func TestBuildRuleset_SingleServiceSingleBackend(t *testing.T) {
	rs := nat.BuildRuleset([]nat.ServiceDNAT{{
		Namespace: "prod", Name: "payments", VIP: "241.0.0.5",
		Ports:    []nat.PortDNAT{{Protocol: "TCP", Port: 80}},
		Backends: []string{"10.96.0.10"},
	}})

	if len(rs.Chains) != 2 {
		t.Fatalf("expected 2 chains (svc+sep), got %d: %v", len(rs.Chains), rs.Chains)
	}
	// Root jump + 1 LB rule (single backend -> unconditional jump) + 1 DNAT.
	if len(rs.Rules) != 3 {
		t.Fatalf("expected 3 rules, got %d: %#v", len(rs.Rules), rs.Rules)
	}

	root := rs.Rules[0]
	if root.Chain != nat.RootChain {
		t.Errorf("first rule chain = %s, want %s", root.Chain, nat.RootChain)
	}
	wantRootArgs := []string{"-d", "241.0.0.5/32", "-p", "tcp", "-m", "tcp", "--dport", "80", "-j", root.Args[len(root.Args)-1]}
	if !reflect.DeepEqual(root.Args, wantRootArgs) {
		t.Errorf("root args = %v, want %v", root.Args, wantRootArgs)
	}

	// Single backend: the LB rule is an unconditional jump (no statistic match).
	lb := rs.Rules[1]
	if len(lb.Args) != 2 || lb.Args[0] != "-j" {
		t.Errorf("single-backend LB rule should be unconditional jump, got %v", lb.Args)
	}

	dnat := rs.Rules[2]
	wantDNAT := []string{"-p", "tcp", "-j", "DNAT", "--to-destination", "10.96.0.10:80"}
	if !reflect.DeepEqual(dnat.Args, wantDNAT) {
		t.Errorf("DNAT args = %v, want %v", dnat.Args, wantDNAT)
	}
}

func TestBuildRuleset_LoadBalanceProbabilities(t *testing.T) {
	rs := nat.BuildRuleset([]nat.ServiceDNAT{{
		Namespace: "prod", Name: "api", VIP: "241.0.0.9",
		Ports:    []nat.PortDNAT{{Protocol: "tcp", Port: 8080}},
		Backends: []string{"10.0.0.3", "10.0.0.1", "10.0.0.2"}, // unsorted on purpose
	}})

	// Collect the statistic probabilities in order from the service chain.
	probs := argValuesAfter(rs.Rules, "--probability")
	lastUnconditional := false
	for _, r := range rs.Rules {
		if len(r.Args) == 2 && r.Args[0] == "-j" {
			lastUnconditional = true
		}
	}

	// 3 backends -> probabilities 1/3, 1/2, then unconditional.
	want := []string{
		strconv.FormatFloat(1.0/3.0, 'f', 10, 64),
		strconv.FormatFloat(1.0/2.0, 'f', 10, 64),
	}
	if !reflect.DeepEqual(probs, want) {
		t.Errorf("probabilities = %v, want %v (uniform split)", probs, want)
	}
	if !lastUnconditional {
		t.Error("expected a final unconditional jump for the last backend")
	}

	// DNAT targets must be the sorted, de-duplicated backends.
	targets := argValuesAfter(rs.Rules, "--to-destination")
	wantTargets := []string{"10.0.0.1:8080", "10.0.0.2:8080", "10.0.0.3:8080"}
	if !reflect.DeepEqual(targets, wantTargets) {
		t.Errorf("DNAT targets = %v, want %v", targets, wantTargets)
	}
}

func TestBuildRuleset_Determinism(t *testing.T) {
	in := []nat.ServiceDNAT{
		{Namespace: "b", Name: "y", VIP: "241.0.0.2", Ports: []nat.PortDNAT{{Port: 80}}, Backends: []string{"10.0.0.2", "10.0.0.1"}},
		{Namespace: "a", Name: "x", VIP: "241.0.0.1", Ports: []nat.PortDNAT{{Port: 443}}, Backends: []string{"10.0.1.1"}},
	}
	first := nat.BuildRuleset(in)
	// Reverse input order; output must be identical.
	reversed := []nat.ServiceDNAT{in[1], in[0]}
	second := nat.BuildRuleset(reversed)
	if !reflect.DeepEqual(first, second) {
		t.Errorf("BuildRuleset is not order-independent:\n%#v\n%#v", first, second)
	}
}

func TestBuildRuleset_Skips(t *testing.T) {
	tests := []struct {
		name string
		in   nat.ServiceDNAT
	}{
		{"no VIP", nat.ServiceDNAT{Namespace: "p", Name: "s", Ports: []nat.PortDNAT{{Port: 80}}, Backends: []string{"10.0.0.1"}}},
		{"no ports", nat.ServiceDNAT{Namespace: "p", Name: "s", VIP: "241.0.0.5", Backends: []string{"10.0.0.1"}}},
		{"no backends", nat.ServiceDNAT{Namespace: "p", Name: "s", VIP: "241.0.0.5", Ports: []nat.PortDNAT{{Port: 80}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rs := nat.BuildRuleset([]nat.ServiceDNAT{tt.in})
			if len(rs.Rules) != 0 || len(rs.Chains) != 0 {
				t.Errorf("expected empty ruleset for %q, got %#v", tt.name, rs)
			}
		})
	}
}

func TestBuildRuleset_DefaultProtocolAndPortDedup(t *testing.T) {
	rs := nat.BuildRuleset([]nat.ServiceDNAT{{
		Namespace: "prod", Name: "svc", VIP: "241.0.0.7",
		Ports:    []nat.PortDNAT{{Port: 80}, {Port: 80, Protocol: "TCP"}}, // dup after normalize
		Backends: []string{"10.0.0.1"},
	}})
	// Only one port (80/tcp) survives dedup -> root+lb+dnat = 3 rules.
	if len(rs.Rules) != 3 {
		t.Fatalf("expected 3 rules after port dedup, got %d: %#v", len(rs.Rules), rs.Rules)
	}
	if rs.Rules[0].Args[3] != "tcp" {
		t.Errorf("empty protocol should default to tcp, got %v", rs.Rules[0].Args)
	}
}

func TestBuildRuleset_IPv6Formatting(t *testing.T) {
	rs := nat.BuildRuleset([]nat.ServiceDNAT{{
		Namespace: "prod", Name: "api6", VIP: "fd00::1",
		Ports:    []nat.PortDNAT{{Protocol: "tcp", Port: 443}},
		Backends: []string{"2001:db8::10"},
	}})

	var rootArgs, dnatTarget string
	for _, r := range rs.Rules {
		for i, a := range r.Args {
			if a == "-d" {
				rootArgs = r.Args[i+1]
			}
			if a == "--to-destination" {
				dnatTarget = r.Args[i+1]
			}
		}
	}
	if rootArgs != "fd00::1/128" {
		t.Errorf("IPv6 VIP match = %q, want fd00::1/128", rootArgs)
	}
	if dnatTarget != "[2001:db8::10]:443" {
		t.Errorf("IPv6 DNAT target = %q, want [2001:db8::10]:443", dnatTarget)
	}
}
