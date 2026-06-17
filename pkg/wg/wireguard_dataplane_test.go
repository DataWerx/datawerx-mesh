//go:build dataplane

// Data-plane integration tests for the WireGuard manager. They program the real
// kernel, creating the device, loading keys, and installing routes.  As such,
// they need root, the `wireguard` kernel module, and CAP_SYS_ADMIN to create a
// throwaway network namespace. Gated behind the `dataplane` build tag and run
// in the privileged CI job, never in the default `go test ./...`.
//
//	sudo -E env PATH="$PATH" go test -tags dataplane ./pkg/wg/...
package wg_test

import (
	"net"
	"os"
	"runtime"
	"testing"

	"github.com/go-logr/logr"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/DataWerx/datawerx-mesh/pkg/wg"
)

func TestWireGuardManager_DataPlane(t *testing.T) {
	skipIfNotRoot(t)
	defer enterTempNetns(t)()

	priv, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	const iface = "dwx-test0"
	mgr, err := wg.NewWireGuardManager(iface, logr.Discard())
	if err != nil {
		t.Skipf("wgctrl unavailable: %v", err)
	}
	defer mgr.Close()

	// SyncInterface must create the device, set the listen port, and bring it up.
	if err := mgr.SyncInterface(priv.String()); err != nil {
		t.Skipf("SyncInterface failed (is the wireguard module loaded?): %v", err)
	}

	wgc, err := wgctrl.New()
	if err != nil {
		t.Fatalf("wgctrl.New: %v", err)
	}
	defer wgc.Close()

	dev, err := wgc.Device(iface)
	if err != nil {
		t.Fatalf("reading device: %v", err)
	}
	if dev.ListenPort != wg.DefaultListenPort {
		t.Errorf("listen port = %d, want %d", dev.ListenPort, wg.DefaultListenPort)
	}
	if dev.PrivateKey != priv {
		t.Error("device private key does not match the one synced")
	}

	link, err := netlink.LinkByName(iface)
	if err != nil {
		t.Fatalf("LinkByName: %v", err)
	}
	if link.Attrs().Flags&net.FlagUp == 0 {
		t.Error("interface is not administratively up")
	}

	// ConfigurePeer must program the peer and install the route.
	peerKey, _ := wgtypes.GeneratePrivateKey()
	peerPub := peerKey.PublicKey().String()
	const cidr = "10.99.0.0/24"
	if err := mgr.ConfigurePeer(peerPub, "", []string{cidr}); err != nil {
		t.Fatalf("ConfigurePeer: %v", err)
	}

	dev, _ = wgc.Device(iface)
	if !hasPeer(dev, peerPub) {
		t.Fatalf("peer %s was not programmed into the device", peerPub)
	}
	if !hasRoute(t, link, cidr) {
		t.Errorf("route %s was not installed", cidr)
	}

	// RemovePeer must withdraw both the peer and its route.
	if err := mgr.RemovePeer(peerPub); err != nil {
		t.Fatalf("RemovePeer: %v", err)
	}
	dev, _ = wgc.Device(iface)
	if hasPeer(dev, peerPub) {
		t.Error("peer still present after RemovePeer")
	}
	if hasRoute(t, link, cidr) {
		t.Error("route still present after RemovePeer")
	}
}

// TestWireGuardManager_RouteWithdrawalOnShrink exercises the route-leak fix:
// when a peer's CIDR set shrinks or changes, ConfigurePeer must withdraw the
// host routes that are no longer desired, not just converge AllowedIPs, and
// must keep its bookkeeping accurate so a later RemovePeer can still clean up.
func TestWireGuardManager_RouteWithdrawalOnShrink(t *testing.T) {
	skipIfNotRoot(t)
	defer enterTempNetns(t)()

	priv, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	const iface = "dwx-test1"
	mgr, err := wg.NewWireGuardManager(iface, logr.Discard())
	if err != nil {
		t.Skipf("wgctrl unavailable: %v", err)
	}
	defer mgr.Close()
	if err := mgr.SyncInterface(priv.String()); err != nil {
		t.Skipf("SyncInterface failed (is the wireguard module loaded?): %v", err)
	}

	link, err := netlink.LinkByName(iface)
	if err != nil {
		t.Fatalf("LinkByName: %v", err)
	}

	peerKey, _ := wgtypes.GeneratePrivateKey()
	peerPub := peerKey.PublicKey().String()

	const (
		cidrA = "10.10.0.0/16"
		cidrB = "10.20.0.0/16"
		cidrC = "10.30.0.0/16"
	)

	// Program two CIDRs; both routes must be installed.
	if err := mgr.ConfigurePeer(peerPub, "", []string{cidrA, cidrB}); err != nil {
		t.Fatalf("ConfigurePeer (two CIDRs): %v", err)
	}
	if !hasRoute(t, link, cidrA) || !hasRoute(t, link, cidrB) {
		t.Fatalf("expected both %s and %s routes after initial program", cidrA, cidrB)
	}

	// Shrink to one CIDR: the dropped CIDR's route MUST be withdrawn (the leak
	// fix), while the retained one stays. Without the fix, cidrB would linger and
	// keep hijacking that prefix into the tunnel.
	if err := mgr.ConfigurePeer(peerPub, "", []string{cidrA}); err != nil {
		t.Fatalf("ConfigurePeer (shrink): %v", err)
	}
	if !hasRoute(t, link, cidrA) {
		t.Errorf("retained route %s was wrongly removed", cidrA)
	}
	if hasRoute(t, link, cidrB) {
		t.Errorf("stale route %s leaked: not withdrawn when its CIDR was removed", cidrB)
	}

	// Change the CIDR entirely: old route withdrawn, new route installed.
	if err := mgr.ConfigurePeer(peerPub, "", []string{cidrC}); err != nil {
		t.Fatalf("ConfigurePeer (change): %v", err)
	}
	if hasRoute(t, link, cidrA) {
		t.Errorf("stale route %s leaked after CIDR change", cidrA)
	}
	if !hasRoute(t, link, cidrC) {
		t.Errorf("new route %s not installed after CIDR change", cidrC)
	}

	// RemovePeer must clean up the current route. Bookkeeping stayed accurate
	// across the shrink/change, so the leaked route is not orphaned.
	if err := mgr.RemovePeer(peerPub); err != nil {
		t.Fatalf("RemovePeer: %v", err)
	}
	if hasRoute(t, link, cidrC) {
		t.Errorf("route %s still present after RemovePeer", cidrC)
	}
}

func hasPeer(dev *wgtypes.Device, pub string) bool {
	for i := range dev.Peers {
		if dev.Peers[i].PublicKey.String() == pub {
			return true
		}
	}
	return false
}

func hasRoute(t *testing.T, link netlink.Link, cidr string) bool {
	t.Helper()
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatalf("parse cidr: %v", err)
	}
	routes, err := netlink.RouteList(link, netlink.FAMILY_V4)
	if err != nil {
		t.Fatalf("RouteList: %v", err)
	}
	for _, r := range routes {
		if r.Dst != nil && r.Dst.String() == ipnet.String() {
			return true
		}
	}
	return false
}

func skipIfNotRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("data-plane test requires root")
	}
}

// enterTempNetns moves the current OS thread into a fresh network namespace and
// returns a function that restores the original namespace. The thread stays
// locked for the duration so netlink/wgctrl operations target the temp netns.
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
