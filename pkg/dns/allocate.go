package dns

import (
	"fmt"
	"hash/fnv"
	"math/big"
	"net"
)

// maxProbeBudget bounds collision re-hashing beyond one attempt per key, so a
// pathological hash distribution can't loop unboundedly on a huge (IPv6) range.
const maxProbeBudget = 64

// AllocateClusterSetIPs deterministically assigns a virtual ClusterSetIP from
// the given IPv4 or IPv6 CIDR to each ServiceKey.
//
// The assignment is a PURE function of the CIDR and the set of keys, so every
// cluster in the mesh that observes the same set of exported services computes
// the identical mapping — no broker or central allocator is required.
//
// Algorithm: keys are processed in sorted order; each key hashes with the CIDR
// size to an offset in the range and is placed there, re-hashing on collision.
// Hashing keeps a service's address stable as unrelated services come and go;
// the sorted, set-deterministic probe makes collision resolution identical
// everywhere. Offset 0 (the network address) is reserved. Arithmetic uses
// math/big so IPv6 ranges up to 128 bits work without overflow.
func AllocateClusterSetIPs(cidr string, keys []ServiceKey) (map[ServiceKey]string, error) {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("parsing ClusterSet CIDR %q: %w", cidr, err)
	}

	base := ipnet.IP
	if v4 := base.To4(); v4 != nil {
		base = v4
	} else {
		base = base.To16()
	}
	byteLen := len(base)
	ones, bits := ipnet.Mask.Size()
	if bits == 0 {
		return nil, fmt.Errorf("ClusterSet CIDR %q has a non-canonical mask", cidr)
	}
	hostBits := bits - ones

	baseInt := new(big.Int).SetBytes(base)
	size := new(big.Int).Lsh(big.NewInt(1), uint(hostBits)) // 2^hostBits addresses

	keys = SortServiceKeys(keys)
	// Need room for every key plus the reserved network address, i.e.
	// size >= len(keys)+1. Erroring at size == len(keys)+1 would wrongly reject a
	// range that fits exactly (e.g. a /30's 3 usable addresses for 3 services).
	if size.Cmp(big.NewInt(int64(len(keys))+1)) < 0 {
		return nil, fmt.Errorf("ClusterSet CIDR %q (%s addresses) too small for %d services", cidr, size, len(keys))
	}

	used := make(map[string]struct{}, len(keys))
	out := make(map[ServiceKey]string, len(keys))
	budget := len(keys) + maxProbeBudget

	for _, k := range keys {
		s, ok := allocateOne(k.String(), baseInt, size, byteLen, budget, used)
		if !ok {
			return nil, fmt.Errorf("ClusterSet CIDR %q: exhausted probe budget allocating %s", cidr, k)
		}
		out[k] = s
	}
	return out, nil
}

// allocateOne probes for the first free, non-reserved offset for key, records
// it in used, and returns the chosen IP string. It reports false if the probe
// budget is exhausted without finding a free address.
func allocateOne(key string, baseInt, size *big.Int, byteLen, budget int, used map[string]struct{}) (string, bool) {
	for attempt := 0; attempt < budget; attempt++ {
		off := offsetInRange(key, attempt, size)
		if off.Sign() == 0 {
			continue // reserve the network address
		}
		ip := bigToIP(new(big.Int).Add(baseInt, off), byteLen)
		s := ip.String()
		if _, taken := used[s]; taken {
			continue
		}
		used[s] = struct{}{}
		return s, true
	}
	return "", false
}

// offsetInRange derives a deterministic offset in [0, size) from key+attempt.
// The 64-bit hash is reduced modulo size; for ranges larger than 2^64 the
// offset spans the low 64 bits, which is ample (services ≪ range).
func offsetInRange(key string, attempt int, size *big.Int) *big.Int {
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "%s#%d", key, attempt)
	hv := new(big.Int).SetUint64(h.Sum64())
	return hv.Mod(hv, size)
}

// bigToIP renders v as a byteLen-wide IP - left-zero-padded.
func bigToIP(v *big.Int, byteLen int) net.IP {
	b := v.Bytes()
	switch {
	case len(b) < byteLen:
		padded := make([]byte, byteLen)
		copy(padded[byteLen-len(b):], b)
		b = padded
	case len(b) > byteLen:
		b = b[len(b)-byteLen:]
	}
	return net.IP(b)
}
