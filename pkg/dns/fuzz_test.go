package dns_test

import (
	"strings"
	"testing"

	"github.com/datawerx/datawerx/pkg/dns"
)

// FuzzParseClusterSetName feeds arbitrary query names to the clusterset.local
// parser. It must never panic, and any name it accepts must have a non-empty
// service and namespace that round-trip back through FQDN to the same identity —
// the property the DNS responder depends on to answer the right service.
func FuzzParseClusterSetName(f *testing.F) {
	for _, s := range []string{
		"payments.prod.svc.clusterset.local.",
		"payments.prod.svc.clusterset.local",
		"PAYMENTS.PROD.svc.clusterset.local",
		"svc.clusterset.local", "a.b.c.svc.clusterset.local",
		"_http._tcp.payments.prod.svc.clusterset.local", "", ".", "...",
		"x.cluster.local",
	} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, qname string) {
		name, namespace, ok := dns.ParseClusterSetName(qname)
		if !ok {
			return
		}
		if name == "" || namespace == "" {
			t.Fatalf("ParseClusterSetName(%q) ok but empty name/namespace (%q/%q)", qname, name, namespace)
		}
		// A parsed name is necessarily inside the zone.
		if !dns.InClusterSetZone(qname) {
			t.Errorf("ParseClusterSetName(%q) parsed but InClusterSetZone is false", qname)
		}
		// Round-trip: rebuilding the FQDN and re-parsing yields the same identity.
		fqdn := dns.FQDN(name, namespace)
		n2, ns2, ok2 := dns.ParseClusterSetName(fqdn)
		if !ok2 || n2 != name || ns2 != namespace {
			t.Errorf("round-trip mismatch: %q -> (%q,%q) -> %q -> (%q,%q,%v)",
				qname, name, namespace, fqdn, n2, ns2, ok2)
		}
		// The parser lowercases; idempotence on its own output must hold.
		if strings.ToLower(name) != name || strings.ToLower(namespace) != namespace {
			t.Errorf("ParseClusterSetName(%q) returned non-lowercased labels (%q,%q)", qname, name, namespace)
		}
	})
}
