//go:build dataplane

// Data-plane integration tests for the mesh firewall manager. They program the
// real iptables filter table, so they need root with the iptables binary, and run
// inside a throwaway network namespace so they never touch the host's rules.
// Gated behind the `dataplane` build tag.
//
//	sudo -E env PATH="$PATH" go test -tags dataplane ./pkg/meshfw/...
package meshfw_test

import (
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/coreos/go-iptables/iptables"
	"github.com/go-logr/logr"
	"github.com/vishvananda/netns"

	"github.com/DataWerx/datawerx-mesh/pkg/meshfw"
)

func TestManager_FirewallDataPlane(t *testing.T) {
	skipIfNotRoot(t)
	defer enterTempNetns(t)()

	mgr, err := meshfw.NewManager("dwx-mesh0", logr.Discard())
	if err != nil {
		t.Skipf("iptables unavailable: %v", err)
	}

	policies := []meshfw.Policy{{
		Name:         "db",
		Destinations: []string{"10.96.5.0/24"},
		Ingress: []meshfw.IngressRule{{
			From:  []meshfw.PeerSelector{{CIDRs: []string{"10.40.0.0/16"}}},
			Ports: []meshfw.Port{{Protocol: "tcp", Port: 5432}},
		}},
	}}
	rs := meshfw.BuildFirewall(policies, nil)
	if err := mgr.SyncFirewall(rs); err != nil {
		t.Fatalf("SyncFirewall: %v", err)
	}

	ipt, err := iptables.New()
	if err != nil {
		t.Fatalf("iptables.New: %v", err)
	}

	// FWChain must be hooked from FORWARD and INPUT for the mesh device.
	assertFWChainHooked(t, ipt)

	rules, err := ipt.List(meshfw.TableFilter, meshfw.FWChain)
	if err != nil {
		t.Fatalf("List FWChain: %v", err)
	}
	joined := strings.Join(rules, "\n")
	for _, want := range []string{"10.40.0.0/16", "10.96.5.0/24", "5432", "DROP"} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected %q in FWChain rules:\n%s", want, joined)
		}
	}

	// An empty sync flushes the chain (but leaves it hooked).
	if err := mgr.SyncFirewall(meshfw.BuildFirewall(nil, nil)); err != nil {
		t.Fatalf("empty SyncFirewall: %v", err)
	}
	rules, _ = ipt.List(meshfw.TableFilter, meshfw.FWChain)
	for _, r := range rules {
		if strings.Contains(r, "DROP") {
			t.Errorf("expected no DROP after empty sync, got %q", r)
		}
	}
}

// TestManager_RebuildGuardFailsClosed validates the fail-closed rebuild guard:
//   - after a SUCCESSFUL sync the guard is disengaged and ot left hooked
//   - repeated syncs are idempotent - no stacked hooks / guard DROPs
//   - if a rebuild ERRORS, the guard is LEFT engaged so mesh ingress is dropped
//     (fail closed) rather than falling through an empty/half-built chain
//   - a subsequent successful sync disengages the guard again.
func TestManager_RebuildGuardFailsClosed(t *testing.T) {
	skipIfNotRoot(t)
	defer enterTempNetns(t)()

	const iface = "dwx-mesh0"
	mgr, err := meshfw.NewManager(iface, logr.Discard())
	if err != nil {
		t.Skipf("iptables unavailable: %v", err)
	}
	ipt, err := iptables.New()
	if err != nil {
		t.Fatalf("iptables.New: %v", err)
	}

	guardHooked := func() bool {
		t.Helper()
		for _, hook := range []string{"FORWARD", "INPUT"} {
			ok, err := ipt.Exists(meshfw.TableFilter, hook, "-i", iface, "-j", meshfw.FWChainGuard)
			if err != nil {
				t.Fatalf("checking %s guard hook: %v", hook, err)
			}
			if ok {
				return true
			}
		}
		return false
	}
	countGuardDrops := func() int {
		t.Helper()
		rules, err := ipt.List(meshfw.TableFilter, meshfw.FWChainGuard)
		if err != nil {
			t.Fatalf("List guard chain: %v", err)
		}
		n := 0
		for _, r := range rules {
			if strings.Contains(r, "DROP") {
				n++
			}
		}
		return n
	}

	// The guard chain must exist with exactly one blanket DROP after construction.
	if got := countGuardDrops(); got != 1 {
		t.Fatalf("guard chain DROP count = %d, want 1", got)
	}

	// A successful sync must leave the guard DISENGAGED.
	rs := meshfw.BuildFirewall([]meshfw.Policy{{Name: "deny-all"}}, nil)
	if err := mgr.SyncFirewall(rs); err != nil {
		t.Fatalf("SyncFirewall: %v", err)
	}
	if guardHooked() {
		t.Error("guard still engaged after a successful sync (should fail closed only DURING rebuild)")
	}
	assertFWChainHooked(t, ipt)

	// Idempotent: a second identical sync must not stack hooks or guard DROPs.
	if err := mgr.SyncFirewall(rs); err != nil {
		t.Fatalf("second SyncFirewall: %v", err)
	}
	if got := countGuardDrops(); got != 1 {
		t.Errorf("guard DROP count = %d after re-sync, want 1 (no stacking)", got)
	}
	if guardHooked() {
		t.Error("guard engaged after a second successful sync")
	}

	// Inject a rebuild failure: an invalid rule makes Append fail mid-rebuild.
	// The guard MUST remain engaged afterward (fail closed).
	bad := meshfw.Ruleset{Rules: []meshfw.Rule{
		{Chain: meshfw.FWChain, Args: []string{"-p", "tcp", "--dport", "notaport", "-j", "ACCEPT"}},
	}}
	if err := mgr.SyncFirewall(bad); err == nil {
		t.Fatal("expected SyncFirewall to fail on an invalid rule")
	}
	if !guardHooked() {
		t.Error("guard NOT engaged after a failed rebuild: firewall would fail OPEN")
	}

	// Recovery: a subsequent valid sync must succeed and disengage the guard.
	if err := mgr.SyncFirewall(rs); err != nil {
		t.Fatalf("recovery SyncFirewall: %v", err)
	}
	if guardHooked() {
		t.Error("guard still engaged after recovery sync")
	}
}

// assertFWChainHooked checks that FWChain is jumped to from FORWARD and INPUT
// for traffic arriving on the mesh device.
func assertFWChainHooked(t *testing.T, ipt *iptables.IPTables) {
	t.Helper()
	for _, hook := range []string{"FORWARD", "INPUT"} {
		ok, err := ipt.Exists(meshfw.TableFilter, hook, "-i", "dwx-mesh0", "-j", meshfw.FWChain)
		if err != nil {
			t.Fatalf("checking %s hook: %v", hook, err)
		}
		if !ok {
			t.Errorf("FWChain not hooked from %s", hook)
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
