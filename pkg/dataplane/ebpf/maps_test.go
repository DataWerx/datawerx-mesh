package ebpf_test

import (
	"testing"

	"github.com/datawerx/datawerx/pkg/dataplane/ebpf"
	"github.com/datawerx/datawerx/pkg/nat"
)

func TestBuildRemapMaps_BidirectionalAndExact(t *testing.T) {
	entries := []nat.RemapEntry{{Real: "10.244.0.0/16", Virtual: "172.20.0.0/16"}}
	maps, err := ebpf.BuildRemapMaps(entries)
	if err != nil {
		t.Fatalf("BuildRemapMaps: %v", err)
	}
	if len(maps.Ingress) != 1 || len(maps.Egress) != 1 {
		t.Fatalf("expected 1 entry per map, got ingress=%d egress=%d", len(maps.Ingress), len(maps.Egress))
	}

	// Ingress rewrites dst virtual→real: key=virtual, val base=real.
	in := maps.Ingress[0]
	if in.KeyCIDR != "172.20.0.0/16" || in.ValCIDR != "10.244.0.0/16" {
		t.Errorf("ingress = key %s val %s, want virtual→real", in.KeyCIDR, in.ValCIDR)
	}
	if in.Key.PrefixLen != 16 || in.Key.Addr != [4]byte{172, 20, 0, 0} {
		t.Errorf("ingress key = %+v, want /16 172.20.0.0", in.Key)
	}
	if in.Val.Base != [4]byte{10, 244, 0, 0} || in.Val.PrefixLen != 16 {
		t.Errorf("ingress val = %+v, want base 10.244.0.0/16", in.Val)
	}

	// Egress rewrites src real→virtual: key=real, val base=virtual.
	eg := maps.Egress[0]
	if eg.KeyCIDR != "10.244.0.0/16" || eg.ValCIDR != "172.20.0.0/16" {
		t.Errorf("egress = key %s val %s, want real→virtual", eg.KeyCIDR, eg.ValCIDR)
	}
	if eg.Val.Base != [4]byte{172, 20, 0, 0} {
		t.Errorf("egress val base = %v, want 172.20.0.0", eg.Val.Base)
	}
}

func TestBuildRemapMaps_DedupeAndDeterministic(t *testing.T) {
	entries := []nat.RemapEntry{
		{Real: "10.60.0.0/16", Virtual: "172.30.0.0/16"},
		{Real: "10.244.0.0/16", Virtual: "172.20.0.0/16"},
		{Real: "10.244.0.0/16", Virtual: "172.20.0.0/16"}, // duplicate
	}
	maps, err := ebpf.BuildRemapMaps(entries)
	if err != nil {
		t.Fatalf("BuildRemapMaps: %v", err)
	}
	if len(maps.Ingress) != 2 {
		t.Fatalf("expected 2 unique entries, got %d", len(maps.Ingress))
	}
	// Sorted by key CIDR: 172.20.0.0/16 before 172.30.0.0/16.
	if maps.Ingress[0].KeyCIDR != "172.20.0.0/16" || maps.Ingress[1].KeyCIDR != "172.30.0.0/16" {
		t.Errorf("ingress not sorted deterministically: %s, %s", maps.Ingress[0].KeyCIDR, maps.Ingress[1].KeyCIDR)
	}
}

func TestBuildRemapMaps_RejectsMismatchedPrefix(t *testing.T) {
	_, err := ebpf.BuildRemapMaps([]nat.RemapEntry{{Real: "10.244.0.0/16", Virtual: "172.20.0.0/24"}})
	if err == nil {
		t.Fatal("expected error for mismatched prefix lengths (not a 1:1 NETMAP)")
	}
}

func TestBuildRemapMaps_RejectsIPv6(t *testing.T) {
	_, err := ebpf.BuildRemapMaps([]nat.RemapEntry{{Real: "fd00::/64", Virtual: "fd01::/64"}})
	if err == nil {
		t.Fatal("expected error for IPv6 (IPv4 datapath)")
	}
}

func TestBuildRemapMaps_SkipsEmpty(t *testing.T) {
	maps, err := ebpf.BuildRemapMaps([]nat.RemapEntry{{Real: "", Virtual: "172.20.0.0/16"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(maps.Ingress) != 0 {
		t.Errorf("expected empty entries skipped, got %d", len(maps.Ingress))
	}
}
