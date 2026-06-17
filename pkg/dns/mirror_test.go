package dns_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"

	mcsv1alpha1 "github.com/DataWerx/datawerx-mesh/pkg/apis/multicluster/v1alpha1"
	"github.com/DataWerx/datawerx-mesh/pkg/dns"
)

func TestPlanEndpointSlices_SingleClusterIPv4(t *testing.T) {
	slices := dns.PlanEndpointSlices("payments", "prod", []dns.ExportedEndpoint{
		{
			Cluster: "east",
			Type:    mcsv1alpha1.Headless,
			Ports:   []mcsv1alpha1.ServicePort{{Name: "http", Port: 80, Protocol: corev1.ProtocolTCP}},
			IPs:     []string{"10.0.0.2", "10.0.0.1"},
		},
	})
	if len(slices) != 1 {
		t.Fatalf("expected one IPv4 slice, got %d", len(slices))
	}
	s := slices[0]
	if s.Namespace != "prod" {
		t.Errorf("namespace = %q, want prod", s.Namespace)
	}
	if s.AddressType != discoveryv1.AddressTypeIPv4 {
		t.Errorf("address type = %q, want IPv4", s.AddressType)
	}
	if got := s.Labels[mcsv1alpha1.LabelServiceName]; got != "payments" {
		t.Errorf("service-name label = %q, want payments", got)
	}
	if got := s.Labels[mcsv1alpha1.LabelSourceCluster]; got != "east" {
		t.Errorf("source-cluster label = %q, want east", got)
	}
	if got := s.Labels[discoveryv1.LabelManagedBy]; got != dns.MirrorManagedBy {
		t.Errorf("managed-by label = %q, want %q", got, dns.MirrorManagedBy)
	}
	// Addresses are sorted and each occupies its own endpoint.
	if len(s.Endpoints) != 2 || s.Endpoints[0].Addresses[0] != "10.0.0.1" || s.Endpoints[1].Addresses[0] != "10.0.0.2" {
		t.Errorf("endpoints not sorted/expanded: %+v", s.Endpoints)
	}
	if r := s.Endpoints[0].Conditions.Ready; r == nil || !*r {
		t.Error("mirrored endpoints must be marked Ready")
	}
	if len(s.Ports) != 1 || s.Ports[0].Port == nil || *s.Ports[0].Port != 80 {
		t.Fatalf("port not carried: %+v", s.Ports)
	}
	if s.Ports[0].Protocol == nil || *s.Ports[0].Protocol != corev1.ProtocolTCP {
		t.Errorf("protocol = %v, want TCP", s.Ports[0].Protocol)
	}
	if s.Ports[0].Name == nil || *s.Ports[0].Name != "http" {
		t.Errorf("port name not carried: %+v", s.Ports[0].Name)
	}
}

func TestPlanEndpointSlices_DualStackSplitsByFamily(t *testing.T) {
	slices := dns.PlanEndpointSlices("db", "data", []dns.ExportedEndpoint{
		{Cluster: "west", Type: mcsv1alpha1.Headless, IPs: []string{"10.1.0.1", "2001:db8::1", "10.1.0.2"}},
	})
	if len(slices) != 2 {
		t.Fatalf("expected one slice per family, got %d", len(slices))
	}
	byFamily := map[discoveryv1.AddressType]discoveryv1.EndpointSlice{}
	for _, s := range slices {
		byFamily[s.AddressType] = s
	}
	if v4, ok := byFamily[discoveryv1.AddressTypeIPv4]; !ok || len(v4.Endpoints) != 2 {
		t.Errorf("IPv4 slice should hold both v4 addrs, got %+v", v4.Endpoints)
	}
	if v6, ok := byFamily[discoveryv1.AddressTypeIPv6]; !ok || len(v6.Endpoints) != 1 || v6.Endpoints[0].Addresses[0] != "2001:db8::1" {
		t.Errorf("IPv6 slice should hold the v6 addr, got %+v", v6.Endpoints)
	}
}

func TestPlanEndpointSlices_PerClusterAndDeterministic(t *testing.T) {
	endpoints := []dns.ExportedEndpoint{
		{Cluster: "west", Type: mcsv1alpha1.Headless, IPs: []string{"10.2.0.1"}},
		{Cluster: "east", Type: mcsv1alpha1.Headless, IPs: []string{"10.1.0.1"}},
	}
	first := dns.PlanEndpointSlices("svc", "ns", endpoints)
	second := dns.PlanEndpointSlices("svc", "ns", endpoints)
	if len(first) != 2 {
		t.Fatalf("expected one slice per source cluster, got %d", len(first))
	}
	// Sorted by name, and a source-cluster label per slice keeps contributions
	// distinct.
	if first[0].Name >= first[1].Name {
		t.Errorf("slices not sorted by name: %q, %q", first[0].Name, first[1].Name)
	}
	for i := range first {
		if first[i].Name != second[i].Name || first[i].AddressType != second[i].AddressType {
			t.Errorf("output not deterministic at %d: %q vs %q", i, first[i].Name, second[i].Name)
		}
	}
}

func TestPlanEndpointSlices_ChunksLargeClusters(t *testing.T) {
	addrs := make([]string, 0, 150)
	for i := 0; i < 150; i++ {
		addrs = append(addrs, ipv4(i))
	}
	slices := dns.PlanEndpointSlices("big", "ns", []dns.ExportedEndpoint{
		{Cluster: "east", Type: mcsv1alpha1.Headless, IPs: addrs},
	})
	if len(slices) != 2 {
		t.Fatalf("150 addresses should split into 2 slices (cap 100), got %d", len(slices))
	}
	total := len(slices[0].Endpoints) + len(slices[1].Endpoints)
	if total != 150 {
		t.Errorf("expected all 150 endpoints across slices, got %d", total)
	}
	if len(slices[0].Endpoints) > 100 || len(slices[1].Endpoints) > 100 {
		t.Errorf("a slice exceeded the 100-endpoint cap: %d, %d", len(slices[0].Endpoints), len(slices[1].Endpoints))
	}
}

func TestPlanEndpointSlices_EmptyAndUnparseable(t *testing.T) {
	if s := dns.PlanEndpointSlices("svc", "ns", nil); len(s) != 0 {
		t.Errorf("no endpoints should yield no slices, got %d", len(s))
	}
	// An endpoint whose only address is unparseable yields no slice.
	s := dns.PlanEndpointSlices("svc", "ns", []dns.ExportedEndpoint{
		{Cluster: "east", Type: mcsv1alpha1.Headless, IPs: []string{"not-an-ip"}},
	})
	if len(s) != 0 {
		t.Errorf("unparseable-only endpoint should yield no slice, got %d", len(s))
	}
}

func ipv4(i int) string {
	return "10.0." + itoa(i/256) + "." + itoa(i%256)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
