// Package wg implements the node-local WireGuard data-plane manager for
// DataWerx Mesh.
//
// The manager owns a single virtual link, `dwx-mesh0`, and is responsible for:
//
//   - bringing the device up and loading the node's private key and listen port
//     SyncInterface,
//   - programming remote peers and installing the host routes that funnel
//     remote-cluster CIDRs into the device (ConfigurePeer), and
//   - tearing peers and their routes down cleanly (RemovePeer).
//
// It is designed to run inside a memory-lean DaemonSet pod. All exported
// methods are safe for concurrent use: the reconciler may process many MeshPeer
// objects in parallel, and every mutation of kernel state is serialized through
// a single RWMutex while bookkeeping reads can proceed concurrently.
//
// Two kernel subsystems are touched, both over Netlink sockets:
//
//   - The WireGuard generic-netlink family (via golang.zx2c4.com/wireguard
//     wgctrl) configures the cryptokey routing table: private key, listen port
//     and per-peer public keys / endpoints / allowed-IPs.
//   - The rtnetlink family (via vishvananda/netlink) creates the link, sets it
//     administratively up, and installs routes in the host routing table.
package wg

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// DefaultInterfaceName is the fixed name of the managed mesh link.
const DefaultInterfaceName = "dwx-mesh0"

// DefaultListenPort is the UDP port WireGuard listens on by default.
const DefaultListenPort = 51820

// persistentKeepalive keeps NAT/firewall pinholes open for peers behind NAT.
// 25s is the community-standard interval.
const persistentKeepalive = 25 * time.Second

// WireGuardManager owns the lifecycle of the `dwx-mesh0` device and its peers.
//
// The zero value is not usable; construct via NewWireGuardManager so the wgctrl
// handle is opened. The manager keeps a record of the routes it installed for
// each peer so that RemovePeer can withdraw exactly what it added and never
// orphans entries in the host routing table.
type WireGuardManager struct {
	// mu guards every interaction with kernel state and the bookkeeping maps
	// below. We use an RWMutex so read-only introspection (e.g. Metrics) does
	// not block on concurrent reads while still fully serializing mutations.
	mu sync.RWMutex

	ifaceName  string
	listenPort int
	keepalive  time.Duration
	mtu        int

	// addrs are the local addresses (CIDR form, e.g. "10.244.255.254/32") to
	// assign to the device in SyncInterface. The mesh link is otherwise
	// address-less, which is fine for pod-sourced traffic but leaves
	// node/gateway-originated traffic (e.g. a masqueraded remote client) with no
	// mesh-routable source. Giving the device a mesh address lets that traffic
	// egress cross-cluster correctly. Empty keeps the historical address-less
	// behavior.
	addrs []string

	// wg is the open WireGuard control client. It is created once and reused;
	// opening a generic-netlink socket per call would be wasteful in the hot
	// reconcile path.
	wg *wgctrl.Client

	// peerRoutes maps a peer public key to the set of CIDR strings for which we
	// installed host routes. It is the authoritative record used during
	// teardown so we only ever delete routes we own.
	peerRoutes map[string][]string

	log logr.Logger
}

// Option customizes a WireGuardManager at construction.
type Option func(*WireGuardManager)

// WithListenPort overrides the UDP port WireGuard listens on. A value <= 0 keeps
// the default. Useful when 51820 is blocked and traffic must use another port.
func WithListenPort(port int) Option {
	return func(m *WireGuardManager) {
		if port > 0 {
			m.listenPort = port
		}
	}
}

// WithKeepalive overrides the persistent-keepalive interval applied to peers
// (NAT/firewall pinhole maintenance). A value <= 0 keeps the default.
func WithKeepalive(d time.Duration) Option {
	return func(m *WireGuardManager) {
		if d > 0 {
			m.keepalive = d
		}
	}
}

// WithMTU sets the mesh device's MTU. A value <= 0 leaves the kernel default
// (1420 for WireGuard links). Lowering it can help when the underlying path MTU
// is smaller than the default; TCP is additionally protected by the MSS clamp
// (pkg/mtu), but a correct device MTU also helps non-TCP and PMTUD-capable flows.
func WithMTU(mtu int) Option {
	return func(m *WireGuardManager) {
		if mtu > 0 {
			m.mtu = mtu
		}
	}
}

