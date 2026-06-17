package nat

import "testing"

func TestBuildGatewayMasqRules_PairsClientToDest(t *testing.T) {
	rules := BuildGatewayMasqRules(
		[]string{"100.64.0.0/10"},
		[]string{"10.96.0.0/16", "10.245.0.0/16"},
	)
	// Sorted by dest: 10.245 sorts before 10.96 ('2' < '9').
	want := [][]string{
		{"-s", "100.64.0.0/10", "-d", "10.245.0.0/16", "-j", "MASQUERADE"},
		{"-s", "100.64.0.0/10", "-d", "10.96.0.0/16", "-j", "MASQUERADE"},
	}
	if len(rules) != len(want) {
		t.Fatalf("got %d rules, want %d: %#v", len(rules), len(want), rules)
	}
	for i, r := range rules {
		if r.Chain != GatewayMasqChain {
			t.Errorf("rule %d chain = %q, want %q", i, r.Chain, GatewayMasqChain)
		}
		if !equalArgs(r.Args, want[i]) {
			t.Errorf("rule %d args = %v, want %v", i, r.Args, want[i])
		}
	}
}

func TestBuildGatewayMasqRules_MultipleClientsSortedByClientThenDest(t *testing.T) {
	rules := BuildGatewayMasqRules(
		[]string{"100.96.0.0/11", "100.64.0.0/11"},
		[]string{"10.245.0.0/16", "10.96.0.0/16"},
	)
	// Deterministic order: client asc, then dest asc.
	want := [][]string{
		{"-s", "100.64.0.0/11", "-d", "10.245.0.0/16", "-j", "MASQUERADE"},
		{"-s", "100.64.0.0/11", "-d", "10.96.0.0/16", "-j", "MASQUERADE"},
		{"-s", "100.96.0.0/11", "-d", "10.245.0.0/16", "-j", "MASQUERADE"},
		{"-s", "100.96.0.0/11", "-d", "10.96.0.0/16", "-j", "MASQUERADE"},
	}
	if len(rules) != len(want) {
		t.Fatalf("got %d rules, want %d: %#v", len(rules), len(want), rules)
	}
	for i, r := range rules {
		if !equalArgs(r.Args, want[i]) {
			t.Errorf("rule %d args = %v, want %v", i, r.Args, want[i])
		}
	}
}

func TestBuildGatewayMasqRules_SkipsCrossFamilyPairs(t *testing.T) {
	rules := BuildGatewayMasqRules(
		[]string{"100.64.0.0/10", "fd7a:115c::/48"},
		[]string{"10.96.0.0/16", "fd00:96::/64"},
	)
	// Only same-family pairs: v4→v4 and v6→v6 (2 rules), not the cross pairs.
	if len(rules) != 2 {
		t.Fatalf("got %d rules, want 2 (same-family only): %#v", len(rules), rules)
	}
	for _, r := range rules {
		src, dst := r.Args[1], r.Args[3]
		if isIPv6(src) != isIPv6(dst) {
			t.Errorf("rule crosses address families: %v", r.Args)
		}
	}
}

func TestBuildGatewayMasqRules_IPv6Pairs(t *testing.T) {
	rules := BuildGatewayMasqRules(
		[]string{"fd7a:115c::/48"},
		[]string{"fd00:96::/64"},
	)
	if len(rules) != 1 {
		t.Fatalf("got %d rules, want 1: %#v", len(rules), rules)
	}
	want := []string{"-s", "fd7a:115c::/48", "-d", "fd00:96::/64", "-j", "MASQUERADE"}
	if !equalArgs(rules[0].Args, want) {
		t.Errorf("args = %v, want %v", rules[0].Args, want)
	}
}

func TestBuildGatewayMasqRules_DedupesAndDropsEmpty(t *testing.T) {
	rules := BuildGatewayMasqRules(
		[]string{"100.64.0.0/10", "100.64.0.0/10", ""},
		[]string{"10.96.0.0/16", "10.96.0.0/16", ""},
	)
	if len(rules) != 1 {
		t.Fatalf("got %d rules, want 1 after dedupe: %#v", len(rules), rules)
	}
}

func TestBuildGatewayMasqRules_EmptyInputsNoRules(t *testing.T) {
	if rules := BuildGatewayMasqRules(nil, []string{"10.96.0.0/16"}); len(rules) != 0 {
		t.Errorf("no client CIDRs should yield no rules, got %#v", rules)
	}
	if rules := BuildGatewayMasqRules([]string{"100.64.0.0/10"}, nil); len(rules) != 0 {
		t.Errorf("no dest CIDRs should yield no rules, got %#v", rules)
	}
	if rules := BuildGatewayMasqRules(nil, nil); len(rules) != 0 {
		t.Errorf("empty inputs should yield no rules, got %#v", rules)
	}
}

// The rule set must be independent of input ordering so the full-state apply is
// idempotent (the same desired topology always produces the same rules).
func TestBuildGatewayMasqRules_OrderIndependent(t *testing.T) {
	a := BuildGatewayMasqRules(
		[]string{"100.64.0.0/11", "100.96.0.0/11"},
		[]string{"10.96.0.0/16", "10.245.0.0/16"},
	)
	b := BuildGatewayMasqRules(
		[]string{"100.96.0.0/11", "100.64.0.0/11"},
		[]string{"10.245.0.0/16", "10.96.0.0/16"},
	)
	if len(a) != len(b) {
		t.Fatalf("len(a)=%d len(b)=%d", len(a), len(b))
	}
	for i := range a {
		if !equalArgs(a[i].Args, b[i].Args) {
			t.Errorf("rule %d differs by input order: %v vs %v", i, a[i].Args, b[i].Args)
		}
	}
}
