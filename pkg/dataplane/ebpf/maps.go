// Package ebpf is the premium, high-performance data plane for overlapping-CIDR
// remap. It is the premium alternative to the open-core iptables NETMAP
// (pkg/nat). Instead of two NETMAP chains traversed per packet, a TC/eBPF
// program attached to the mesh device rewrites addresses in the kernel fast
// path using LPM-trie maps, with no conntrack and no chain churn.
//
// Open-core boundary - this package contains the control-plane half.  It is the
// pure computation of the BPF map contents from the same RemapEntry set the
// iptables path consumes, plus a full-state reconcile Manager over an injectable
// map-ops seam. That half is dependency-free and fully unit-tested here. The
// kernel binding (loading the compiled CO-RE object, attaching the TC programs,
// and a libbpf/cilium-ebpf-backed MapOps) is the premium build and is described
// in;
// Load() returns a clear error in the open-core binary so selecting the backend
// fails fast rather than silently.
//
// Keeping the Manager behind the same controllers.RemapDataPlane interface as
// nat.Manager means the reconciler never branches on which backend is active —
// the open-core design principle.
package ebpf

import (
	"fmt"
	"net"
	"sort"

	"github.com/DataWerx/datawerx-mesh/pkg/nat"
)

// TrieKey is the key layout of the LPM-trie BPF maps: a prefix length followed
// by the big-endian network address. It mirrors struct bpf_lpm_trie_key for
// an IPv4 datapath, so the planner output maps 1:1 onto what the loader writes.
type TrieKey struct {
	PrefixLen uint32
	Addr      [4]byte
}

// Rewrite is the map value: the base address (big-endian) the matched prefix is
// rewritten onto, plus the prefix length (always equal to the key's, since the
// remap is a 1:1 same-size block translation that preserves host bits).
type Rewrite struct {
	Base      [4]byte
	PrefixLen uint32
}

// MapEntry is one key → value pair destined for a BPF map, carrying the parsed
// binary form the loader needs plus the CIDR strings for readable logs/tests.
type MapEntry struct {
	Key     TrieKey
	Val     Rewrite
	KeyCIDR string
	ValCIDR string
}

// MapID identifies which of the two datapath maps an entry belongs to.
type MapID int

const (
	// IngressMap rewrites the destination of packets arriving from the tunnel:
	// dst ∈ virtual → real so traffic reaches the real local pod. Keyed by the
	// virtual prefix.
	IngressMap MapID = iota
	// EgressMap rewrites the source of packets leaving toward the tunnel:
	// src ∈ real → virtual so the remote peer sees the agreed virtual source.
	// Keyed by the real prefix.
	EgressMap
)

func (m MapID) String() string {
	if m == IngressMap {
		return "ingress"
	}
	return "egress"
}

// RemapMaps is the full, deterministic desired state of both datapath maps.
type RemapMaps struct {
	Ingress []MapEntry
	Egress  []MapEntry
}

// BuildRemapMaps compiles the local real⇄virtual entries into the two LPM-trie
// maps the eBPF datapath programs. It is a pure function, sorted, de-duplicated
// output. The testable heart of the premium backend, exactly analogous to
// nat.BuildRemapRules for the iptables path.
//
// For each entry {Real, Virtual} which must be equal-length IPv4 prefixes for
// a 1:1 NETMAP:
//
//	ingress: key=virtual → rewrite dst base=real
//	egress:  key=real    → rewrite src base=virtual
func BuildRemapMaps(entries []nat.RemapEntry) (RemapMaps, error) {
	var out RemapMaps
	seen := map[nat.RemapEntry]struct{}{}

	for _, e := range entries {
		if e.Real == "" || e.Virtual == "" {
			continue
		}
		if _, ok := seen[e]; ok {
			continue
		}
		seen[e] = struct{}{}

		realKey, realCIDR, err := parseV4Prefix(e.Real)
		if err != nil {
			return RemapMaps{}, fmt.Errorf("ebpf remap: real %q: %w", e.Real, err)
		}
		virtKey, virtCIDR, err := parseV4Prefix(e.Virtual)
		if err != nil {
			return RemapMaps{}, fmt.Errorf("ebpf remap: virtual %q: %w", e.Virtual, err)
		}
		if realKey.PrefixLen != virtKey.PrefixLen {
			return RemapMaps{}, fmt.Errorf("ebpf remap: %s and %s differ in prefix length; a 1:1 NETMAP requires equal-size blocks", realCIDR, virtCIDR)
		}

		out.Ingress = append(out.Ingress, MapEntry{
			Key:     virtKey,
			Val:     Rewrite{Base: realKey.Addr, PrefixLen: realKey.PrefixLen},
			KeyCIDR: virtCIDR, ValCIDR: realCIDR,
		})
		out.Egress = append(out.Egress, MapEntry{
			Key:     realKey,
			Val:     Rewrite{Base: virtKey.Addr, PrefixLen: virtKey.PrefixLen},
			KeyCIDR: realCIDR, ValCIDR: virtCIDR,
		})
	}

	sortEntries(out.Ingress)
	sortEntries(out.Egress)
	return out, nil
}

// parseV4Prefix parses an IPv4 CIDR into a TrieKey prefix length + network
// base, and its canonical string form.
func parseV4Prefix(cidr string) (TrieKey, string, error) {
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return TrieKey{}, "", err
	}
	v4 := ip.To4()
	if v4 == nil || ipnet.IP.To4() == nil {
		return TrieKey{}, "", fmt.Errorf("not an IPv4 prefix")
	}
	ones, _ := ipnet.Mask.Size()
	var k TrieKey
	k.PrefixLen = uint32(ones)
	copy(k.Addr[:], ipnet.IP.To4()) // network base, masked
	return k, ipnet.String(), nil
}

func sortEntries(es []MapEntry) {
	sort.Slice(es, func(i, j int) bool { return es[i].KeyCIDR < es[j].KeyCIDR })
}
