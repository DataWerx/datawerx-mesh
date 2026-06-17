package routed_test

import (
	"net"
	"testing"

	"github.com/datawerx/datawerx/pkg/routed"
)

func cidr(t *testing.T, s string) net.IPNet {
	t.Helper()
	ip, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("ParseCIDR(%q): %v", s, err)
	}
	n.IP = ip // keep the host address, not just the network base
	return *n
}

func TestSelectOverlayIP_PrefersRequestedFamily(t *testing.T) {
	addrs := []net.IPNet{
		cidr(t, "fd00::5/64"),
		cidr(t, "100.64.0.5/10"),
	}
	got, err := routed.SelectOverlayIP(addrs, false) // prefer v4
	if err != nil || got != "100.64.0.5" {
		t.Fatalf("SelectOverlayIP(v4) = (%q, %v), want 100.64.0.5", got, err)
	}
	got, err = routed.SelectOverlayIP(addrs, true) // prefer v6
	if err != nil || got != "fd00::5" {
		t.Fatalf("SelectOverlayIP(v6) = (%q, %v), want fd00::5", got, err)
	}
}

func TestSelectOverlayIP_FallsBackAcrossFamily(t *testing.T) {
	addrs := []net.IPNet{cidr(t, "100.64.0.5/10")}
	got, err := routed.SelectOverlayIP(addrs, true) // prefer v6, only v4 present
	if err != nil || got != "100.64.0.5" {
		t.Fatalf("fallback = (%q, %v), want 100.64.0.5", got, err)
	}
}

func TestSelectOverlayIP_SkipsLoopbackAndLinkLocal(t *testing.T) {
	addrs := []net.IPNet{
		cidr(t, "127.0.0.1/8"),
		cidr(t, "169.254.1.1/16"),
		cidr(t, "fe80::1/64"),
		cidr(t, "100.64.0.7/10"),
	}
	got, err := routed.SelectOverlayIP(addrs, false)
	if err != nil || got != "100.64.0.7" {
		t.Fatalf("SelectOverlayIP = (%q, %v), want 100.64.0.7 (skipping loopback/link-local)", got, err)
	}
}

func TestSelectOverlayIP_NoneUsable(t *testing.T) {
	addrs := []net.IPNet{cidr(t, "127.0.0.1/8"), cidr(t, "fe80::1/64")}
	if _, err := routed.SelectOverlayIP(addrs, false); err == nil {
		t.Error("expected error when no global-unicast address is present")
	}
}
