package dns_test

import (
	"reflect"
	"testing"

	mcsv1alpha1 "github.com/DataWerx/datawerx-mesh/pkg/apis/multicluster/v1alpha1"
	"github.com/DataWerx/datawerx-mesh/pkg/dns"
)

func TestGroupExports(t *testing.T) {
	exports := []dns.ExportedService{
		{Namespace: "prod", Name: "payments", Endpoint: dns.ExportedEndpoint{Cluster: "b", Type: mcsv1alpha1.ClusterSetIP}},
		{Namespace: "prod", Name: "payments", Endpoint: dns.ExportedEndpoint{Cluster: "a", Type: mcsv1alpha1.ClusterSetIP}},
		{Namespace: "data", Name: "db", Endpoint: dns.ExportedEndpoint{Cluster: "a", Type: mcsv1alpha1.Headless}},
	}

	grouped, keys := dns.GroupExports(exports)

	wantKeys := []dns.ServiceKey{
		{Namespace: "data", Name: "db"},
		{Namespace: "prod", Name: "payments"},
	}
	if !reflect.DeepEqual(keys, wantKeys) {
		t.Errorf("keys = %v, want %v (sorted)", keys, wantKeys)
	}

	payments := grouped[dns.ServiceKey{Namespace: "prod", Name: "payments"}]
	if len(payments) != 2 {
		t.Fatalf("expected 2 endpoints for payments, got %d", len(payments))
	}
	// Endpoints within a bucket are sorted by cluster ID.
	if payments[0].Cluster != "a" || payments[1].Cluster != "b" {
		t.Errorf("endpoints not sorted by cluster: %v", payments)
	}
}

func TestGroupExports_Empty(t *testing.T) {
	grouped, keys := dns.GroupExports(nil)
	if len(grouped) != 0 || len(keys) != 0 {
		t.Errorf("expected empty grouping, got %v / %v", grouped, keys)
	}
}

func TestServiceKeyString(t *testing.T) {
	k := dns.ServiceKey{Namespace: "prod", Name: "payments"}
	if k.String() != "prod/payments" {
		t.Errorf("String() = %q", k.String())
	}
}
