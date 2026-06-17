package nat

import (
	"strings"
	"testing"

	"github.com/coreos/go-iptables/iptables"
	"github.com/go-logr/logr"
)

// fakeIPT is an in-memory model of an iptables handle that records every chain
// and rule, faithfully enough to verify the manager's apply orchestration
// (chain creation, single hook insertion, full-chain rebuild) without root.
type fakeIPT struct {
	proto  iptables.Protocol
	chains map[string]bool     // "table/chain" -> exists
	rules  map[string][]string // "table/chain" -> ordered rules (args joined by space)
}

func newFakeIPT(proto iptables.Protocol) *fakeIPT {
	f := &fakeIPT{proto: proto, chains: map[string]bool{}, rules: map[string][]string{}}
	for _, b := range []string{"PREROUTING", "OUTPUT", "POSTROUTING", "INPUT"} {
		f.chains[fk(TableNAT, b)] = true // built-in chains always exist
	}
	return f
}

func fk(table, chain string) string { return table + "/" + chain }

func (f *fakeIPT) ChainExists(t, c string) (bool, error) { return f.chains[fk(t, c)], nil }

func (f *fakeIPT) NewChain(t, c string) error {
	if f.chains[fk(t, c)] {
		return &iptables.Error{} // mirror real iptables: creating an existing chain errors
	}
	f.chains[fk(t, c)] = true
	return nil
}

