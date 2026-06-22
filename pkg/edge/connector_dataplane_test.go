//go:build dataplane

// Data-plane integration tests for the edge device connector. They program the
// real kernel: a dedicated `dwx-edge0` WireGuard terminator, the pure pkg/edge
// planner driving it, and a second-netns "device" dialing in through a veth as it
// would from behind NAT. They need root, the `wireguard` kernel module, and
// CAP_SYS_ADMIN for throwaway network namespaces. Gated behind the `dataplane`
// build tag; run in the privileged CI job, never in the default `go test ./...`.
//
//	sudo -E env PATH="$PATH" go test -tags dataplane ./pkg/edge/...
//
// The terminator is byte-identical regardless of the mesh data plane (it never
// touches dwx-mesh0), so these tests do not vary by DataWerx_DATAPLANE. The full
// out-of-cluster "reach a *.clusterset.local service" masquerade path — which
// needs a real Service backend and the gateway onward leg — is asserted in the
// kind e2e (test/e2e), the correct layer for an end-to-end L7 path, and is run
// once with the native data plane and once with routed.
package edge_test

import (
	"net"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/DataWerx/datawerx-mesh/pkg/edge"
	"github.com/DataWerx/datawerx-mesh/pkg/wg"
)

// TestEdgeTerminator_DataPlane brings up a real `dwx-edge0` terminator and drives
// it with the pure planner exactly as the premium reconciler does: allocate the
// device address, plan the peer, program it as a roaming /32, then tear it down.
func TestEdgeTerminator_DataPlane(t *testing.T) {
	skipIfNotRoot(t)
	defer enterTempNetns(t)()

	termPriv, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	const iface = "dwx-edge0"
	mgr, err := wg.NewWireGuardManager(iface, logr.Discard(), wg.WithListenPort(51821))
	if err != nil {
		t.Skipf("wgctrl unavailable: %v", err)
	}
	defer mgr.Close()
	if err := mgr.SyncInterface(termPriv.String()); err != nil {
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
	if dev.ListenPort != 51821 {
		t.Errorf("listen port = %d, want 51821 (distinct from the mesh's 51820)", dev.ListenPort)
	}

	// Pure planner: deterministic address, then a roaming device peer whose only
	// AllowedIP is its own /32.
	devPriv, _ := wgtypes.GeneratePrivateKey()
	devPub := devPriv.PublicKey().String()
	alloc, err := edge.AllocateDeviceIPs("100.71.0.0/24", []edge.DeviceClaim{{Key: devPub}})
	if err != nil {
		t.Fatalf("AllocateDeviceIPs: %v", err)
	}
	addr := alloc[devPub]
	peer, err := edge.PlanDevicePeer(devPub, addr)
	if err != nil {
		t.Fatalf("PlanDevicePeer: %v", err)
	}
	if err := mgr.ConfigurePeer(peer.PublicKey, "", peer.AllowedIPs); err != nil {
		t.Fatalf("ConfigurePeer: %v", err)
	}

	link, err := netlink.LinkByName(iface)
	if err != nil {
		t.Fatalf("LinkByName: %v", err)
	}
	dev, _ = wgc.Device(iface)
	if !hasPeer(dev, devPub) {
		t.Fatalf("device peer %s was not programmed", devPub)
	}
	if !hasRoute(t, link, peer.AllowedIPs[0]) {
		t.Errorf("device host route %s was not installed", peer.AllowedIPs[0])
	}

	// The device-side profile renders against this terminator.
	profile, err := edge.BuildDeviceProfile(edge.DeviceProfileInput{
		DeviceID: "dataplane-dev", PublicKey: devPub, Address: addr,
		EdgeEndpoint: "10.0.0.1:51821", PeerPublicKey: termPriv.PublicKey().String(),
		RouteCIDRs: []string{"241.0.0.0/8"},
	})
	if err != nil {
		t.Fatalf("BuildDeviceProfile: %v", err)
	}
	if !strings.Contains(profile.WireGuardQuickConfig(""), "[Peer]") {
		t.Error("rendered wg-quick config missing [Peer]")
	}

	// Teardown withdraws both the peer and its route.
	if err := mgr.RemovePeer(devPub); err != nil {
		t.Fatalf("RemovePeer: %v", err)
	}
	dev, _ = wgc.Device(iface)
	if hasPeer(dev, devPub) {
		t.Error("device peer still present after RemovePeer")
	}
	if hasRoute(t, link, peer.AllowedIPs[0]) {
		t.Error("device route still present after RemovePeer")
	}
}

// TestEdgeTerminator_DeviceDialIn stands up the terminator and a separate-netns
// device connected only by a veth, with the device dialing the terminator
// outbound (as it would from behind NAT) and a short keepalive holding the
// pinhole open. It asserts the terminator records a cryptographic handshake —
// the core proof that an outbound-only device reaches the edge ingress.
func TestEdgeTerminator_DeviceDialIn(t *testing.T) {
	skipIfNotRoot(t)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	orig, err := netns.Get()
	if err != nil {
		t.Skipf("netns.Get: %v", err)
	}
	defer func() { _ = netns.Set(orig); _ = orig.Close() }()

	nsTerm, err := netns.New() // creates and switches
	if err != nil {
		t.Skipf("creating terminator netns (needs CAP_SYS_ADMIN): %v", err)
	}
	defer nsTerm.Close()
	nsDev, err := netns.New() // creates and switches; current = nsDev
	if err != nil {
		t.Skipf("creating device netns: %v", err)
	}
	defer nsDev.Close()

	// veth pair: build in the device netns, move one end into the terminator netns.
	if err := netlink.LinkAdd(&netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: "veth-dev"},
		PeerName:  "veth-term",
	}); err != nil {
		t.Fatalf("creating veth pair: %v", err)
	}
	termEnd, err := netlink.LinkByName("veth-term")
	if err != nil {
		t.Fatalf("LinkByName veth-term: %v", err)
	}
	if err := netlink.LinkSetNsFd(termEnd, int(nsTerm)); err != nil {
		t.Fatalf("moving veth-term to terminator netns: %v", err)
	}
	configureVeth(t, "veth-dev", "10.123.0.2/24")

	// Device side: bring up dwx-dev0 and dial the terminator with a 1s keepalive.
	devPriv, _ := wgtypes.GeneratePrivateKey()
	termPriv, _ := wgtypes.GeneratePrivateKey()
	devMgr, err := wg.NewWireGuardManager("dwx-dev0", logr.Discard(),
		wg.WithListenPort(51890), wg.WithKeepalive(time.Second))
	if err != nil {
		t.Skipf("wgctrl unavailable: %v", err)
	}
	defer devMgr.Close()
	if err := devMgr.SyncInterface(devPriv.String()); err != nil {
		t.Skipf("device SyncInterface failed: %v", err)
	}
	// AllowedIPs is a mesh range distinct from the veth subnet so the outer
	// transport to 10.123.0.1 is not captured by the tunnel route.
	if err := devMgr.ConfigurePeer(termPriv.PublicKey().String(), "10.123.0.1:51821", []string{"10.200.0.0/16"}); err != nil {
		t.Fatalf("device ConfigurePeer: %v", err)
	}

	// Terminator side: switch to its netns, wire the veth, bring up dwx-edge0, and
	// program the device as a roaming /32 peer.
	if err := netns.Set(nsTerm); err != nil {
		t.Fatalf("switching to terminator netns: %v", err)
	}
	configureVeth(t, "veth-term", "10.123.0.1/24")
	termMgr, err := wg.NewWireGuardManager("dwx-edge0", logr.Discard(), wg.WithListenPort(51821))
	if err != nil {
		t.Skipf("wgctrl unavailable: %v", err)
	}
	defer termMgr.Close()
	if err := termMgr.SyncInterface(termPriv.String()); err != nil {
		t.Skipf("terminator SyncInterface failed: %v", err)
	}
	devPub := devPriv.PublicKey().String()
	peer, err := edge.PlanDevicePeer(devPub, "100.71.0.5")
	if err != nil {
		t.Fatalf("PlanDevicePeer: %v", err)
	}
	if err := termMgr.ConfigurePeer(peer.PublicKey, "", peer.AllowedIPs); err != nil {
		t.Fatalf("terminator ConfigurePeer: %v", err)
	}

	// termMgr's control socket is bound to the terminator netns, so PeerHandshake
	// queries it regardless of the current thread netns.
	deadline := time.Now().Add(20 * time.Second)
	var hs int64
	for time.Now().Before(deadline) {
		if hs, err = termMgr.PeerHandshake(devPub); err == nil && hs > 0 {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if hs == 0 {
		t.Fatal("terminator recorded no handshake from the dialed-in device within the timeout")
	}
}

// configureVeth assigns an address to a veth end in the current netns and brings
// it (and loopback) up.
func configureVeth(t *testing.T, name, cidr string) {
	t.Helper()
	link, err := netlink.LinkByName(name)
	if err != nil {
		t.Fatalf("LinkByName %s: %v", name, err)
	}
	addr, err := netlink.ParseAddr(cidr)
	if err != nil {
		t.Fatalf("ParseAddr %s: %v", cidr, err)
	}
	if err := netlink.AddrAdd(link, addr); err != nil {
		t.Fatalf("AddrAdd %s on %s: %v", cidr, name, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		t.Fatalf("LinkSetUp %s: %v", name, err)
	}
	if lo, err := netlink.LinkByName("lo"); err == nil {
		_ = netlink.LinkSetUp(lo)
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
// returns a function that restores the original. Mirrors pkg/wg.
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
