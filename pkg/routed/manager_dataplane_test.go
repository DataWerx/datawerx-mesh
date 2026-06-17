//go:build dataplane

// Data-plane integration test for the routed/BYO-overlay manager. It programs
// real kernel routes, so it needs root + netlink, and runs inside a throwaway
// network namespace so it never touches the host routing table. Gated behind the
// `dataplane` build tag.
//
//	sudo -E env PATH="$PATH" go test -tags dataplane ./pkg/routed/...
package routed_test

import (
	"net"
	"os"
	"runtime"
	"testing"

	"github.com/go-logr/logr"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"

	"github.com/DataWerx/datawerx-mesh/pkg/routed"
)

func TestManager_RoutedDataPlane(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("data-plane test requires root")
	}
	defer enterTempNetns(t)()

	// Stand up a dummy "overlay" device with an address, so a route via an
	// on-link next-hop in its subnet is installable.
	const iface = "dwxtest0"
	la := netlink.NewLinkAttrs()
	la.Name = iface
	if err := netlink.LinkAdd(&netlink.Dummy{LinkAttrs: la}); err != nil {
		t.Skipf("creating dummy link: %v", err)
	}
	link, err := netlink.LinkByName(iface)
	if err != nil {
		t.Fatalf("LinkByName: %v", err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		t.Fatalf("LinkSetUp: %v", err)
	}
	addr, _ := netlink.ParseAddr("100.64.0.1/10")
	if err := netlink.AddrAdd(link, addr); err != nil {
		t.Fatalf("AddrAdd: %v", err)
	}

	mgr, err := routed.NewManager(iface, logr.Discard())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	const dest = "10.96.0.0/16"
	if err := mgr.ConfigurePeer("cluster-b", "100.64.0.9", []string{dest}); err != nil {
		t.Fatalf("ConfigurePeer: %v", err)
	}
	if !routeExists(t, dest) {
		t.Errorf("expected route to %s after ConfigurePeer", dest)
	}

	if err := mgr.RemovePeer("cluster-b"); err != nil {
		t.Fatalf("RemovePeer: %v", err)
	}
	if routeExists(t, dest) {
		t.Errorf("expected route to %s withdrawn after RemovePeer", dest)
	}
}

func routeExists(t *testing.T, cidr string) bool {
	t.Helper()
	_, want, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatalf("ParseCIDR: %v", err)
	}
	routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		t.Fatalf("RouteList: %v", err)
	}
	for _, r := range routes {
		if r.Dst != nil && r.Dst.String() == want.String() {
			return true
		}
	}
	return false
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
