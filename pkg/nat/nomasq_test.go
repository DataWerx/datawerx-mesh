package nat

import "testing"

func TestBuildNoMasqRules_PairsLocalToRemote(t *testing.T) {
	rules := BuildNoMasqRules(
		[]string{"10.244.0.0/16"},
		[]string{"10.97.0.0/16", "10.245.0.0/16"},
	)
	// Sorted by remote: 10.245 sorts before 10.97 ('2' < '9').
	want := [][]string{
		{"-s", "10.244.0.0/16", "-d", "10.245.0.0/16", "-j", "ACCEPT"},
		{"-s", "10.244.0.0/16", "-d", "10.97.0.0/16", "-j", "ACCEPT"},
	}
	if len(rules) != len(want) {
		t.Fatalf("got %d rules, want %d: %#v", len(rules), len(want), rules)
	}
	for i, r := range rules {
		if r.Chain != NoMasqChain {
			t.Errorf("rule %d chain = %q, want %q", i, r.Chain, NoMasqChain)
		}
		if !equalArgs(r.Args, want[i]) {
			t.Errorf("rule %d args = %v, want %v", i, r.Args, want[i])
		}
	}
}

func TestBuildNoMasqRules_SkipsCrossFamilyPairs(t *testing.T) {
	rules := BuildNoMasqRules(
		[]string{"10.244.0.0/16", "fd00:244::/64"},
		[]string{"10.245.0.0/16", "fd00:245::/64"},
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

func TestBuildNoMasqRules_IPv6Pairs(t *testing.T) {
	rules := BuildNoMasqRules(
		[]string{"fd00:aaaa::/64"},
		[]string{"fd00:beef::/64"},
	)
	if len(rules) != 1 {
		t.Fatalf("got %d rules, want 1: %#v", len(rules), rules)
	}
	want := []string{"-s", "fd00:aaaa::/64", "-d", "fd00:beef::/64", "-j", "ACCEPT"}
	if !equalArgs(rules[0].Args, want) {
		t.Errorf("args = %v, want %v", rules[0].Args, want)
	}
}

func TestBuildNoMasqRules_DedupesAndDropsEmpty(t *testing.T) {
	rules := BuildNoMasqRules(
		[]string{"10.244.0.0/16", "10.244.0.0/16", ""},
		[]string{"10.245.0.0/16", "10.245.0.0/16"},
	)
	if len(rules) != 1 {
		t.Fatalf("got %d rules, want 1 after dedupe: %#v", len(rules), rules)
	}
}

func TestBuildNoMasqRules_EmptyInputsNoRules(t *testing.T) {
	if rules := BuildNoMasqRules(nil, []string{"10.245.0.0/16"}); len(rules) != 0 {
		t.Errorf("no local CIDRs should yield no rules, got %#v", rules)
	}
	if rules := BuildNoMasqRules([]string{"10.244.0.0/16"}, nil); len(rules) != 0 {
		t.Errorf("no remote CIDRs should yield no rules, got %#v", rules)
	}
}

func equalArgs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
