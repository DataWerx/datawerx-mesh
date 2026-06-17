// Package routed is the "bring your own overlay" data plane: instead of owning a
// WireGuard device, the agent assumes node-to-node L3 connectivity is already
// provided by an existing overlay (Tailscale, NetBird, Cilium, plain WireGuard,
// a cloud VPN — anything).  It only programs the *Kubernetes multi-cluster layer
// on top, and the host routes that steer remote pod/service CIDRs at the overlay,
// while the MCS service discovery, ClusterSetIP NAT, overlap remap, and
// MeshNetworkPolicy data planes work unchanged.
//
// DataWerx becomes the k8s multi-cluster service layer that rides whatever
// encrypted transport you already run, rather than a competing VPN. The
// encryption / NAT-traversal / relay burden stays with the overlay you chose;
// DataWerx adds service discovery, VIPs, overlap handling, and policy.
//
// As elsewhere, the routing decision is a pure, unit-tested function. The
// kernel side effects (manager.go) need root + netlink and are covered
// by a dataplane-tagged test.
package routed

import (
	"fmt"
	"net"
	"sort"
)

// Route is one host route to install: a destination CIDR steered via an overlay
// next-hop address. It is comparable, which makes the planner table-testable.
type Route struct {
	Dest string // remote pod/service CIDR or remapped virtual CIDR
	Via  string // overlay next-hop IP - the remote gateway's overlay address
}

// PlanRoutes computes the deterministic, de-duplicated, sorted set of routes
// that steer allowedIPs at the overlay next-hop. via must be a bare IP (the
// remote gateway's overlay address); allowedIPs are CIDRs. Invalid input is an
// error rather than a silently dropped route, so a misconfigured MeshPeer
// surfaces on status instead of black-holing traffic.
func PlanRoutes(allowedIPs []string, via string) ([]Route, error) {
	viaIP := net.ParseIP(via)
	if viaIP == nil {
		return nil, fmt.Errorf("overlay next-hop %q is not a valid IP address", via)
	}

	seen := map[string]struct{}{}
	out := make([]Route, 0, len(allowedIPs))
	for _, c := range allowedIPs {
		if c == "" {
			continue
		}
		_, ipnet, err := net.ParseCIDR(c)
		if err != nil {
			return nil, fmt.Errorf("invalid routed CIDR %q: %w", c, err)
		}
		// Family must match the next-hop: you cannot route a v6 prefix via a v4
		// gateway. This catches a common dual-stack misconfiguration early.
		if (ipnet.IP.To4() != nil) != (viaIP.To4() != nil) {
			return nil, fmt.Errorf("address family mismatch: CIDR %s via %s", ipnet.String(), via)
		}
		dst := ipnet.String()
		if _, dup := seen[dst]; dup {
			continue
		}
		seen[dst] = struct{}{}
		out = append(out, Route{Dest: dst, Via: viaIP.String()})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Dest < out[j].Dest })
	return out, nil
}

// hostOnly strips a ":port" suffix from an endpoint, returning the bare host.
// In WireGuard mode an endpoint is "host:port"; in routed mode it should be a
// bare overlay IP, but we tolerate a port so the same MeshPeer.Spec.Endpoint
// field works in both modes.
func hostOnly(endpoint string) string {
	if host, _, err := net.SplitHostPort(endpoint); err == nil {
		return host
	}
	return endpoint
}
