// Package meshfw computes and programs DataWerx Mesh's cross-cluster network
// policy: an L3/L4 firewall on the WireGuard ingress path that restricts which
// remote clusters (or source CIDRs) may reach which local destinations.
//
// Kubernetes NetworkPolicy is namespace-scoped and only understands local pod
// selectors; it cannot express "only cluster-b may reach my database over the
// mesh". MeshNetworkPolicy fills that gap at the granularity the mesh actually
// operates on — cluster IDs and CIDRs — and the agent compiles it into iptables
// filter rules matched on traffic arriving via the mesh device.
//
// As everywhere in this repo, rule computation is a pure, exhaustively unit-tested
// function.  Applying rules to the kernel (manager.go) needs root and iptables,
// and is covered by data-plane tests.
//
// Semantics mirror Kubernetes NetworkPolicy: a destination "selected" by any
// policy is default-deny for mesh ingress, and is reachable only by the union
// of that destination's allow rules. Destinations selected by no policy are not
// touched (the chain returns and normal forwarding continues). Established and
// related flows are always allowed so locally-initiated connections still get
// their replies.
package meshfw

import (
	"net"
	"sort"
	"strconv"
	"strings"
)

const (
	// TableFilter is the iptables table the mesh firewall programs.
	TableFilter = "filter"
	// FWChain is our firewall chain, jumped from FORWARD and INPUT for packets
	// that arrive on the WireGuard device.
	FWChain = "DWX-MESH-FW"
	// FWChainGuard is a blanket-DROP chain mesh ingress is temporarily diverted to
	// while FWChain is rebuilt, so a default-deny firewall fails CLOSED, not OPEN
	// during the non-atomic flush+repopulate window.
	FWChainGuard = "DWX-MESH-FW-GUARD"
)

// Rule is one iptables rule: a chain plus its argument vector (excluding the
// table). Mirrors nat.Rule so the manager code reads the same way.
type Rule struct {
	Chain string
	Args  []string
}

// Ruleset is the fully-expanded, deterministic realization of a set of mesh
// network policies. It is comparable-by-value (via its slices) which is what
// makes the planner table-testable.
type Ruleset struct {
	// Rules is the ordered list to append to FWChain: the conntrack fast-path,
	// then the ACCEPT allow-rules, then the DROP rules for protected dests.
	Rules []Rule
	// Skipped lists inputs that were dropped from the plan because they are not
	// IPv4 (the filter data plane is IPv4 today; IPv6 lands with dual stack).
	// Surfaced so the controller can report it on status rather than silently
	// losing a rule.
	Skipped []string
}

// Port is an allowed L4 port. Protocol is tcp|udp|sctp (lowercased; empty means
// the rule is protocol/port-agnostic).
type Port struct {
	Protocol string
	Port     int32
}

// PeerSelector names allowed sources, either by mesh cluster ID (resolved to
// that cluster's CIDRs via the topology) or by explicit CIDR.
type PeerSelector struct {
	ClusterIDs []string
	CIDRs      []string
}

// IngressRule is one allow rule: traffic from any of From, to any of the
// owning policy's destinations, on any of Ports (empty Ports = all ports).
type IngressRule struct {
	From  []PeerSelector
	Ports []Port
}

// Policy is one MeshNetworkPolicy projected into pure inputs. Destinations are
// the local CIDRs it protects; empty Destinations means it protects ALL mesh
// ingress (a default-deny posture for this cluster).
type Policy struct {
	Name         string
	Destinations []string
	Ingress      []IngressRule
}

// BuildFirewall compiles the policy set into a deterministic FWChain ruleset.
// clusterCIDRs maps a remote cluster ID to its advertised CIDRs, used to
// resolve ClusterID selectors; unknown cluster IDs contribute no sources (the
// peer may simply not be known yet). The result is independent of input order.
func BuildFirewall(policies []Policy, clusterCIDRs map[string][]string) Ruleset {
	b := &fwBuilder{
		clusterCIDRs:  clusterCIDRs,
		accepts:       map[acceptKey]struct{}{},
		protectedDest: map[string]struct{}{},
	}
	for _, p := range policies {
		b.addPolicy(p)
	}
	return b.ruleset()
}

// acceptKey identifies one ACCEPT rule. "" is the sentinel for "any" in src/dst.
type acceptKey struct{ src, dst, proto, port string }

// fwBuilder accumulates the deduplicated accept/protected sets while folding in
// each policy, then emits the deterministic ruleset.
type fwBuilder struct {
	clusterCIDRs  map[string][]string
	accepts       map[acceptKey]struct{}
	protectedDest map[string]struct{} // dest CIDR ("" = any) under default-deny
	skipped       []string
}

// keepIPv4 filters list to IPv4 CIDRs (ignoring ""), recording every non-IPv4
// entry in skipped tagged with kind so it can be surfaced on status.
func (b *fwBuilder) keepIPv4(list []string, kind string) []string {
	var out []string
	for _, c := range list {
		if c == "" {
			continue
		}
		if !isIPv4CIDR(c) {
			b.skipped = append(b.skipped, kind+":"+c)
			continue
		}
		out = append(out, c)
	}
	return out
}

