//go:build dataplane

// Data-plane integration tests for the ClusterSetIP NAT manager. They program
// the real iptables nat table, so they need root + the iptables binary and
// iptable_nat, and run inside a throwaway network namespace so they never touch
// the host's rules. Gated behind the `dataplane` build tag.
//
//	sudo -E env PATH="$PATH" go test -tags dataplane ./pkg/nat/...
package nat_test

import (
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/coreos/go-iptables/iptables"
	"github.com/go-logr/logr"
	"github.com/vishvananda/netns"

	"github.com/datawerx/datawerx/pkg/nat"
)

func TestManager_DataPlane(t *testing.T) {
	skipIfNotRoot(t)
	defer enterTempNetns(t)()

	mgr, err := nat.NewManager(logr.Discard())
	if err != nil {
		t.Skipf("iptables unavailable (binary + iptable_nat required): %v", err)
	}

	svc := nat.ServiceDNAT{
		Namespace: "prod", Name: "payments", VIP: "241.0.0.5",
		Ports:    []nat.PortDNAT{{Protocol: "tcp", Port: 80}},
		Backends: []string{"10.96.0.10", "10.96.0.11"},
	}
	if err := mgr.SyncClusterSetNAT([]nat.ServiceDNAT{svc}); err != nil {
		t.Fatalf("SyncClusterSetNAT: %v", err)
	}

	ipt, err := iptables.New()
	if err != nil {
		t.Fatalf("iptables.New: %v", err)
	}

	// The root chain must be hooked from PREROUTING and OUTPUT.
	assertRootHooked(t, ipt)

	// Both backends must have a DNAT rule.
	for _, target := range []string{"10.96.0.10:80", "10.96.0.11:80"} {
		if !dnatPresent(t, ipt, target) {
			t.Errorf("no DNAT rule targeting %s", target)
		}
	}

	// An empty sync must remove every managed chain (idempotent teardown).
	if err := mgr.SyncClusterSetNAT(nil); err != nil {
		t.Fatalf("empty SyncClusterSetNAT: %v", err)
	}
	assertNoManagedChains(t, ipt, "")

	// Re-syncing the same desired state must be safe (no error, idempotent).
	if err := mgr.SyncClusterSetNAT([]nat.ServiceDNAT{svc}); err != nil {
		t.Fatalf("re-sync after teardown: %v", err)
	}
	if err := mgr.SyncClusterSetNAT([]nat.ServiceDNAT{svc}); err != nil {
		t.Fatalf("repeated sync should be idempotent: %v", err)
	}
}

func TestManager_DualStackDataPlane(t *testing.T) {
	skipIfNotRoot(t)
	defer enterTempNetns(t)()

	ipt6, err := iptables.NewWithProtocol(iptables.ProtocolIPv6)
	if err != nil {
		t.Skipf("ip6tables unavailable: %v", err)
	}
	if ok, err := ipt6.Exists(nat.TableNAT, "PREROUTING", "-j", "RETURN"); err != nil && !ok {
		// A probe that fails outright usually means ip6tables nat is missing
		// (older kernels). Skip rather than fail the suite.
		t.Skipf("ip6tables nat table unusable: %v", err)
	}

	mgr, err := nat.NewManager(logr.Discard())
	if err != nil {
		t.Skipf("iptables unavailable: %v", err)
	}

	v6 := nat.ServiceDNAT{
		Namespace: "prod", Name: "web", VIP: "fd00:cafe::5",
		Ports:    []nat.PortDNAT{{Protocol: "tcp", Port: 80}},
		Backends: []string{"fd00:96::10", "fd00:96::11"},
	}
	if err := mgr.SyncClusterSetNAT([]nat.ServiceDNAT{v6}); err != nil {
		t.Fatalf("SyncClusterSetNAT v6: %v", err)
	}

	// The DNAT rules for the v6 backends must land in the ip6tables nat table.
	for _, target := range []string{"fd00:96::10", "fd00:96::11"} {
		if !dnatPresent(t, ipt6, target) {
			t.Errorf("no v6 DNAT rule targeting %s", target)
		}
	}

	// Teardown is family-aware: an empty sync must clear the v6 managed chains.
	if err := mgr.SyncClusterSetNAT(nil); err != nil {
		t.Fatalf("empty SyncClusterSetNAT: %v", err)
	}
	assertNoManagedChains(t, ipt6, "v6 ")
}

// dnatPresent reports whether any DWX-SEP- chain contains a rule mentioning
// target (family-agnostic; works for both iptables and ip6tables handles).
func dnatPresent(t *testing.T, ipt *iptables.IPTables, target string) bool {
	t.Helper()
	chains, err := ipt.ListChains(nat.TableNAT)
	if err != nil {
		t.Fatalf("ListChains: %v", err)
	}
	for _, c := range chains {
		if !strings.HasPrefix(c, "DWX-SEP-") {
			continue
		}
		rules, err := ipt.List(nat.TableNAT, c)
		if err != nil {
			t.Fatalf("List %s: %v", c, err)
		}
		for _, r := range rules {
			if strings.Contains(r, target) {
				return true
			}
		}
	}
	return false
}

