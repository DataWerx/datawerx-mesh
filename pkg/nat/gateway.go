package nat

import "sort"

// GatewayMasqChain holds the remote-access gateway's masquerade rules, hooked
// from POSTROUTING.
//
// This exists because a DataWerx access gateway lets a remote client (a laptop on a
// shared overlay — Tailscale, a corporate VPN, etc.) reach in-cluster
// ClusterSetIP VIPs and cross-cluster pod/service ranges "like a VPN". The
// client routes those destination CIDRs at the gateway's overlay address; the
// gateway forwards them into the mesh data plane (where the ClusterSetIP DNAT in
// DWX-CLUSTERSET rewrites a VIP to a real backend). For the reply to find its
// way back, the gateway must present itself as the source of that forwarded
// traffic — otherwise the backend would answer the client's overlay address,
// which the backend's cluster has no route for, and the connection black-holes.
//
// So we MASQUERADE traffic whose SOURCE is a remote-access client range and
// whose DESTINATION is a mesh-reachable range. Scoping to the client source
// range, not local pod CIDRs, is deliberate: it never matches the gateway node's
// own cross-cluster pod traffic, so it cannot pre-empt the source-preserving
// exemption in DWX-MESH-NOMASQ. The chain is hooked at the TOP of POSTROUTING so
// the MASQUERADE (a terminating target in the nat table) takes effect before any
// generic CNI/cloud masquerade.
const GatewayMasqChain = "DWX-GW-MASQ"

// BuildGatewayMasqRules expands the client source, mesh destination CIDR sets
// into the deterministic, de-duplicated, sorted set of masquerade rules:
//
//	-s <client> -d <dest> -j MASQUERADE
//
// Only same-family, both IPv4 or both IPv6, pairs are emitted; mixed pairs are
// skipped, since a v4 client cannot be SNAT'd toward a v6 destination. The
// result is pure and comparable, so the planning is unit-testable and the
// manager only applies it.
func BuildGatewayMasqRules(clientCIDRs, destCIDRs []string) []Rule {
	clients := dedupeSorted(clientCIDRs)
	dests := dedupeSorted(destCIDRs)

	rules := make([]Rule, 0, len(clients)*len(dests))
	for _, c := range clients {
		for _, d := range dests {
			if isIPv6(c) != isIPv6(d) {
				continue // never SNAT across address families
			}
			rules = append(rules, Rule{
				Chain: GatewayMasqChain,
				Args:  []string{"-s", c, "-d", d, "-j", "MASQUERADE"},
			})
		}
	}
	// dedupeSorted already sorts both inputs; the nested loop therefore emits
	// rules in (client, dest) lexical order. A final sort guards that contract
	// even if the loop structure changes.
	sort.Slice(rules, func(i, j int) bool {
		if rules[i].Args[1] != rules[j].Args[1] {
			return rules[i].Args[1] < rules[j].Args[1]
		}
		return rules[i].Args[3] < rules[j].Args[3]
	})
	return rules
}