// addPolicy folds one policy's destinations and ingress allow-rules into the
// accumulating accept/protected sets.
func (b *fwBuilder) addPolicy(p Policy) {
	dests := b.keepIPv4(p.Destinations, "dest")
	// A policy with no (valid) destinations protects everything: sentinel "".
	if len(p.Destinations) == 0 {
		dests = []string{""}
	} else if len(dests) == 0 {
		// All destinations were non-IPv4 and skipped; nothing to program.
		return
	}

	for _, d := range dests {
		b.protectedDest[d] = struct{}{}
	}
	for _, ing := range p.Ingress {
		b.addIngress(ing, dests)
	}
}

// addIngress records the accept keys for one ingress rule across the policy's
// destinations.
func (b *fwBuilder) addIngress(ing IngressRule, dests []string) {
	srcs := b.keepIPv4(resolveSources(ing.From, b.clusterCIDRs), "from")
	for _, d := range dests {
		for _, s := range srcs {
			b.addAccept(s, d, ing.Ports)
		}
	}
}

// addAccept records the accept key(s) for one (src, dst) pair: a single
// port-agnostic key when ports is empty, otherwise one key per port.
func (b *fwBuilder) addAccept(src, dst string, ports []Port) {
	if len(ports) == 0 {
		b.accepts[acceptKey{src: src, dst: dst}] = struct{}{}
		return
	}
	for _, pt := range ports {
		b.accepts[acceptKey{src: src, dst: dst, proto: normalizeProto(pt.Protocol), port: portStr(pt.Port)}] = struct{}{}
	}
}

// ruleset emits the deterministic FWChain ruleset: conntrack fast-path, then the
// sorted ACCEPT rules, then the sorted DROP rules for protected destinations.
func (b *fwBuilder) ruleset() Ruleset {
	var rs Ruleset

	// 1. conntrack fast-path: always let replies/established flows through.
	rs.Rules = append(rs.Rules, Rule{Chain: FWChain, Args: []string{
		"-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT",
	}})

	// 2. ACCEPT rules, sorted for determinism.
	keys := make([]acceptKey, 0, len(b.accepts))
	for k := range b.accepts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return lessAccept(keys[i], keys[j]) })
	for _, k := range keys {
		rs.Rules = append(rs.Rules, Rule{Chain: FWChain, Args: acceptArgs(k.src, k.dst, k.proto, k.port)})
	}

	// 3. DROP rules for every protected destination, sorted. "" (protect-all)
	//    sorts last so it acts as the final catch-all.
	dests := make([]string, 0, len(b.protectedDest))
	for d := range b.protectedDest {
		dests = append(dests, d)
	}
	sort.Slice(dests, func(i, j int) bool { return lessDest(dests[i], dests[j]) })
	for _, d := range dests {
		rs.Rules = append(rs.Rules, Rule{Chain: FWChain, Args: dropArgs(d)})
	}

	sort.Strings(b.skipped)
	rs.Skipped = dedupe(b.skipped)
	return rs
}

func acceptArgs(src, dst, proto, port string) []string {
	args := []string{}
	if src != "" {
		args = append(args, "-s", src)
	}
	if dst != "" {
		args = append(args, "-d", dst)
	}
	if proto != "" {
		args = append(args, "-p", proto)
		if port != "" {
			args = append(args, "-m", proto, "--dport", port)
		}
	}
	return append(args, "-j", "ACCEPT")
}

func dropArgs(dst string) []string {
	if dst == "" {
		return []string{"-j", "DROP"}
	}
	return []string{"-d", dst, "-j", "DROP"}
}

// resolveSources expands selectors into concrete source CIDRs (cluster IDs via
// the topology map plus explicit CIDRs).
func resolveSources(from []PeerSelector, clusterCIDRs map[string][]string) []string {
	var out []string
	for _, sel := range from {
		for _, id := range sel.ClusterIDs {
			out = append(out, clusterCIDRs[id]...)
		}
		out = append(out, sel.CIDRs...)
	}
	return out
}

func lessAccept(a, b acceptKey) bool {
	if a.dst != b.dst {
		return a.dst < b.dst
	}
	if a.src != b.src {
		return a.src < b.src
	}
	if a.proto != b.proto {
		return a.proto < b.proto
	}
	return a.port < b.port
}

// lessDest orders protected destinations, sorting the "" (protect-all) sentinel
// last so its DROP acts as the final catch-all.
func lessDest(a, b string) bool {
	if a == "" {
		return false
	}
	if b == "" {
		return true
	}
	return a < b
}

func isIPv4CIDR(c string) bool {
	ip, _, err := net.ParseCIDR(c)
	return err == nil && ip.To4() != nil
}

func normalizeProto(p string) string {
	p = strings.ToLower(strings.TrimSpace(p))
	if p == "" {
		return "tcp"
	}
	return p
}

func portStr(p int32) string { return strconv.Itoa(int(p)) }

func dedupe(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := in[:1]
	for _, s := range in[1:] {
		if s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}
