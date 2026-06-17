package topology_test

import (
	"net"
	"reflect"
	"testing"

	"github.com/datawerx/datawerx/pkg/topology"
)

func mustParseCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return n
}

func TestVirtualCIDR_DeterministicAndInPool(t *testing.T) {
	const pool = "172.16.0.0/12"
	_, poolNet := mustParseCIDR(t, pool), mustParseCIDR(t, pool)

	v1, err := topology.VirtualCIDR(pool, "cluster-a", "10.244.0.0/16")
	if err != nil {
		t.Fatalf("VirtualCIDR: %v", err)
	}
	// Deterministic: same inputs => identical output (the broker-less agreement).
	v2, _ := topology.VirtualCIDR(pool, "cluster-a", "10.244.0.0/16")
	if v1 != v2 {
		t.Errorf("VirtualCIDR not deterministic: %q vs %q", v1, v2)
	}

	ip, vNet, err := net.ParseCIDR(v1)
	if err != nil {
		t.Fatalf("result %q not a CIDR: %v", v1, err)
	}
	if ones, _ := vNet.Mask.Size(); ones != 16 {
		t.Errorf("virtual prefix = /%d, want /16 (match the real)", ones)
	}
	if !poolNet.Contains(ip) {
		t.Errorf("virtual %q not inside pool %q", v1, pool)
	}
}

func TestVirtualCIDR_Errors(t *testing.T) {
	// Real range larger than the pool can't be 1:1 remapped.
	if _, err := topology.VirtualCIDR("172.16.0.0/12", "c", "10.0.0.0/8"); err == nil {
		t.Error("expected error for real /8 larger than pool /12")
	}
	// IPv6 not supported.
	if _, err := topology.VirtualCIDR("172.16.0.0/12", "c", "fd00::/64"); err == nil {
		t.Error("expected error for IPv6 real CIDR")
	}
	if _, err := topology.VirtualCIDR("bogus", "c", "10.244.0.0/16"); err == nil {
		t.Error("expected error for bad pool")
	}
}

func TestPlanRemap_OverlappingPeer(t *testing.T) {
	const pool = "172.16.0.0/12"
	// Both clusters use 10.244.0.0/16 (the classic clash).
	plan, err := topology.PlanRemap(pool, "cluster-a", []string{"10.244.0.0/16", "10.96.0.0/16"},
		"cluster-b", []string{"10.244.0.0/16"})
	if err != nil {
		t.Fatalf("PlanRemap: %v", err)
	}

	// Routes the peer's virtual range (computed from the PEER's id), not the real one.
	wantPeerVirtual, _ := topology.VirtualCIDR(pool, "cluster-b", "10.244.0.0/16")
	if !reflect.DeepEqual(plan.RouteVirtual, []string{wantPeerVirtual}) {
		t.Errorf("RouteVirtual = %v, want [%s]", plan.RouteVirtual, wantPeerVirtual)
	}

	// Local 10.244.0.0/16 overlaps and is remapped under OUR id; 10.96/16 does not.
	wantLocalVirtual, _ := topology.VirtualCIDR(pool, "cluster-a", "10.244.0.0/16")
	want := []topology.Remap{{Real: "10.244.0.0/16", Virtual: wantLocalVirtual}}
	if !reflect.DeepEqual(plan.Locals, want) {
		t.Errorf("Locals = %#v, want %#v", plan.Locals, want)
	}
}

func TestPlanRemap_DetectsVirtualCollision(t *testing.T) {
	// A /15 pool holds only two /16 blocks, so three distinct /16 peer CIDRs
	// cannot all get unique virtual ranges (pigeonhole). PlanRemap must surface
	// the collision instead of silently emitting an ambiguous NETMAP mapping.
	_, err := topology.PlanRemap("172.16.0.0/15", "cluster-a", nil,
		"cluster-b", []string{"10.0.0.0/16", "10.1.0.0/16", "10.2.0.0/16"})
	if err == nil {
		t.Fatal("expected a remap collision error for an oversubscribed pool")
	}
}

func TestPlanRemap_SymmetricAgreement(t *testing.T) {
	const pool = "172.16.0.0/12"
	// A planning its peering with B, and B planning with A, must agree on the two
	// virtual ranges (each routes the other's virtual; each NETMAPs its own).
	a, _ := topology.PlanRemap(pool, "cluster-a", []string{"10.244.0.0/16"}, "cluster-b", []string{"10.244.0.0/16"})
	b, _ := topology.PlanRemap(pool, "cluster-b", []string{"10.244.0.0/16"}, "cluster-a", []string{"10.244.0.0/16"})

	// A routes B's virtual; that must equal the virtual B presents itself under.
	if len(a.RouteVirtual) != 1 || len(b.Locals) != 1 || a.RouteVirtual[0] != b.Locals[0].Virtual {
		t.Errorf("A must route the virtual B presents: A routes %v, B presents %v", a.RouteVirtual, b.Locals)
	}
	if len(b.RouteVirtual) != 1 || len(a.Locals) != 1 || b.RouteVirtual[0] != a.Locals[0].Virtual {
		t.Errorf("B must route the virtual A presents: B routes %v, A presents %v", b.RouteVirtual, a.Locals)
	}
}
