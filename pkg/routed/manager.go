package routed

import (
	"fmt"
	"net"
	"sync"

	"github.com/go-logr/logr"
	"github.com/vishvananda/netlink"
)

// routeOps is the seam over the kernel routing table. The production
// implementation uses netlink. Tests inject an in-memory fake so the Manager's
// per-peer bookkeeping is unit-testable without
// root.
type routeOps interface {
	Replace(r Route) error
	Delete(r Route) error
}

// Manager programs host routes that steer remote CIDRs at an existing overlay,
// implementing controllers.PeerDataPlane without owning any WireGuard device.
// It tracks the routes installed per peer so RemovePeer withdraws exactly what
// ConfigurePeer added. All kernel mutations are serialized behind a mutex, like
// the WireGuard manager.
type Manager struct {
	mu         sync.Mutex
	ops        routeOps
	peerRoutes map[string][]Route
	log        logr.Logger
}

// NewManager builds a routed data plane. overlayIface is the existing overlay
// device (e.g. "tailscale0", "wt0", "wg0") used as the route's output device;
// empty lets the kernel resolve the output device from the next-hop, which works
// when the overlay installs a connected route for its address range.
func NewManager(overlayIface string, log logr.Logger) (*Manager, error) {
	ops, err := newNetlinkOps(overlayIface)
	if err != nil {
		return nil, err
	}
	return &Manager{
		ops:        ops,
		peerRoutes: map[string][]Route{},
		log:        log.WithName("routed"),
	}, nil
}

// ConfigurePeer installs or reconciles the routes that steer allowedIPs at the
// peer's overlay next-hop carried in endpoint. It converges to exactly the
// declared set: routes no longer desired for this peer are withdrawn, making it
// idempotent across CIDR additions and removals.
func (m *Manager) ConfigurePeer(peerKey, endpoint string, allowedIPs []string) error {
	via := hostOnly(endpoint)
	desired, err := PlanRoutes(allowedIPs, via)
	if err != nil {
		return fmt.Errorf("routed: peer %s: %w", peerKey, err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	installed := make([]Route, 0, len(desired))
	for _, r := range desired {
		if err := m.ops.Replace(r); err != nil {
			// Roll back this call's additions so we never leave a half-programmed
			// peer behind.
			m.deleteRoutes(installed)
			return fmt.Errorf("routed: installing route %s via %s: %w", r.Dest, r.Via, err)
		}
		installed = append(installed, r)
	}

	// Withdraw any previously-installed route for this peer that is no longer
	// desired (e.g. a CIDR was removed, or the next-hop changed).
	for _, old := range m.peerRoutes[peerKey] {
		if !containsRoute(desired, old) {
			if err := m.ops.Delete(old); err != nil {
				m.log.Error(err, "withdrawing stale route", "dest", old.Dest, "via", old.Via)
			}
		}
	}

	m.peerRoutes[peerKey] = installed
	// Fires on every reconcile; debug-level so Info stays a feed of real
	// state changes.
	m.log.V(1).Info("peer routes programmed", "peer", peerKey, "via", via, "routes", len(installed))
	return nil
}

// RemovePeer withdraws every route installed for peerKey. It is idempotent:
// removing an unknown peer is a no-op.
func (m *Manager) RemovePeer(peerKey string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.deleteRoutes(m.peerRoutes[peerKey])
	delete(m.peerRoutes, peerKey)
	return nil
}

// PeerHandshake has no meaning in routed mode - the overlay owns the transport
// and liveness. Returning 0 reports "no handshake info"; the reconciler treats
// a successful ConfigurePeer as Connected.
func (m *Manager) PeerHandshake(peerKey string) (int64, error) { return 0, nil }

// Close releases nothing the routed manager owns (the overlay device is not
// ours), so it is a no-op present for symmetry with the WireGuard manager.
func (m *Manager) Close() error { return nil }

// deleteRoutes best-effort withdraws a set of routes (caller holds the lock).
func (m *Manager) deleteRoutes(routes []Route) {
	for _, r := range routes {
		if err := m.ops.Delete(r); err != nil {
			m.log.Error(err, "deleting route", "dest", r.Dest, "via", r.Via)
		}
	}
}

func containsRoute(rs []Route, want Route) bool {
	for _, r := range rs {
		if r == want {
			return true
		}
	}
	return false
}

// netlinkOps is the production routeOps backed by the kernel routing table.
type netlinkOps struct {
	linkIndex int // 0 = let the kernel resolve the output device from the next-hop
}

func newNetlinkOps(overlayIface string) (*netlinkOps, error) {
	if overlayIface == "" {
		return &netlinkOps{}, nil
	}
	link, err := netlink.LinkByName(overlayIface)
	if err != nil {
		return nil, fmt.Errorf("routed: overlay interface %q not found: %w", overlayIface, err)
	}
	return &netlinkOps{linkIndex: link.Attrs().Index}, nil
}

func (o *netlinkOps) route(r Route) (*netlink.Route, error) {
	_, dst, err := net.ParseCIDR(r.Dest)
	if err != nil {
		return nil, err
	}
	nr := &netlink.Route{Dst: dst, Gw: net.ParseIP(r.Via)}
	if o.linkIndex != 0 {
		// Pin the output device and mark the gateway on-link, so the route works
		// even when the overlay assigns per-peer /32s without a connected subnet.
		nr.LinkIndex = o.linkIndex
		nr.Flags = int(netlink.FLAG_ONLINK)
	}
	return nr, nil
}

func (o *netlinkOps) Replace(r Route) error {
	nr, err := o.route(r)
	if err != nil {
		return err
	}
	return netlink.RouteReplace(nr) // idempotent: add-or-overwrite
}

func (o *netlinkOps) Delete(r Route) error {
	nr, err := o.route(r)
	if err != nil {
		return err
	}
	if err := netlink.RouteDel(nr); err != nil && !isNoSuchRoute(err) {
		return err
	}
	return nil
}

// isNoSuchRoute reports whether err is the kernel's "no such process/route"
// signal returned when deleting an already-absent route, which makes Delete
// idempotent. Mirrors pkg/wg.isNoSuchRoute.
func isNoSuchRoute(err error) bool {
	return err != nil && err.Error() == "no such process"
}