func (f *fakeIPT) Exists(t, c string, spec ...string) (bool, error) {
	want := strings.Join(spec, " ")
	for _, r := range f.rules[fk(t, c)] {
		if r == want {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeIPT) Insert(t, c string, pos int, spec ...string) error {
	k := fk(t, c)
	rs := f.rules[k]
	idx := pos - 1 // iptables positions are 1-based
	if idx < 0 || idx > len(rs) {
		idx = len(rs)
	}
	out := make([]string, 0, len(rs)+1)
	out = append(out, rs[:idx]...)
	out = append(out, strings.Join(spec, " "))
	out = append(out, rs[idx:]...)
	f.rules[k] = out
	return nil
}

func (f *fakeIPT) ClearChain(t, c string) error {
	f.chains[fk(t, c)] = true // ClearChain also creates the chain if missing
	f.rules[fk(t, c)] = nil
	return nil
}

func (f *fakeIPT) Append(t, c string, spec ...string) error {
	k := fk(t, c)
	f.rules[k] = append(f.rules[k], strings.Join(spec, " "))
	return nil
}

func (f *fakeIPT) ListChains(t string) ([]string, error) {
	var out []string
	for k := range f.chains {
		if parts := strings.SplitN(k, "/", 2); parts[0] == t {
			out = append(out, parts[1])
		}
	}
	return out, nil
}

func (f *fakeIPT) DeleteChain(t, c string) error {
	delete(f.chains, fk(t, c))
	delete(f.rules, fk(t, c))
	return nil
}

func (f *fakeIPT) Proto() iptables.Protocol { return f.proto }

// count returns how many rules in the chain exactly equal want.
func (f *fakeIPT) count(chain, want string) int {
	n := 0
	for _, r := range f.rules[fk(TableNAT, chain)] {
		if r == want {
			n++
		}
	}
	return n
}

func newTestManager(ipt, ipt6 iptablesRules) *Manager {
	return &Manager{ipt: ipt, ipt6: ipt6, log: logr.Discard()}
}

const nomasqJump = "-j " + NoMasqChain

func TestSyncMeshNoMasq_AppliesRulesAndHook(t *testing.T) {
	f := newFakeIPT(iptables.ProtocolIPv4)
	m := newTestManager(f, nil)

	if err := m.SyncMeshNoMasq([]string{"10.244.0.0/16"}, []string{"10.97.0.0/16", "10.245.0.0/16"}); err != nil {
		t.Fatalf("SyncMeshNoMasq: %v", err)
	}

	if !f.chains[fk(TableNAT, NoMasqChain)] {
		t.Fatalf("%s chain was not created", NoMasqChain)
	}
	if got := f.count("POSTROUTING", nomasqJump); got != 1 {
		t.Errorf("POSTROUTING jump count = %d, want 1", got)
	}
	want := []string{
		"-s 10.244.0.0/16 -d 10.245.0.0/16 -j ACCEPT", // sorted by remote: 10.245 before 10.97
		"-s 10.244.0.0/16 -d 10.97.0.0/16 -j ACCEPT",
	}
	if got := f.rules[fk(TableNAT, NoMasqChain)]; !equalStrs(got, want) {
		t.Errorf("chain rules = %v, want %v", got, want)
	}
}

func TestSyncMeshNoMasq_Idempotent(t *testing.T) {
	f := newFakeIPT(iptables.ProtocolIPv4)
	m := newTestManager(f, nil)

	local, remote := []string{"10.244.0.0/16"}, []string{"10.245.0.0/16"}
	for i := 0; i < 3; i++ {
		if err := m.SyncMeshNoMasq(local, remote); err != nil {
			t.Fatalf("sync %d: %v", i, err)
		}
	}
	if got := f.count("POSTROUTING", nomasqJump); got != 1 {
		t.Errorf("after 3 syncs, POSTROUTING jump count = %d, want 1 (no duplicate hooks)", got)
	}
	want := []string{"-s 10.244.0.0/16 -d 10.245.0.0/16 -j ACCEPT"}
	if got := f.rules[fk(TableNAT, NoMasqChain)]; !equalStrs(got, want) {
		t.Errorf("after 3 syncs, chain rules = %v, want %v (rebuilt, not appended)", got, want)
	}
}

func TestSyncMeshNoMasq_ClearsOnEmpty(t *testing.T) {
	f := newFakeIPT(iptables.ProtocolIPv4)
	m := newTestManager(f, nil)

	if err := m.SyncMeshNoMasq([]string{"10.244.0.0/16"}, []string{"10.245.0.0/16"}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(f.rules[fk(TableNAT, NoMasqChain)]) != 1 {
		t.Fatalf("setup: expected 1 rule, got %v", f.rules[fk(TableNAT, NoMasqChain)])
	}
	// Peers gone → empty remote set → chain must be cleared (no stale exemption).
	if err := m.SyncMeshNoMasq([]string{"10.244.0.0/16"}, nil); err != nil {
		t.Fatalf("clear sync: %v", err)
	}
	if got := f.rules[fk(TableNAT, NoMasqChain)]; len(got) != 0 {
		t.Errorf("chain rules after clear = %v, want empty", got)
	}
	if got := f.count("POSTROUTING", nomasqJump); got != 1 {
		t.Errorf("hook should remain after clear, jump count = %d, want 1", got)
	}
}

func TestSyncMeshNoMasq_FamilySplit(t *testing.T) {
	v4 := newFakeIPT(iptables.ProtocolIPv4)
	v6 := newFakeIPT(iptables.ProtocolIPv6)
	m := newTestManager(v4, v6)

	err := m.SyncMeshNoMasq(
		[]string{"10.244.0.0/16", "fd00:aaaa::/64"},
		[]string{"10.245.0.0/16", "fd00:beef::/64"},
	)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got, want := f4Rules(v4), []string{"-s 10.244.0.0/16 -d 10.245.0.0/16 -j ACCEPT"}; !equalStrs(got, want) {
		t.Errorf("v4 chain = %v, want %v", got, want)
	}
	if got, want := f4Rules(v6), []string{"-s fd00:aaaa::/64 -d fd00:beef::/64 -j ACCEPT"}; !equalStrs(got, want) {
		t.Errorf("v6 chain = %v, want %v", got, want)
	}
}

func TestSyncMeshNoMasq_IPv6DroppedWhenNoIP6tables(t *testing.T) {
	v4 := newFakeIPT(iptables.ProtocolIPv4)
	m := newTestManager(v4, nil) // no ip6tables handle

	// v6 remote present but unsupported: must not panic/error, and the v4 chain
	// must still be synced from the v4 inputs.
	err := m.SyncMeshNoMasq(
		[]string{"10.244.0.0/16", "fd00:aaaa::/64"},
		[]string{"10.245.0.0/16", "fd00:beef::/64"},
	)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got, want := f4Rules(v4), []string{"-s 10.244.0.0/16 -d 10.245.0.0/16 -j ACCEPT"}; !equalStrs(got, want) {
		t.Errorf("v4 chain = %v, want %v (v6 dropped, v4 intact)", got, want)
	}
}

func f4Rules(f *fakeIPT) []string { return f.rules[fk(TableNAT, NoMasqChain)] }

func equalStrs(a, b []string) bool {
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
