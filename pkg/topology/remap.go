package topology

import (
	"fmt"
	"hash/fnv"
	"net"
	"sort"
)

// DefaultRemapPool is the IPv4 range from which virtual, non-overlapping, CIDRs
// are carved when a peer's real CIDR collides with a local range. 172.16.0.0/12
// is RFC-1918 space reserved for this purpose by convention.
const DefaultRemapPool = "172.16.0.0/12"

// Remap is a single real⇄virtual CIDR pair for the local cluster. The data
// plane installs two stateless 1:1 NETMAP rules from it:
//
//	PREROUTING : -d Virtual -j NETMAP --to Real   (inbound: peers address us by Virtual)
//	POSTROUTING: -s Real    -j NETMAP --to Virtual (outbound: our src presented as Virtual)
type Remap struct {
	Real    string
	Virtual string
}

// RemapPlan is the desired overlap-remap state for one peer.
//
// The model each cluster presents its own overlapping range under a
// deterministic virtual range, and routes a peer's overlapping range
// under the peer's virtual range. Because VirtualCIDR is a pure function
// of (clusterID, realCIDR), both ends of a peering compute identical ranges
// with no coordination, so the NETMAP rules compose symmetrically.
type RemapPlan struct {
	// RouteVirtual are the peer's virtual CIDRs to route over WireGuard in place
	// of its conflicting real CIDRs. Sorted and deduplicated.
	RouteVirtual []string
	// Locals are the local real⇄virtual pairs to program as NETMAP rules which
	// are sorted by Real and deduplicated. These are this cluster's own ranges
	// that overlap the peer, presented to the mesh under their virtual range.
	Locals []Remap
}

// VirtualCIDR deterministically maps a real CIDR to a virtual CIDR of the same
// prefix length inside pool, keyed by (clusterID, realCIDR). The result is a
// pure function of its inputs, so every cluster computes the same virtual range
// for a given cluster's real range — the broker-less agreement the NETMAP rules
// rely on. IPv4 only; the real prefix must fit inside the pool.
func VirtualCIDR(pool, clusterID, realCIDR string) (string, error) {
	_, poolNet, err := net.ParseCIDR(pool)
	if err != nil {
		return "", fmt.Errorf("parsing remap pool %q: %w", pool, err)
	}
	poolBase := poolNet.IP.To4()
	if poolBase == nil {
		return "", fmt.Errorf("remap pool %q must be IPv4", pool)
	}
	poolOnes, _ := poolNet.Mask.Size()

	_, realNet, err := net.ParseCIDR(realCIDR)
	if err != nil {
		return "", fmt.Errorf("parsing real CIDR %q: %w", realCIDR, err)
	}
	if realNet.IP.To4() == nil {
		return "", fmt.Errorf("real CIDR %q must be IPv4 to remap", realCIDR)
	}
	realOnes, _ := realNet.Mask.Size()
	if realOnes < poolOnes {
		return "", fmt.Errorf("real CIDR %q (/%d) is larger than the remap pool %q (/%d)", realCIDR, realOnes, pool, poolOnes)
	}

	// Number of /realOnes blocks that fit in the pool, and the block size.
	blockBits := uint(32 - realOnes)
	numBlocks := uint64(1) << uint(realOnes-poolOnes)
	index := uint64(hash32(clusterID+"|"+realCIDR)) % numBlocks

	virtual := ipToUint32(poolBase) + uint32(index)<<blockBits
	return fmt.Sprintf("%s/%d", uint32ToIP(virtual).String(), realOnes), nil
}

// PlanRemap computes the remap for one peer: which of the peer's conflicting
// CIDRs to route under virtual ranges, and which local ranges to NETMAP. Only
// the supplied conflicting CIDRs are remapped. Callers pass the conflicts from
// PlanPeer. Non-overlapping CIDRs route directly and are not this function's
// concern.
func PlanRemap(pool, localClusterID string, localCIDRs []string, peerClusterID string, conflictingPeerCIDRs []string) (RemapPlan, error) {
	locals := ParseCIDRList(localCIDRs)

	routeSet := map[string]struct{}{}
	localSet := map[string]Remap{}

	// VirtualCIDR is a per-CIDR hash with no probing, so two distinct real CIDRs
	// can hash to the same virtual block. The pool only holds a limited number of
	// /prefix blocks. Such a collision would make the resulting NETMAP rules
	// ambiguous — two reals mapped to one virtual cannot be demuxed on the inbound
	// path. Detect it and fail safe and the reconciler reports Phase=Error. Rather
	// than silently program a broken, traffic-corrupting mapping. owner records,
	// per virtual, the distinct source that claimed it.
	owner := map[string]string{}
	claim := func(virtual, source string) error {
		if prev, ok := owner[virtual]; ok && prev != source {
			return fmt.Errorf("remap collision in pool %s: %s and %s both map to virtual %s; "+
				"choose a larger remap pool or non-overlapping CIDRs", pool, prev, source, virtual)
		}
		owner[virtual] = source
		return nil
	}

	for _, cy := range conflictingPeerCIDRs {
		_, cyNet, err := net.ParseCIDR(cy)
		if err != nil {
			// Malformed conflicting CIDR: cannot remap it. Skip; the reconciler
			// surfaces malformed input elsewhere.
			continue
		}
		vy, err := VirtualCIDR(pool, peerClusterID, cy)
		if err != nil {
			return RemapPlan{}, fmt.Errorf("allocating virtual CIDR for peer %s %s: %w", peerClusterID, cy, err)
		}
		if err := claim(vy, "peer "+peerClusterID+" "+cy); err != nil {
			return RemapPlan{}, err
		}
		routeSet[vy] = struct{}{}

		// Every local range that overlaps this peer CIDR must present itself
		// under its own virtual range.
		for _, cx := range locals {
			if !cidrsOverlap(cx, cyNet) {
				continue
			}
			real := cx.String()
			vx, err := VirtualCIDR(pool, localClusterID, real)
			if err != nil {
				return RemapPlan{}, fmt.Errorf("allocating virtual CIDR for local %s: %w", real, err)
			}
			if err := claim(vx, "local "+localClusterID+" "+real); err != nil {
				return RemapPlan{}, err
			}
			localSet[real] = Remap{Real: real, Virtual: vx}
		}
	}

	plan := RemapPlan{RouteVirtual: keysSorted(routeSet)}
	for _, real := range sortedRemapKeys(localSet) {
		plan.Locals = append(plan.Locals, localSet[real])
	}
	return plan, nil
}

// cidrsOverlap reports whether two networks overlap (either contains the
// other's base address).
func cidrsOverlap(a, b *net.IPNet) bool {
	return a.Contains(b.IP) || b.Contains(a.IP)
}

func hash32(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return h.Sum32()
}

func ipToUint32(ip net.IP) uint32 {
	ip = ip.To4()
	return uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
}

func uint32ToIP(v uint32) net.IP {
	return net.IPv4(byte(v>>24), byte(v>>16), byte(v>>8), byte(v)).To4()
}

func keysSorted(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedRemapKeys(m map[string]Remap) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
