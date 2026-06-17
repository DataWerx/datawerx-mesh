package routed_test

import (
	"reflect"
	"testing"

	"github.com/DataWerx/datawerx-mesh/pkg/routed"
)

func TestPlanRoutes_SortedDedupedAndScoped(t *testing.T) {
	got, err := routed.PlanRoutes([]string{"10.96.0.0/16", "10.40.0.0/16", "10.96.0.0/16", ""}, "100.64.0.5")
	if err != nil {
		t.Fatalf("PlanRoutes: %v", err)
	}
	want := []routed.Route{
		{Dest: "10.40.0.0/16", Via: "100.64.0.5"},
		{Dest: "10.96.0.0/16", Via: "100.64.0.5"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PlanRoutes = %#v, want %#v", got, want)
	}
}

func TestPlanRoutes_BadNextHop(t *testing.T) {
	if _, err := routed.PlanRoutes([]string{"10.0.0.0/8"}, "not-an-ip"); err == nil {
		t.Error("expected error for invalid next-hop")
	}
}

func TestPlanRoutes_BadCIDR(t *testing.T) {
	if _, err := routed.PlanRoutes([]string{"nonsense"}, "100.64.0.5"); err == nil {
		t.Error("expected error for invalid CIDR")
	}
}

func TestPlanRoutes_FamilyMismatch(t *testing.T) {
	// A v6 prefix cannot be routed via a v4 next-hop.
	if _, err := routed.PlanRoutes([]string{"fd00::/64"}, "100.64.0.5"); err == nil {
		t.Error("expected address-family mismatch error")
	}
	// ...and vice versa.
	if _, err := routed.PlanRoutes([]string{"10.0.0.0/8"}, "fd00::1"); err == nil {
		t.Error("expected address-family mismatch error")
	}
}

func TestPlanRoutes_IPv6(t *testing.T) {
	got, err := routed.PlanRoutes([]string{"fd00:1::/64"}, "fd00:ffff::1")
	if err != nil {
		t.Fatalf("PlanRoutes v6: %v", err)
	}
	if len(got) != 1 || got[0].Via != "fd00:ffff::1" {
		t.Errorf("unexpected v6 plan: %#v", got)
	}
}
