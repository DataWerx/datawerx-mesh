package nat_test

import (
	"reflect"
	"testing"

	"github.com/datawerx/datawerx/pkg/nat"
)

func TestBuildRemapRules(t *testing.T) {
	rules := nat.BuildRemapRules([]nat.RemapEntry{
		{Real: "10.244.0.0/16", Virtual: "172.20.0.0/16"},
		{Real: "10.244.0.0/16", Virtual: "172.20.0.0/16"}, // dup, collapsed
		{Real: "10.96.0.0/16", Virtual: "172.21.0.0/16"},
	})

	// Two unique entries → 2 PRE (dst) + 2 POST (src) rules, PRE first.
	if len(rules) != 4 {
		t.Fatalf("expected 4 rules, got %d: %#v", len(rules), rules)
	}

	var pre, post []nat.Rule
	for _, r := range rules {
		switch r.Chain {
		case nat.RemapPreChain:
			pre = append(pre, r)
		case nat.RemapPostChain:
			post = append(post, r)
		default:
			t.Fatalf("unexpected chain %q", r.Chain)
		}
	}

	// PRE: -d <virtual> -j NETMAP --to <real>; sorted by virtual.
	wantPre := [][]string{
		{"-d", "172.20.0.0/16", "-j", "NETMAP", "--to", "10.244.0.0/16"},
		{"-d", "172.21.0.0/16", "-j", "NETMAP", "--to", "10.96.0.0/16"},
	}
	for i, r := range pre {
		if !reflect.DeepEqual(r.Args, wantPre[i]) {
			t.Errorf("PRE[%d] = %v, want %v", i, r.Args, wantPre[i])
		}
	}

	// POST: -s <real> -j NETMAP --to <virtual>.
	wantPost := [][]string{
		{"-s", "10.244.0.0/16", "-j", "NETMAP", "--to", "172.20.0.0/16"},
		{"-s", "10.96.0.0/16", "-j", "NETMAP", "--to", "172.21.0.0/16"},
	}
	for i, r := range post {
		if !reflect.DeepEqual(r.Args, wantPost[i]) {
			t.Errorf("POST[%d] = %v, want %v", i, r.Args, wantPost[i])
		}
	}
}

func TestBuildRemapRules_EmptyAndSkips(t *testing.T) {
	if got := nat.BuildRemapRules(nil); len(got) != 0 {
		t.Errorf("nil entries should yield no rules, got %v", got)
	}
	// Incomplete entries are skipped.
	if got := nat.BuildRemapRules([]nat.RemapEntry{{Real: "10.0.0.0/8"}, {Virtual: "172.16.0.0/12"}}); len(got) != 0 {
		t.Errorf("incomplete entries should be skipped, got %v", got)
	}
}
