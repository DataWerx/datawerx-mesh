// Package mtu keeps cross-cluster TCP healthy across the mesh's reduced path MTU.
//
// The WireGuard mesh device has a smaller MTU than the pods' 1500-byte cluster
// interfaces because of the encapsulation overhead. When a pod opens a TCP
// connection to a remote cluster and Path MTU Discovery is broken, which it
// very often is in Kubernetes, where the ICMP "fragmentation needed" replies are
// dropped by intermediate firewalls/NAT — large segments are silently discarded
// and the connection hangs after the handshake. This is the single most common
// "it pings but nothing works" failure in encapsulated networks.
//
// The robust, universal fix is to clamp the TCP MSS of SYN packets leaving the
// mesh device to the path MTU, so both peers negotiate a segment size that fits.
// Each node clamps its own egress (-o <mesh-iface>), which covers both the SYN
// and the SYN-ACK since both endpoints run the agent.
//
// As elsewhere, the rule computation here is a pure, unit-tested function; the
// kernel side effects (manager.go) need root + iptables and are covered by a
// dataplane-tagged integration test.
package mtu

const (
	// TableMangle is the iptables table the MSS clamp lives in.
	TableMangle = "mangle"
	// MSSChain is our managed chain, hooked from POSTROUTING.
	MSSChain = "DWX-MESH-MSS"
)

// Rule is a single iptables rule: the chain it lives in and its argument list
// (excluding the table), in canonical order. It is comparable, which makes the
// planner table-testable.
type Rule struct {
	Chain string
	Args  []string
}

// BuildClampRules returns the MSS-clamp rules for the given mesh interface. It
// clamps the MSS of TCP connection-opening packets (SYN, but not SYN-ACK via the
// SYN,RST/SYN match) egressing the device to the path MTU. An empty interface
// yields no rules (nothing to clamp), so the manager simply clears the chain.
//
// The same rule is valid for IPv4 and IPv6; the manager applies it to both the
// iptables and ip6tables mangle tables.
func BuildClampRules(iface string) []Rule {
	if iface == "" {
		return nil
	}
	return []Rule{{
		Chain: MSSChain,
		Args: []string{
			"-o", iface,
			"-p", "tcp", "--tcp-flags", "SYN,RST", "SYN",
			"-j", "TCPMSS", "--clamp-mss-to-pmtu",
		},
	}}
}
