//go:build dataplane

// Data-plane integration test for the MSS-clamp manager. It programs the real
// mangle table, so it needs root + the iptables binary, and runs inside a
// throwaway network namespace so it never touches the host. Gated behind the
// `dataplane` build tag.
//
//	sudo -E env PATH="$PATH" go test -tags dataplane ./pkg/mtu/...
package mtu_test

import (
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/coreos/go-iptables/iptables"
	"github.com/go-logr/logr"
	"github.com/vishvananda/netns"

	"github.com/DataWerx/datawerx-mesh/pkg/mtu"
)

func TestManager_ClampDataPlane(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("data-plane test requires root")
	}
	defer enterTempNetns(t)()

	mgr, err := mtu.NewManager(logr.Discard())
	if err != nil {
		t.Skipf("iptables unavailable: %v", err)
	}
	if err := mgr.EnsureClamp("dwx-mesh0"); err != nil {
		t.Fatalf("EnsureClamp: %v", err)
	}

	ipt, err := iptables.New()
	if err != nil {
		t.Fatalf("iptables.New: %v", err)
	}
	// The chain must be hooked from POSTROUTING in the mangle table.
	ok, err := ipt.Exists(mtu.TableMangle, "POSTROUTING", "-j", mtu.MSSChain)
	if err != nil {
		t.Fatalf("checking hook: %v", err)
	}
	if !ok {
		t.Errorf("%s not hooked from POSTROUTING", mtu.MSSChain)
	}
	// The clamp rule must be present.
	rules, err := ipt.List(mtu.TableMangle, mtu.MSSChain)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var found bool
	for _, r := range rules {
		if strings.Contains(r, "TCPMSS") && strings.Contains(r, "dwx-mesh0") {
			found = true
		}
	}
	if !found {
		t.Errorf("MSS clamp rule missing: %v", rules)
	}

	// Re-ensure is idempotent.
	if err := mgr.EnsureClamp("dwx-mesh0"); err != nil {
		t.Fatalf("re-ensure: %v", err)
	}
	// Clearing to an empty iface flushes the rule but leaves the chain hooked.
	if err := mgr.EnsureClamp(""); err != nil {
		t.Fatalf("empty ensure: %v", err)
	}
	if rules, _ := ipt.List(mtu.TableMangle, mtu.MSSChain); len(rules) > 1 {
		t.Errorf("expected chain flushed, got %v", rules)
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
