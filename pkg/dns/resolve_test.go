package dns_test

import (
	"testing"

	"github.com/DataWerx/datawerx-mesh/pkg/dns"
)

func TestInClusterSetZone(t *testing.T) {
	in := []string{
		"payments.prod.svc.clusterset.local.",
		"payments.prod.svc.clusterset.local",
		"PAYMENTS.PROD.SVC.CLUSTERSET.LOCAL.",
		"svc.clusterset.local",
	}
	for _, q := range in {
		if !dns.InClusterSetZone(q) {
			t.Errorf("InClusterSetZone(%q) = false, want true", q)
		}
	}
	out := []string{"payments.prod.svc.cluster.local.", "example.com.", "clusterset.local."}
	for _, q := range out {
		if dns.InClusterSetZone(q) {
			t.Errorf("InClusterSetZone(%q) = true, want false", q)
		}
	}
}

func TestParseClusterSetName(t *testing.T) {
	tests := []struct {
		qname    string
		wantName string
		wantNS   string
		wantOK   bool
	}{
		{"payments.prod.svc.clusterset.local.", "payments", "prod", true},
		{"payments.prod.svc.clusterset.local", "payments", "prod", true},
		{"DB.Data.svc.clusterset.local.", "db", "data", true},
		// too many labels (e.g. SRV-style) — not a plain A/AAAA name.
		{"_http._tcp.payments.prod.svc.clusterset.local.", "", "", false},
		// too few labels.
		{"prod.svc.clusterset.local.", "", "", false},
		{"svc.clusterset.local.", "", "", false},
		// outside the zone.
		{"payments.prod.svc.cluster.local.", "", "", false},
		{"example.com.", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.qname, func(t *testing.T) {
			name, ns, ok := dns.ParseClusterSetName(tt.qname)
			if name != tt.wantName || ns != tt.wantNS || ok != tt.wantOK {
				t.Errorf("ParseClusterSetName(%q) = (%q,%q,%v), want (%q,%q,%v)",
					tt.qname, name, ns, ok, tt.wantName, tt.wantNS, tt.wantOK)
			}
		})
	}
}
