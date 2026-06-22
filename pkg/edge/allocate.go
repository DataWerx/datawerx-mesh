package edge

import (
	"fmt"
	"hash/fnv"
	"math/big"
	"net"
	"sort"
	"strings"
)

// maxProbeBudget bounds collision re-hashing beyond one attempt per key, so a
// pathological hash distribution can't loop unboundedly on a huge (IPv6) range.
// Mirrors pkg/dns.
const maxProbeBudget = 64

// DeviceClaim is one device's demand on the edge address pool: its stable
// identity key (the device public key) and an optional explicit address pin.
type DeviceClaim struct {
	// Key is the device's stable identity — its WireGuard public key. The hash of
	// this key (with the CIDR) determines the allocated address, so a device keeps
	// its address as unrelated devices come and go.
	Key string
	// Address optionally pins the device to a specific host address (a bare IP or
	// a host CIDR within the edge range). Empty means allocate deterministically.
	Address string
}

// AllocateDeviceIPs deterministically assigns a tunnel address from the edge CIDR
// to each device claim, returning a map keyed by DeviceClaim.Key whose values are
// the assigned host IPs (bare, e.g. "100.71.0.5").
//
// Like pkg/dns.AllocateClusterSetIPs, the assignment is a PURE function of the
// CIDR and the full claim set, so every gateway node's terminator computes the
// identical mapping with no broker or central allocator. Explicit pins
// (DeviceClaim.Address) are honored first and reserved so allocation flows around
// them; remaining devices are placed in sorted-key order by hashing the key into
// the range and re-hashing on collision. Offset 0 (the network address) is
// reserved. Arithmetic uses math/big so IPv6 ranges work without overflow.
func AllocateDeviceIPs(edgeCIDR string, claims []DeviceClaim) (map[string]string, error) {
	_, ipnet, err := net.ParseCIDR(strings.TrimSpace(edgeCIDR))
	if err != nil {
		return nil, fmt.Errorf("parsing edge CIDR %q: %w", edgeCIDR, err)
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
		return nil, fmt.Errorf("edge CIDR %q has a non-canonical mask", edgeCIDR)
	}
	hostBits := bits - ones

	baseInt := new(big.Int).SetBytes(base)
	size := new(big.Int).Lsh(big.NewInt(1), uint(hostBits)) // 2^hostBits addresses

	claims = sortClaims(claims)
	// Need room for every claim plus the reserved network address.
	if size.Cmp(big.NewInt(int64(len(claims))+1)) < 0 {
		return nil, fmt.Errorf("edge CIDR %q (%s addresses) too small for %d devices", edgeCIDR, size, len(claims))
	}

	used := make(map[string]struct{}, len(claims))
	out := make(map[string]string, len(claims))

	// Honor explicit pins first, reserving each so allocation flows around them.
	for _, c := range claims {
		if strings.TrimSpace(c.Address) == "" {
			continue
		}
		ip, err := pinnedIP(c.Address, ipnet)
		if err != nil {
			return nil, fmt.Errorf("device %q address pin: %w", shortKey(c.Key), err)
		}
		s := ip.String()
		if _, taken := used[s]; taken {
			return nil, fmt.Errorf("device %q pins address %s already claimed by another device", shortKey(c.Key), s)
		}
		used[s] = struct{}{}
		out[c.Key] = s
	}

	// Deterministically place the remaining devices.
	budget := len(claims) + maxProbeBudget
	for _, c := range claims {
		if strings.TrimSpace(c.Address) != "" {
			continue
		}
		s, ok := allocateOne(c.Key, baseInt, size, byteLen, budget, used)
		if !ok {
			return nil, fmt.Errorf("edge CIDR %q: exhausted probe budget allocating device %q", edgeCIDR, shortKey(c.Key))
		}
		out[c.Key] = s
	}
	return out, nil
}

// pinnedIP validates an explicit address pin: it must parse as a single host,
// fall within the edge range, and not be the reserved network address.
func pinnedIP(addr string, ipnet *net.IPNet) (net.IP, error) {
	host, err := hostCIDR(addr)
	if err != nil {
		return nil, err
	}
	ip, _, err := net.ParseCIDR(host)
	if err != nil { // unreachable after hostCIDR, but keep the contract honest
		return nil, err
	}
	if !ipnet.Contains(ip) {
		return nil, fmt.Errorf("%s is outside the edge CIDR %s", ip, ipnet)
	}
	if ip.Equal(ipnet.IP) {
		return nil, fmt.Errorf("%s is the reserved network address", ip)
	}
	if v4 := ip.To4(); v4 != nil {
		return v4, nil
	}
	return ip.To16(), nil
}

// allocateOne probes for the first free, non-reserved offset for key, records it
// in used, and returns the chosen IP string. Mirrors pkg/dns.allocateOne.
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

// sortClaims returns claims ordered by key, so collision resolution is identical
// on every node regardless of the order the EdgeDevices were observed in.
func sortClaims(claims []DeviceClaim) []DeviceClaim {
	out := append([]DeviceClaim(nil), claims...)
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// shortKey truncates a WireGuard key for log/error messages so full key material
// never leaks. Mirrors the helper in pkg/wg and pkg/topology.
func shortKey(k string) string {
	if len(k) <= 8 {
		return k
	}
	return k[:8] + "…"
}
