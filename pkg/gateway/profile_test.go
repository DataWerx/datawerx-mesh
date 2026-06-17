package gateway

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestBuildAccessProfile_UnionsAndSortsRoutes(t *testing.T) {
	p := BuildAccessProfile(
		[]string{"100.64.0.5"},
		[]string{"241.0.0.0/8"},
		[]string{"10.96.0.0/16", "10.244.0.0/16"},
		DNSConfig{Addr: "100.64.0.5:5353", SearchDomains: []string{"clusterset.local"}},
	)
	wantRoutes := []string{"10.244.0.0/16", "10.96.0.0/16", "241.0.0.0/8"}
	if !reflect.DeepEqual(p.RouteCIDRs, wantRoutes) {
		t.Errorf("RouteCIDRs = %v, want %v", p.RouteCIDRs, wantRoutes)
	}
	if !reflect.DeepEqual(p.GatewayEndpoints, []string{"100.64.0.5"}) {
		t.Errorf("GatewayEndpoints = %v", p.GatewayEndpoints)
	}
	if p.DNS.Addr != "100.64.0.5:5353" {
		t.Errorf("DNS.Addr = %q", p.DNS.Addr)
	}
}

func TestBuildAccessProfile_DedupesAndDropsEmpty(t *testing.T) {
	p := BuildAccessProfile(
		[]string{"100.64.0.5", "100.64.0.5", ""},
		[]string{"241.0.0.0/8", "241.0.0.0/8"},
		[]string{"10.96.0.0/16", "", "10.96.0.0/16"},
		DNSConfig{SearchDomains: []string{"clusterset.local", "clusterset.local", ""}},
	)
	if len(p.GatewayEndpoints) != 1 {
		t.Errorf("endpoints not deduped: %v", p.GatewayEndpoints)
	}
	if !reflect.DeepEqual(p.RouteCIDRs, []string{"10.96.0.0/16", "241.0.0.0/8"}) {
		t.Errorf("routes not deduped/sorted: %v", p.RouteCIDRs)
	}
	if !reflect.DeepEqual(p.DNS.SearchDomains, []string{"clusterset.local"}) {
		t.Errorf("search domains not deduped: %v", p.DNS.SearchDomains)
	}
}

// Profiles built from the same inputs in different orders must be identical, so
// the published ConfigMap does not churn on every reconcile.
func TestBuildAccessProfile_OrderIndependent(t *testing.T) {
	a := BuildAccessProfile(
		[]string{"a", "b"}, []string{"241.0.0.0/8"}, []string{"10.96.0.0/16", "10.244.0.0/16"}, DNSConfig{})
	b := BuildAccessProfile(
		[]string{"b", "a"}, []string{"241.0.0.0/8"}, []string{"10.244.0.0/16", "10.96.0.0/16"}, DNSConfig{})
	if !reflect.DeepEqual(a, b) {
		t.Errorf("profiles differ by input order:\n a=%+v\n b=%+v", a, b)
	}
}

func TestBuildAccessProfile_EmptyInputs(t *testing.T) {
	p := BuildAccessProfile(nil, nil, nil, DNSConfig{})
	if len(p.GatewayEndpoints) != 0 || len(p.RouteCIDRs) != 0 {
		t.Errorf("expected empty profile, got %+v", p)
	}
}

func TestProfileConfigMapData_RoundTrips(t *testing.T) {
	p := BuildAccessProfile(
		[]string{"100.64.0.5"},
		[]string{"241.0.0.0/8"},
		[]string{"10.96.0.0/16"},
		DNSConfig{Addr: "100.64.0.5:5353", SearchDomains: []string{"clusterset.local"}},
	)
	data, err := ProfileConfigMapData(p)
	if err != nil {
		t.Fatalf("ProfileConfigMapData: %v", err)
	}
	raw, ok := data[ProfileConfigMapKey]
	if !ok {
		t.Fatalf("data missing key %q: %v", ProfileConfigMapKey, data)
	}

	var got AccessProfile
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got, p) {
		t.Errorf("round-trip mismatch:\n got=%+v\nwant=%+v", got, p)
	}
}

func TestProfileFromConfigMapData_RoundTrips(t *testing.T) {
	p := BuildAccessProfile(
		[]string{"100.64.0.5"}, []string{"241.0.0.0/8"}, []string{"10.96.0.0/16"},
		DNSConfig{Addr: "100.64.0.5:5353", SearchDomains: []string{"clusterset.local"}})
	data, err := ProfileConfigMapData(p)
	if err != nil {
		t.Fatalf("ProfileConfigMapData: %v", err)
	}
	got, err := ProfileFromConfigMapData(data)
	if err != nil {
		t.Fatalf("ProfileFromConfigMapData: %v", err)
	}
	if !reflect.DeepEqual(got, p) {
		t.Errorf("round-trip mismatch:\n got=%+v\nwant=%+v", got, p)
	}
}

func TestProfileFromConfigMapData_MissingKeyIsError(t *testing.T) {
	if _, err := ProfileFromConfigMapData(map[string]string{"other": "{}"}); err == nil {
		t.Fatal("expected an error when the profile key is absent")
	}
}

func TestDecodeProfile_RejectsMalformedJSON(t *testing.T) {
	if _, err := DecodeProfile([]byte("{not json")); err == nil {
		t.Fatal("expected an error decoding malformed JSON")
	}
}

// Identical profiles must serialize to byte-identical ConfigMap data so the
// reconciler's CreateOrUpdate is a no-op when nothing changed.
func TestProfileConfigMapData_Deterministic(t *testing.T) {
	p := BuildAccessProfile(
		[]string{"100.64.0.5"}, []string{"241.0.0.0/8"}, []string{"10.96.0.0/16", "10.244.0.0/16"}, DNSConfig{})
	d1, err := ProfileConfigMapData(p)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	d2, err := ProfileConfigMapData(p)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if d1[ProfileConfigMapKey] != d2[ProfileConfigMapKey] {
		t.Errorf("serialization not deterministic:\n %q\n %q", d1[ProfileConfigMapKey], d2[ProfileConfigMapKey])
	}
}
