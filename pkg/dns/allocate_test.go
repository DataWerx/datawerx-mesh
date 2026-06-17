package dns_test

import (
	"net"
	"testing"

	"github.com/DataWerx/datawerx-mesh/pkg/dns"
)

func keys(specs ...string) []dns.ServiceKey {
	out := make([]dns.ServiceKey, 0, len(specs))
	for _, s := range specs {
		// "ns/name"
		for i := 0; i < len(s); i++ {
			if s[i] == '/' {
				out = append(out, dns.ServiceKey{Namespace: s[:i], Name: s[i+1:]})
				break
			}
		}
	}
	return out
}

func TestAllocateClusterSetIPs_DeterministicAndConsistent(t *testing.T) {
	cidr := "241.0.0.0/16"
	k := keys("prod/payments", "prod/orders", "data/db")

	a, err := dns.AllocateClusterSetIPs(cidr, k)
	if err != nil {
		t.Fatalf("alloc: %v", err)
	}
	// Same keys in a DIFFERENT order must yield the identical mapping — this is
	// what guarantees every cluster derives the same ClusterSetIPs.
	shuffled := []dns.ServiceKey{k[2], k[0], k[1]}
	b, err := dns.AllocateClusterSetIPs(cidr, shuffled)
	if err != nil {
		t.Fatalf("alloc2: %v", err)
	}
	for key, ip := range a {
		if b[key] != ip {
			t.Errorf("inconsistent allocation for %s: %s vs %s", key, ip, b[key])
		}
	}
}

func TestAllocateClusterSetIPs_UniqueAndInRange(t *testing.T) {
	cidr := "241.0.0.0/24"
	_, ipnet, _ := net.ParseCIDR(cidr)

	var k []dns.ServiceKey
	for _, n := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		k = append(k, dns.ServiceKey{Namespace: "ns", Name: n})
	}
	got, err := dns.AllocateClusterSetIPs(cidr, k)
	if err != nil {
		t.Fatalf("alloc: %v", err)
	}
	if len(got) != len(k) {
		t.Fatalf("expected %d allocations, got %d", len(k), len(got))
	}
	seen := map[string]bool{}
	for key, ip := range got {
		parsed := net.ParseIP(ip)
		if parsed == nil || !ipnet.Contains(parsed) {
			t.Errorf("%s: ip %q not in range %s", key, ip, cidr)
		}
		if seen[ip] {
			t.Errorf("duplicate ip %q", ip)
		}
		seen[ip] = true
		if ip == "241.0.0.0" {
			t.Errorf("network address must be reserved, got %q", ip)
		}
	}
}

func TestAllocateClusterSetIPs_StableUnderAddition(t *testing.T) {
	cidr := "241.0.0.0/16"
	base := keys("prod/payments", "prod/orders")
	withNew := keys("prod/payments", "prod/orders", "prod/new")

	a, _ := dns.AllocateClusterSetIPs(cidr, base)
	b, _ := dns.AllocateClusterSetIPs(cidr, withNew)

	// Hash-based placement keeps existing services stable as long as there's no
	// collision with the newcomer - true for this small set.
	for _, k := range base {
		if a[k] != b[k] {
			t.Errorf("allocation for %s moved on addition: %s -> %s", k, a[k], b[k])
		}
	}
}

func TestAllocateClusterSetIPs_Errors(t *testing.T) {
	if _, err := dns.AllocateClusterSetIPs("not-a-cidr", nil); err == nil {
		t.Error("expected error for bad CIDR")
	}
	// Mask of /30 has 4 addresses; with the network address reserved only 3 are usable.
	if _, err := dns.AllocateClusterSetIPs("241.0.0.0/30", keys("ns/a", "ns/b", "ns/c", "ns/d")); err == nil {
		t.Error("expected exhaustion error for oversubscribed range")
	}
	// Exactly-full: a /30's 3 usable addresses must accommodate 3 services.
	got, err := dns.AllocateClusterSetIPs("241.0.0.0/30", keys("ns/a", "ns/b", "ns/c"))
	if err != nil {
		t.Errorf("expected exactly-full /30 to succeed, got %v", err)
	}
	if len(got) != 3 {
		t.Errorf("expected 3 allocations, got %d", len(got))
	}
}

func TestAllocateClusterSetIPs_IPv6(t *testing.T) {
	const cidr = "fd00::/64"
	_, ipnet, _ := net.ParseCIDR(cidr)
	k := keys("prod/payments", "prod/orders", "data/db")

	got, err := dns.AllocateClusterSetIPs(cidr, k)
	if err != nil {
		t.Fatalf("alloc v6: %v", err)
	}
	if len(got) != len(k) {
		t.Fatalf("expected %d allocations, got %d", len(k), len(got))
	}
	seen := map[string]bool{}
	for key, ip := range got {
		parsed := net.ParseIP(ip)
		if parsed == nil || parsed.To4() != nil || !ipnet.Contains(parsed) {
			t.Errorf("%s: %q is not an IPv6 address inside %s", key, ip, cidr)
		}
		if seen[ip] {
			t.Errorf("duplicate v6 ip %q", ip)
		}
		seen[ip] = true
	}

	// Deterministic and order-independent across the two families' shared codepath.
	again, _ := dns.AllocateClusterSetIPs(cidr, []dns.ServiceKey{k[2], k[0], k[1]})
	for key, ip := range got {
		if again[key] != ip {
			t.Errorf("v6 allocation not deterministic for %s: %s vs %s", key, ip, again[key])
		}
	}
}