func TestManager_RemapDataPlane(t *testing.T) {
	skipIfNotRoot(t)
	defer enterTempNetns(t)()

	mgr, err := nat.NewManager(logr.Discard())
	if err != nil {
		t.Skipf("iptables unavailable: %v", err)
	}

	entry := nat.RemapEntry{Real: "10.244.0.0/16", Virtual: "172.20.0.0/16"}
	if err := mgr.SyncRemap([]nat.RemapEntry{entry}); err != nil {
		t.Fatalf("SyncRemap: %v", err)
	}

	ipt, err := iptables.New()
	if err != nil {
		t.Fatalf("iptables.New: %v", err)
	}
	// Both remap chains must be hooked.
	for chain, hook := range map[string]string{nat.RemapPreChain: "PREROUTING", nat.RemapPostChain: "POSTROUTING"} {
		ok, err := ipt.Exists(nat.TableNAT, hook, "-j", chain)
		if err != nil {
			t.Fatalf("checking %s hook: %v", hook, err)
		}
		if !ok {
			t.Errorf("%s not hooked from %s", chain, hook)
		}
	}
	// The NETMAP rules must be present (dst virtual→real in PRE, src real→virtual in POST).
	if !rulePresent(t, ipt, nat.RemapPreChain, "172.20.0.0/16", "10.244.0.0/16") {
		t.Error("PRE NETMAP rule missing")
	}
	if !rulePresent(t, ipt, nat.RemapPostChain, "10.244.0.0/16", "172.20.0.0/16") {
		t.Error("POST NETMAP rule missing")
	}

	// Empty sync clears the chains (but leaves them hooked).
	if err := mgr.SyncRemap(nil); err != nil {
		t.Fatalf("empty SyncRemap: %v", err)
	}
	if rules, _ := ipt.List(nat.TableNAT, nat.RemapPreChain); len(rules) > 1 {
		t.Errorf("expected PRE chain flushed, got rules %v", rules)
	}
}

func TestManager_GatewayMasqDataPlane(t *testing.T) {
	skipIfNotRoot(t)
	defer enterTempNetns(t)()

	mgr, err := nat.NewManager(logr.Discard())
	if err != nil {
		t.Skipf("iptables unavailable: %v", err)
	}

	const client = "100.64.0.0/10"
	const dest = "10.96.0.0/16"
	if err := mgr.SyncGatewayMasq([]string{client}, []string{dest}); err != nil {
		t.Fatalf("SyncGatewayMasq: %v", err)
	}

	ipt, err := iptables.New()
	if err != nil {
		t.Fatalf("iptables.New: %v", err)
	}

	// The masquerade chain must be hooked from POSTROUTING.
	ok, err := ipt.Exists(nat.TableNAT, "POSTROUTING", "-j", nat.GatewayMasqChain)
	if err != nil {
		t.Fatalf("checking POSTROUTING hook: %v", err)
	}
	if !ok {
		t.Errorf("%s not hooked from POSTROUTING", nat.GatewayMasqChain)
	}

	// The MASQUERADE rule (client source → mesh dest) must be present.
	if !rulePresent(t, ipt, nat.GatewayMasqChain, client, dest) {
		t.Error("gateway MASQUERADE rule missing")
	}

	// Empty sync clears the chain (but leaves it hooked).
	if err := mgr.SyncGatewayMasq(nil, nil); err != nil {
		t.Fatalf("empty SyncGatewayMasq: %v", err)
	}
	if rules, _ := ipt.List(nat.TableNAT, nat.GatewayMasqChain); len(rules) > 1 {
		t.Errorf("expected gateway chain flushed, got rules %v", rules)
	}

	// Re-syncing the same desired state must be idempotent.
	if err := mgr.SyncGatewayMasq([]string{client}, []string{dest}); err != nil {
		t.Fatalf("re-sync after teardown: %v", err)
	}
	if err := mgr.SyncGatewayMasq([]string{client}, []string{dest}); err != nil {
		t.Fatalf("repeated sync should be idempotent: %v", err)
	}
}

func rulePresent(t *testing.T, ipt *iptables.IPTables, chain, match, to string) bool {
	t.Helper()
	rules, err := ipt.List(nat.TableNAT, chain)
	if err != nil {
		t.Fatalf("List %s: %v", chain, err)
	}
	for _, r := range rules {
		if strings.Contains(r, match) && strings.Contains(r, to) {
			return true
		}
	}
	return false
}

// assertRootHooked checks that the root chain is jumped to from both built-in
// nat hooks.
func assertRootHooked(t *testing.T, ipt *iptables.IPTables) {
	t.Helper()
	for _, hook := range []string{"PREROUTING", "OUTPUT"} {
		ok, err := ipt.Exists(nat.TableNAT, hook, "-j", nat.RootChain)
		if err != nil {
			t.Fatalf("checking %s hook: %v", hook, err)
		}
		if !ok {
			t.Errorf("root chain not hooked from %s", hook)
		}
	}
}

// assertNoManagedChains checks that no DWX-SVC-/DWX-SEP- chains remain. label is
// prepended to the failure message (e.g. "v6 ") to distinguish address families.
func assertNoManagedChains(t *testing.T, ipt *iptables.IPTables, label string) {
	t.Helper()
	chains, err := ipt.ListChains(nat.TableNAT)
	if err != nil {
		t.Fatalf("ListChains: %v", err)
	}
	for _, c := range chains {
		if strings.HasPrefix(c, "DWX-SVC-") || strings.HasPrefix(c, "DWX-SEP-") {
			t.Errorf("%smanaged chain %s remained after empty sync", label, c)
		}
	}
}

func skipIfNotRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("data-plane test requires root")
	}
}

func enterTempNetns(t *testing.T) func() {
	t.Helper()
	runtime.LockOSThread()
	orig, err := netns.Get()
	if err != nil {
		runtime.UnlockOSThread()
		t.Skipf("netns.Get: %v", err)
	}
	fresh, err := netns.New()
	if err != nil {
		_ = netns.Set(orig)
		_ = orig.Close()
		runtime.UnlockOSThread()
		t.Skipf("creating netns (needs CAP_SYS_ADMIN): %v", err)
	}
	return func() {
		_ = fresh.Close()
		_ = netns.Set(orig)
		_ = orig.Close()
		runtime.UnlockOSThread()
	}
}
