package topology_test

import (
	"net"
	"regexp"
	"strings"
	"testing"

	"github.com/datawerx/datawerx/pkg/topology"
)

// dnsName matches a legal RFC 1123 subdomain — the shape a Kubernetes object
// name must have. SanitizeName must always produce one.
var dnsName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// FuzzSanitizeName asserts the invariants the reconciler and `dwxctl join` rely
// on: SanitizeName turns any string — including an arbitrarily long, hostile
// cluster ID from an untrusted join bundle — into a non-empty, legal Kubernetes
// object name, deterministically and idempotently.
func FuzzSanitizeName(f *testing.F) {
	for _, s := range []string{
		"", "cluster-a", "Cluster.A", "!!!", "-leading", "trailing-",
		"a_b", "münchen", strings.Repeat("x", 5000), strings.Repeat("-", 300),
		strings.Repeat("a.b", 200),
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, id string) {
		got := topology.SanitizeName(id)

		if got == "" {
			t.Fatalf("SanitizeName(%q) returned empty", id)
		}
		if len(got) > 253 {
			t.Errorf("SanitizeName(%q) is %d chars, over the 253 object-name limit", id, len(got))
		}
		if !dnsName.MatchString(got) {
			t.Errorf("SanitizeName(%q) = %q is not a legal DNS-1123 name", id, got)
		}
		// Deterministic.
		if again := topology.SanitizeName(id); again != got {
			t.Errorf("SanitizeName(%q) not deterministic: %q vs %q", id, got, again)
		}
		// Idempotent: a name that is already sanitized survives unchanged.
		if reapplied := topology.SanitizeName(got); reapplied != got {
			t.Errorf("SanitizeName not idempotent: SanitizeName(%q) = %q", got, reapplied)
		}
	})
}

// FuzzParseCIDRList asserts the helper never panics on arbitrary input and only
// ever returns successfully parsed networks.
func FuzzParseCIDRList(f *testing.F) {
	f.Add("10.0.0.0/8")
	f.Add("10.0.0.0/8,not-a-cidr,fd00::/8")
	f.Add(",,,")
	f.Fuzz(func(t *testing.T, joined string) {
		nets := topology.ParseCIDRList(strings.Split(joined, ","))
		for _, n := range nets {
			if n == nil {
				t.Fatalf("ParseCIDRList(%q) returned a nil network", joined)
			}
			if _, _, err := net.ParseCIDR(n.String()); err != nil {
				t.Errorf("ParseCIDRList(%q) returned an unparseable net %q: %v", joined, n.String(), err)
			}
		}
	})
}

// FuzzVirtualCIDR asserts the remap allocator never panics and, when it
// succeeds, returns a valid CIDR contained within the pool.
func FuzzVirtualCIDR(f *testing.F) {
	f.Add("172.30.0.0/16", "cluster-a", "10.244.0.0/24")
	f.Add("", "", "")
	f.Add("172.30.0.0/16", "c", "0.0.0.0/0")
	f.Fuzz(func(t *testing.T, pool, clusterID, real string) {
		got, err := topology.VirtualCIDR(pool, clusterID, real)
		if err != nil {
			return // rejection is fine; we only care that it didn't panic
		}
		_, gotNet, perr := net.ParseCIDR(got)
		if perr != nil {
			t.Fatalf("VirtualCIDR returned unparseable CIDR %q: %v", got, perr)
		}
		_, poolNet, _ := net.ParseCIDR(pool)
		if poolNet != nil && !poolNet.Contains(gotNet.IP) {
			t.Errorf("VirtualCIDR(%q,%q,%q) = %q is outside the pool", pool, clusterID, real, got)
		}
	})
}
