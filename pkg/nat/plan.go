// Package nat computes and programs the ClusterSetIP DNAT/load-balancing data
// plane: traffic sent to a ServiceImport's virtual ClusterSetIP is rewritten to
// one of the exporting clusters' reachable service IPs (the backends) and
// load-balanced across them, so a single stable VIP fans out across the mesh.
//
// The rule computation is pure and exhaustively unit-tested — it
// is the part that must never be wrong. Applying the rules to the kernel
// (manager.go) needs root and iptables and is covered by integration tests.
//
// The model mirrors kube-proxy's iptables mode:
//
//	PREROUTING/OUTPUT ─► DWX-CLUSTERSET ──(-d VIP -p proto --dport P)──► DWX-SVC-<h>
//	                                          DWX-SVC-<h> ──(statistic LB)──► DWX-SEP-<h>
//	                                          DWX-SEP-<h> ──(DNAT)──► backend:P
package nat

import (
	"crypto/sha256"
	"encoding/base32"
	"sort"
	"strconv"
	"strings"
)

const (
	// TableNAT is the iptables table we program.
	TableNAT = "nat"
	// RootChain is our entrypoint chain, jumped from PREROUTING and OUTPUT.
	RootChain = "DWX-CLUSTERSET"

	svcChainPrefix = "DWX-SVC-"
	sepChainPrefix = "DWX-SEP-"
)

// PortDNAT is one port of a ClusterSetIP service.
type PortDNAT struct {
	Protocol string // tcp|udp|sctp, lowercased; empty defaults to tcp
	Port     int32
}

// ServiceDNAT is the desired DNAT/LB for one imported ClusterSetIP service.
type ServiceDNAT struct {
	Namespace string
	Name      string
	VIP       string // the allocated ClusterSetIP (IPv4)
	Ports     []PortDNAT
	Backends  []string // exporting clusters' reachable service IPs
}

// Rule is a single iptables rule: the chain it lives in and its argument list
// (excluding the table), in canonical order.
type Rule struct {
	Chain string
	Args  []string
}

// Ruleset is the fully-expanded, deterministic realization of a list of
// ServiceDNATs. It is comparable, which is what makes the planner testable.
type Ruleset struct {
	// Chains is the sorted, de-duplicated list of custom chains to create
	// excluding RootChain, which the manager ensures separately.
	Chains []string
	// Rules is the ordered list of rules to append, grouped per (service,port):
	// the root jump, then that port's load-balancing rules, then its DNAT rules.
	Rules []Rule
}

// BuildRuleset deterministically expands services into the chains and rules
// that realize ClusterSetIP DNAT + load-balancing. Services with no VIP, no
// ports, or no backends are skipped (nothing to program). Inputs may be in any
// order; the output is fully deterministic.
func BuildRuleset(services []ServiceDNAT) Ruleset {
	rs := Ruleset{}
	chainSet := map[string]struct{}{}

	svcs := append([]ServiceDNAT(nil), services...)
	sort.Slice(svcs, func(i, j int) bool {
		if svcs[i].Namespace != svcs[j].Namespace {
			return svcs[i].Namespace < svcs[j].Namespace
		}
		return svcs[i].Name < svcs[j].Name
	})

	for _, s := range svcs {
		if s.VIP == "" || len(s.Ports) == 0 || len(s.Backends) == 0 {
			continue
		}
		backends := uniqueSortedStrings(s.Backends)
		for _, p := range sortedPorts(s.Ports) {
			appendPortRules(&rs, chainSet, s, p, backends)
		}
	}

	rs.Chains = sortedKeys(chainSet)
	return rs
}

// appendPortRules emits the root jump, load-balancing, and DNAT rules for one
// (service, port) pair, registering every custom chain it references in
// chainSet. Backends must already be unique and sorted.
func appendPortRules(rs *Ruleset, chainSet map[string]struct{}, s ServiceDNAT, p PortDNAT, backends []string) {
	proto := normalizeProto(p.Protocol)
	portStr := strconv.Itoa(int(p.Port))
	svcChain := hashChain(svcChainPrefix, s.Namespace, s.Name, proto, portStr)

	// Root jump: VIP:port -> service chain.
	rs.Rules = append(rs.Rules, Rule{
		Chain: RootChain,
		Args:  []string{"-d", hostCIDR(s.VIP), "-p", proto, "-m", proto, "--dport", portStr, "-j", svcChain},
	})
	chainSet[svcChain] = struct{}{}

	n := len(backends)
	sepChains := make([]string, n)
	for i, b := range backends {
		sepChains[i] = hashChain(sepChainPrefix, s.Namespace, s.Name, proto, portStr, b)
		chainSet[sepChains[i]] = struct{}{}
	}

	// Service chain: statistic-based load-balance across SEP chains.
	for i := range backends {
		if i < n-1 {
			// Probability 1/(n-i) gives a uniform split across all backends.
			prob := strconv.FormatFloat(1.0/float64(n-i), 'f', 10, 64)
			rs.Rules = append(rs.Rules, Rule{
				Chain: svcChain,
				Args:  []string{"-m", "statistic", "--mode", "random", "--probability", prob, "-j", sepChains[i]},
			})
		} else {
			rs.Rules = append(rs.Rules, Rule{Chain: svcChain, Args: []string{"-j", sepChains[i]}})
		}
	}

	// SEP chains: DNAT to the backend, preserving the port.
	for i, b := range backends {
		rs.Rules = append(rs.Rules, Rule{
			Chain: sepChains[i],
			Args:  []string{"-p", proto, "-j", "DNAT", "--to-destination", dnatTarget(b, portStr)},
		})
	}
}

// isIPv6 reports whether an IP literal is IPv6 (contains a colon).
func isIPv6(ip string) bool { return strings.Contains(ip, ":") }

// hostCIDR renders a single-host match for the address family: /32 for IPv4,
// /128 for IPv6.
func hostCIDR(ip string) string {
	if isIPv6(ip) {
		return ip + "/128"
	}
	return ip + "/32"
}

// dnatTarget renders an iptables --to-destination value, bracketing IPv6
// addresses so the port is parsed correctly (e.g. [2001:db8::1]:80).
func dnatTarget(ip, port string) string {
	if isIPv6(ip) {
		return "[" + ip + "]:" + port
	}
	return ip + ":" + port
}

// normalizeProto lowercases the protocol and defaults empty to tcp.
func normalizeProto(p string) string {
	p = strings.ToLower(strings.TrimSpace(p))
	if p == "" {
		return "tcp"
	}
	return p
}

// hashChain builds a stable, length-safe iptables chain name from its parts.
func hashChain(prefix string, parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:])
	return prefix + enc[:16]
}

func uniqueSortedStrings(in []string) []string {
	seen := map[string]struct{}{}
	for _, s := range in {
		if s != "" {
			seen[s] = struct{}{}
		}
	}
	return sortedKeys(seen)
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sortedPorts de-duplicates ports by (port, protocol) and sorts them.
func sortedPorts(in []PortDNAT) []PortDNAT {
	seen := map[string]PortDNAT{}
	for _, p := range in {
		key := strconv.Itoa(int(p.Port)) + "/" + normalizeProto(p.Protocol)
		if _, ok := seen[key]; !ok {
			seen[key] = PortDNAT{Protocol: normalizeProto(p.Protocol), Port: p.Port}
		}
	}
	out := make([]PortDNAT, 0, len(seen))
	for _, p := range seen {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Port != out[j].Port {
			return out[i].Port < out[j].Port
		}
		return out[i].Protocol < out[j].Protocol
	})
	return out
}
