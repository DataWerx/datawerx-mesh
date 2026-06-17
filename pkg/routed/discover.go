package routed

import (
	"fmt"
	"net"

	"github.com/vishvananda/netlink"
)

// SelectOverlayIP picks the address a remote cluster should use as the next-hop
// to reach this node over the overlay: the first global-unicast address on the
// overlay interface, preferring the requested family but falling back to the
// other. It is pure and operates on a supplied address list, so it is unit-testable
// without touching the kernel.
//
// "Global unicast" excludes loopback, link-local, and multicast — i.e. exactly
// the routable overlay address an operator would put in a peer's spec.endpoint.
func SelectOverlayIP(addrs []net.IPNet, preferV6 bool) (string, error) {
	var fallback string
	for _, a := range addrs {
		ip := a.IP
		if ip == nil || !ip.IsGlobalUnicast() || ip.IsLinkLocalUnicast() {
			continue
		}
		isV6 := ip.To4() == nil
		if isV6 == preferV6 {
			return ip.String(), nil
		}
		if fallback == "" {
			fallback = ip.String()
		}
	}
	if fallback != "" {
		return fallback, nil
	}
	return "", fmt.Errorf("no global-unicast address found among %d interface addresses", len(addrs))
}

// DiscoverOverlayIP returns this node's overlay address on iface — the value a
// remote cluster should set as spec.endpoint to reach this cluster in routed
// mode. It is best-effort: the agent logs it at startup so operators don't have
// to look it up by hand, but it is not required for the data plane to function.
func DiscoverOverlayIP(iface string) (string, error) {
	link, err := netlink.LinkByName(iface)
	if err != nil {
		return "", fmt.Errorf("routed: overlay interface %q: %w", iface, err)
	}
	addrs, err := netlink.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		return "", fmt.Errorf("routed: listing %q addresses: %w", iface, err)
	}
	nets := make([]net.IPNet, 0, len(addrs))
	for _, a := range addrs {
		if a.IPNet != nil {
			nets = append(nets, *a.IPNet)
		}
	}
	return SelectOverlayIP(nets, false)
}