// WithAddress configures one or more local addresses (in CIDR form, e.g.
// "10.244.255.254/32") to assign to the mesh device on SyncInterface. Empty or
// blank entries are ignored, so passing an unset config value is a no-op that
// keeps the device address-less. Multiple entries support dual-stack.
func WithAddress(addrs ...string) Option {
	return func(m *WireGuardManager) {
		for _, a := range addrs {
			if a = strings.TrimSpace(a); a != "" {
				m.addrs = append(m.addrs, a)
			}
		}
	}
}

// NewWireGuardManager constructs a manager bound to the given interface name
// (use DefaultInterfaceName for production) and opens the WireGuard control
// socket. The caller owns the returned manager and must call Close when done.
func NewWireGuardManager(ifaceName string, log logr.Logger, opts ...Option) (*WireGuardManager, error) {
	if ifaceName == "" {
		ifaceName = DefaultInterfaceName
	}

	wgClient, err := wgctrl.New()
	if err != nil {
		return nil, fmt.Errorf("wg: opening wgctrl client (is the wireguard kernel module loaded?): %w", err)
	}

	m := &WireGuardManager{
		ifaceName:  ifaceName,
		listenPort: DefaultListenPort,
		keepalive:  persistentKeepalive,
		wg:         wgClient,
		peerRoutes: make(map[string][]string),
		log:        log.WithName("wireguard").WithValues("iface", ifaceName),
	}
	for _, o := range opts {
		o(m)
	}
	return m, nil
}

// Close releases the WireGuard control socket. It does not tear down the link
// or peers; those persist in the kernel so traffic survives an agent restart.
func (m *WireGuardManager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.wg == nil {
		return nil
	}
	err := m.wg.Close()
	m.wg = nil
	return err
}

