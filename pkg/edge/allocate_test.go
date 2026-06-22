package edge_test

import (
	"math/rand"
	"net"
	"testing"

	"github.com/DataWerx/datawerx-mesh/pkg/edge"
)

func claims(keys ...string) []edge.DeviceClaim {
	out := make([]edge.DeviceClaim, len(keys))
	for i, k := range keys {
		out[i] = edge.DeviceClaim{Key: k}
	}
	return out
}

func TestAllocateDeviceIPs_DeterministicAndOrderIndependent(t *testing.T) {
	const cidr = "100.71.0.0/24"
	keys := []string{"dev-a", "dev-b", "dev-c", "dev-d", "dev-e"}

	want, err := edge.AllocateDeviceIPs(cidr, claims(keys...))
	if err != nil {
		t.Fatal(err)
	}
	// Shuffle the input order a few times; the mapping must be identical.
	for i := 0; i < 10; i++ {
		shuf := append([]string(nil), keys...)
		rand.Shuffle(len(shuf), func(a, b int) { shuf[a], shuf[b] = shuf[b], shuf[a] })
		got, err := edge.AllocateDeviceIPs(cidr, claims(shuf...))
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range want {
			if got[k] != v {
				t.Fatalf("order changed allocation for %s: %s vs %s", k, got[k], v)
			}
		}
	}
}

func TestAllocateDeviceIPs_UniqueInRangeNonReserved(t *testing.T) {
	const cidr = "100.71.0.0/28" // 16 addresses
	_, ipnet, _ := net.ParseCIDR(cidr)
	m, err := edge.AllocateDeviceIPs(cidr, claims("a", "b", "c", "d", "e"))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for k, v := range m {
		ip := net.ParseIP(v)
		if ip == nil {
			t.Fatalf("%s got non-IP %q", k, v)
		}
		if !ipnet.Contains(ip) {
			t.Errorf("%s -> %s outside %s", k, v, cidr)
		}
		if ip.Equal(ipnet.IP) {
			t.Errorf("%s -> %s is the reserved network address", k, v)
		}
		if seen[v] {
			t.Errorf("address %s assigned twice", v)
		}
		seen[v] = true
	}
}

func TestAllocateDeviceIPs_StableUnderAddition(t *testing.T) {
	const cidr = "100.71.0.0/24"
	base, _ := edge.AllocateDeviceIPs(cidr, claims("a", "b", "c"))
	grown, _ := edge.AllocateDeviceIPs(cidr, claims("a", "b", "c", "d", "e"))
	for _, k := range []string{"a", "b", "c"} {
		if base[k] != grown[k] {
			t.Errorf("device %s moved from %s to %s when others were added", k, base[k], grown[k])
		}
	}
}

func TestAllocateDeviceIPs_HonorsPins(t *testing.T) {
	const cidr = "100.71.0.0/24"
	in := []edge.DeviceClaim{
		{Key: "pinned", Address: "100.71.0.42"},
		{Key: "a"},
		{Key: "b"},
	}
	m, err := edge.AllocateDeviceIPs(cidr, in)
	if err != nil {
		t.Fatal(err)
	}
	if m["pinned"] != "100.71.0.42" {
		t.Errorf("pin not honored: %s", m["pinned"])
	}
	// Allocated devices must avoid the pinned address.
	if m["a"] == "100.71.0.42" || m["b"] == "100.71.0.42" {
		t.Errorf("allocation collided with the pin: a=%s b=%s", m["a"], m["b"])
	}
}

func TestAllocateDeviceIPs_Errors(t *testing.T) {
	tests := []struct {
		name   string
		cidr   string
		claims []edge.DeviceClaim
	}{
		{"too small", "100.71.0.0/30", claims("a", "b", "c", "d")}, // 4 addrs, need 5 (4 + network)
		{"malformed cidr", "not-a-cidr", claims("a")},
		{"pin outside cidr", "100.71.0.0/24", []edge.DeviceClaim{{Key: "x", Address: "10.0.0.1"}}},
		{"pin is network address", "100.71.0.0/24", []edge.DeviceClaim{{Key: "x", Address: "100.71.0.0"}}},
		{"two pins collide", "100.71.0.0/24", []edge.DeviceClaim{
			{Key: "x", Address: "100.71.0.9"},
			{Key: "y", Address: "100.71.0.9"},
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := edge.AllocateDeviceIPs(tc.cidr, tc.claims); err == nil {
				t.Errorf("expected error for %s", tc.name)
			}
		})
	}
}

func TestAllocateDeviceIPs_IPv6(t *testing.T) {
	const cidr = "fd00:dead:beef::/64"
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatalf("bad test cidr: %v", err)
	}
	m, err := edge.AllocateDeviceIPs(cidr, claims("a", "b", "c"))
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 3 {
		t.Fatalf("got %d allocations, want 3", len(m))
	}
	for k, v := range m {
		ip := net.ParseIP(v)
		if ip == nil || ip.To4() != nil {
			t.Errorf("%s -> %q is not an IPv6 address", k, v)
		}
		if !ipnet.Contains(ip) {
			t.Errorf("%s -> %s outside %s", k, v, cidr)
		}
	}
}
