package nat

import "sort"

// NoMasqChain holds the masquerade-exemption rules, hooked from POSTROUTING.
//
// DataWerx routes a remote cluster's pod/service CIDRs over the mesh device,
// but the node's own SNAT/masquerade rules (kind-masq-agent here, the CNI's
// or a cloud NAT in production) masquerade any traffic masquerade whose
// destination is "non-local". That rewrites the source of cross-cluster pod
// traffic as it egresses the mesh device — and since a WireGuard device has
// no address, the masqueraded source falls outside the peer's AllowedIPs and the
// far end drops the packet (cryptokey-routing rejection). Exempting local→remote
// mesh traffic from masquerade preserves the real pod source end to end.
//
// The chain is hooked at the TOP of POSTROUTING so its ACCEPT (a terminating
// target in the nat table) short-circuits the later masquerade rules. It is
// scoped to the directly-routed (non-overlapping) remote CIDRs, so in overlap
// mode — where the source is instead presented via the NETMAP in RemapPostChain
// — it contributes no rules and cannot pre-empt that translation.
const NoMasqChain = "DWX-MESH-NOMASQ"

// BuildNoMasqRules expands the (local source, remote destination) CIDR sets into
// the deterministic, de-duplicated, sorted set of masquerade-exemption rules:
//
//	-s <local> -d <remote> -j ACCEPT
//
// Only same-family (both IPv4 or both IPv6) pairs are emitted; mixed pairs are
// skipped. The result is pure and comparable, so the planning is unit-testable
// and the manager only applies it.
func BuildNoMasqRules(local, remote []string) []Rule {
	locals := dedupeSorted(local)
	remotes := dedupeSorted(remote)

	rules := make([]Rule, 0, len(locals)*len(remotes))
	for _, l := range locals {
		for _, r := range remotes {
			if isIPv6(l) != isIPv6(r) {
				continue // never match across address families
			}
			rules = append(rules, Rule{
				Chain: NoMasqChain,
				Args:  []string{"-s", l, "-d", r, "-j", "ACCEPT"},
			})
		}
	}
	return rules
}

// dedupeSorted returns the non-empty inputs, de-duplicated and sorted, so rule
// generation is deterministic regardless of input order.
func dedupeSorted(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