// SyncInterface ensures the `dwx-mesh0` device exists, is loaded with the
// supplied private key, listens on the configured UDP port, and is
// administratively up. It is idempotent and safe to call on every resync.
//
// Sequence (all over Netlink):
//  1. rtnetlink: look up the link; if absent, create a link of type
//     "wireguard" (RTM_NEWLINK with IFLA_INFO_KIND="wireguard").
//  2. genetlink/wireguard: load PrivateKey + ListenPort into the device's
//     cryptokey routing table (WG_CMD_SET_DEVICE). ReplacePeers is left false
//     so an in-place key rotation does not flush already-programmed peers.
//  3. rtnetlink: set the link administratively UP (IFF_UP) if it is not
//     already, so the kernel begins servicing the device.
func (m *WireGuardManager) SyncInterface(privateKey string) error {
	key, err := wgtypes.ParseKey(privateKey)
	if err != nil {
		return fmt.Errorf("wg: parsing node private key: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	link, err := m.ensureLinkLocked()
	if err != nil {
		return err
	}

	// Apply a configured device MTU before bringing the link up so routes through
	// it advertise the right MTU from the start.
	if m.mtu > 0 && link.Attrs().MTU != m.mtu {
		if err := netlink.LinkSetMTU(link, m.mtu); err != nil {
			return fmt.Errorf("wg: setting link %q MTU to %d: %w", m.ifaceName, m.mtu, err)
		}
	}

	port := m.listenPort
	cfg := wgtypes.Config{
		PrivateKey: &key,
		ListenPort: &port,
		// Do NOT replace peers here: SyncInterface may run after peers are
		// already configured (e.g. on private-key rotation or periodic resync),
		// and flushing them would blackhole live traffic.
		ReplacePeers: false,
	}
	if err := m.wg.ConfigureDevice(m.ifaceName, cfg); err != nil {
		return fmt.Errorf("wg: configuring device %q (key/port): %w", m.ifaceName, err)
	}

	// Bring the link up if it is not already. Checking the flag first avoids an
	// unnecessary netlink write on the steady-state path.
	if link.Attrs().Flags&net.FlagUp == 0 {
		if err := netlink.LinkSetUp(link); err != nil {
			return fmt.Errorf("wg: setting link %q up: %w", m.ifaceName, err)
		}
	}

	// Assign any configured local address(es). The mesh link is otherwise
	// address-less; a mesh-routable source is what lets node/gateway-originated
	// traffic (e.g. a masqueraded remote client) egress to remote clusters.
	// AddrReplace is idempotent, so a resync that re-applies the same address is a
	// no-op rather than an EEXIST error.
	for _, a := range m.addrs {
		addr, perr := parseAddr(a)
		if perr != nil {
			return fmt.Errorf("wg: parsing device address %q: %w", a, perr)
		}
		if err := netlink.AddrReplace(link, addr); err != nil {
			return fmt.Errorf("wg: assigning address %s to %q: %w", a, m.ifaceName, err)
		}
	}

	m.log.Info("interface synchronized", "listenPort", port, "addrs", m.addrs)
	return nil
}

// parseAddr converts a CIDR-form address string into a netlink.Addr. It is the
// pure half of address assignment, unit-testable without root or a netlink
// socket.
func parseAddr(cidr string) (*netlink.Addr, error) {
	addr, err := netlink.ParseAddr(cidr)
	if err != nil {
		return nil, fmt.Errorf("invalid address %q (want CIDR form, e.g. 10.244.255.254/32): %w", cidr, err)
	}
	return addr, nil
}

// ConfigurePeer programs (or updates) a single remote peer and installs host
// routes for every allowed CIDR so that traffic destined for the remote
// cluster is channeled into the `dwx-mesh0` device.
//
// Steps:
//  1. Parse the peer public key and the allowed CIDRs into typed values,
//     rejecting malformed input before we touch the kernel.
//  2. Resolve the endpoint (host:port) to a UDP address when provided. A blank
//     endpoint marks a roaming peer whose address is learned from inbound
//     handshakes.
//  3. genetlink/wireguard: upsert the peer with ReplaceAllowedIPs=true so the
//     device's allowed-IP set converges exactly to the desired CIDRs.
//  4. rtnetlink: install a scope-link route per CIDR pointing at the device,
//     then record those CIDRs so RemovePeer can withdraw precisely them.
//
// The whole operation is performed under the write lock so a concurrent
// RemovePeer for the same key cannot interleave.
func (m *WireGuardManager) ConfigurePeer(peerKey, endpoint string, allowedIPs []string) error {
	pubKey, err := wgtypes.ParseKey(peerKey)
	if err != nil {
		return fmt.Errorf("wg: parsing peer public key: %w", err)
	}

	nets, err := parseCIDRs(allowedIPs)
	if err != nil {
		return fmt.Errorf("wg: peer %s: %w", shortKey(peerKey), err)
	}

	var udpEndpoint *net.UDPAddr
	if endpoint != "" {
		udpEndpoint, err = net.ResolveUDPAddr("udp", endpoint)
		if err != nil {
			return fmt.Errorf("wg: resolving peer endpoint %q: %w", endpoint, err)
		}
	}

	keepalive := m.keepalive

	m.mu.Lock()
	defer m.mu.Unlock()

	link, err := m.ensureLinkLocked()
	if err != nil {
		return err
	}

	peerCfg := wgtypes.PeerConfig{
		PublicKey: pubKey,
		Endpoint:  udpEndpoint,
		// Converge the allowed-IP set to exactly what the spec declares; this
		// makes ConfigurePeer idempotent even when CIDRs are added or removed.
		ReplaceAllowedIPs:           true,
		AllowedIPs:                  nets,
		PersistentKeepaliveInterval: &keepalive,
	}
	if err := m.wg.ConfigureDevice(m.ifaceName, wgtypes.Config{
		ReplacePeers: false,
		Peers:        []wgtypes.PeerConfig{peerCfg},
	}); err != nil {
		return fmt.Errorf("wg: programming peer %s: %w", shortKey(peerKey), err)
	}

	// Install routes after the cryptokey routing is in place. If any route
	// fails we roll back the ones we already added for this call so we do not
	// leave a half-programmed peer behind.
	installed := make([]string, 0, len(nets))
	for i := range nets {
		dst := &nets[i]
		route := &netlink.Route{
			LinkIndex: link.Attrs().Index,
			Dst:       dst,
			Scope:     netlink.SCOPE_LINK,
		}
		// RouteReplace is idempotent: it adds the route or overwrites an
		// existing identical one without erroring on EEXIST.
		if err := netlink.RouteReplace(route); err != nil {
			m.rollbackRoutesLocked(link.Attrs().Index, installed)
			return fmt.Errorf("wg: installing route %s -> %s: %w", dst.String(), m.ifaceName, err)
		}
		installed = append(installed, dst.String())
	}

	// Withdraw any host route we previously installed for this peer that is no
	// longer desired - a CIDR was removed or its prefix changed. Skipping this
	// leaves a stale SCOPE_LINK route still funnelling that prefix into the mesh
	// device — a traffic hijack — and, once we overwrite the bookkeeping below,
	// RemovePeer could never reclaim it. ReplaceAllowedIPs converges the cryptokey
	// table the same way, but the host routes are ours to reconcile by hand.
	if stale := staleRoutes(m.peerRoutes[peerKey], installed); len(stale) > 0 {
		m.rollbackRoutesLocked(link.Attrs().Index, stale)
	}

	m.peerRoutes[peerKey] = installed
	// V(1): this fires on every reconcile (~every handshakeRefreshInterval per
	// peer), so it is debug-level to keep Info reserved for real state changes.
	m.log.V(1).Info("peer configured", "peer", shortKey(peerKey), "endpoint", endpoint, "routes", len(installed))
	return nil
}

// staleRoutes returns the CIDR strings in old that are absent from current. It
// is the pure diff ConfigurePeer uses to decide which previously-installed host
// routes to withdraw before overwriting a peer's route bookkeeping.
func staleRoutes(old, current []string) []string {
	if len(old) == 0 {
		return nil
	}
	keep := make(map[string]struct{}, len(current))
	for _, c := range current {
		keep[c] = struct{}{}
	}
	var stale []string
	for _, o := range old {
		if _, ok := keep[o]; !ok {
			stale = append(stale, o)
		}
	}
	return stale
}

// RemovePeer cleanly removes a peer's cryptokey association and withdraws every
// host route the manager installed for it. It is idempotent: removing an
// unknown peer is a no-op and never errors.
//
// Steps:
//  1. genetlink/wireguard: issue a peer config with Remove=true, dropping the
//     public key from the device.
//  2. rtnetlink: delete each route we previously recorded for the peer.
//  3. Forget the bookkeeping entry.
func (m *WireGuardManager) RemovePeer(peerKey string) error {
	pubKey, err := wgtypes.ParseKey(peerKey)
	if err != nil {
		return fmt.Errorf("wg: parsing peer public key for removal: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Drop the peer from the WireGuard device. We do this even if we have no
	// route bookkeeping for it, to stay convergent after an agent restart that
	// lost in-memory state.
	if err := m.wg.ConfigureDevice(m.ifaceName, wgtypes.Config{
		ReplacePeers: false,
		Peers: []wgtypes.PeerConfig{{
			PublicKey: pubKey,
			Remove:    true,
		}},
	}); err != nil {
		// If the device itself is gone there is nothing left to clean up.
		if isLinkNotFound(err) {
			delete(m.peerRoutes, peerKey)
			return nil
		}
		return fmt.Errorf("wg: removing peer %s from device: %w", shortKey(peerKey), err)
	}

	cidrs, known := m.peerRoutes[peerKey]
	if known {
		link, lerr := netlink.LinkByName(m.ifaceName)
		if lerr == nil {
			m.rollbackRoutesLocked(link.Attrs().Index, cidrs)
		} else if !isLinkNotFound(lerr) {
			return fmt.Errorf("wg: looking up link for route cleanup: %w", lerr)
		}
	}

	delete(m.peerRoutes, peerKey)
	m.log.Info("peer removed", "peer", shortKey(peerKey))
	return nil
}

// PeerHandshake returns the Unix-epoch seconds of the most recent successful
// handshake for the given peer, or 0 if none is known. It is read-only and
// uses the read lock so it can run concurrently with other readers. The
// reconciler uses this to populate MeshPeer.Status.LastHandshakeTime.
func (m *WireGuardManager) PeerHandshake(peerKey string) (int64, error) {
	pubKey, err := wgtypes.ParseKey(peerKey)
	if err != nil {
		return 0, fmt.Errorf("wg: parsing peer public key: %w", err)
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	dev, err := m.wg.Device(m.ifaceName)
	if err != nil {
		return 0, fmt.Errorf("wg: reading device %q: %w", m.ifaceName, err)
	}
	for i := range dev.Peers {
		if dev.Peers[i].PublicKey == pubKey {
			ts := dev.Peers[i].LastHandshakeTime
			if ts.IsZero() {
				return 0, nil
			}
			return ts.Unix(), nil
		}
	}
	return 0, nil
}

// ensureLinkLocked looks up the managed link and creates it as a WireGuard link
// if it does not yet exist. Callers MUST hold m.mu.
func (m *WireGuardManager) ensureLinkLocked() (netlink.Link, error) {
	link, err := netlink.LinkByName(m.ifaceName)
	if err == nil {
		return link, nil
	}
	if !isLinkNotFound(err) {
		return nil, fmt.Errorf("wg: looking up link %q: %w", m.ifaceName, err)
	}

	// Create a kernel WireGuard link: RTM_NEWLINK with IFLA_INFO_KIND set to
	// "wireguard". The netlink.Wireguard type encodes exactly that.
	wgLink := &netlink.Wireguard{
		LinkAttrs: netlink.LinkAttrs{Name: m.ifaceName},
	}
	if err := netlink.LinkAdd(wgLink); err != nil {
		return nil, fmt.Errorf("wg: creating wireguard link %q: %w", m.ifaceName, err)
	}

	link, err = netlink.LinkByName(m.ifaceName)
	if err != nil {
		return nil, fmt.Errorf("wg: re-reading freshly created link %q: %w", m.ifaceName, err)
	}
	m.log.Info("created wireguard link")
	return link, nil
}

// rollbackRoutesLocked deletes the supplied CIDR routes from the given link
// index, best-effort. Callers MUST hold m.mu. Errors are logged rather than
// returned because rollback runs on an already-failing path and we want to
// remove as much as possible.
func (m *WireGuardManager) rollbackRoutesLocked(linkIndex int, cidrs []string) {
	for _, c := range cidrs {
		_, ipnet, err := net.ParseCIDR(c)
		if err != nil {
			continue
		}
		route := &netlink.Route{LinkIndex: linkIndex, Dst: ipnet, Scope: netlink.SCOPE_LINK}
		if err := netlink.RouteDel(route); err != nil && !isNoSuchRoute(err) {
			m.log.Error(err, "failed to delete route during cleanup", "cidr", c)
		}
	}
}

// parseCIDRs converts a slice of CIDR strings into net.IPNet values, rejecting
// any malformed entry. An empty input yields an empty (non-nil) slice.
func parseCIDRs(cidrs []string) ([]net.IPNet, error) {
	out := make([]net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, ipnet, err := net.ParseCIDR(c)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q: %w", c, err)
		}
		out = append(out, *ipnet)
	}
	return out, nil
}

// isLinkNotFound reports whether err signals an absent netlink link.
func isLinkNotFound(err error) bool {
	if err == nil {
		return false
	}
	_, ok := err.(netlink.LinkNotFoundError)
	return ok
}

// isNoSuchRoute reports whether err is the "no such process/route" (ESRCH)
// signal returned when deleting an already-absent route.
func isNoSuchRoute(err error) bool {
	return err != nil && err.Error() == "no such process"
}

// shortKey returns a truncated, log-safe rendering of a WireGuard key so full
// public keys do not litter logs.
func shortKey(k string) string {
	if len(k) <= 8 {
		return k
	}
	return k[:8] + "…"
}
