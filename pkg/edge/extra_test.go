package edge_test

import (
	"strings"
	"testing"

	"github.com/DataWerx/datawerx-mesh/pkg/edge"
	"github.com/DataWerx/datawerx-mesh/pkg/gateway"
)

// TestValidate_MalformedRoute hits the route-parse (not dangerous) branch: a
// route string that survives the profile's dedupe but is not a CIDR at all.
func TestValidate_MalformedRoute(t *testing.T) {
	in := sampleInput()
	in.RouteCIDRs = []string{"not-a-cidr"}
	if _, err := edge.BuildDeviceProfile(in); err == nil {
		t.Fatal("BuildDeviceProfile accepted a malformed route CIDR")
	}
}

// TestEncode_RejectsInvalidProfile covers Encode's own Validate guard for a
// hand-built (not BuildDeviceProfile-vetted) profile.
func TestEncode_RejectsInvalidProfile(t *testing.T) {
	p := edge.DeviceProfile{Version: edge.ProfileVersion} // missing identity
	if _, err := p.Encode(); err == nil {
		t.Fatal("Encode accepted an invalid profile")
	}
}

// TestWireGuardQuickConfig_BareDNS covers the dnsResolver fallback when the DNS
// address carries no port.
func TestWireGuardQuickConfig_BareDNS(t *testing.T) {
	in := sampleInput()
	in.DNS = gateway.DNSConfig{Addr: "241.0.0.10"} // no :port
	p, err := edge.BuildDeviceProfile(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.WireGuardQuickConfig(""), "DNS = 241.0.0.10") {
		t.Errorf("bare DNS address not rendered:\n%s", p.WireGuardQuickConfig(""))
	}
}

// TestWireGuardQuickConfig_NoDNS covers omitting the DNS line entirely.
func TestWireGuardQuickConfig_NoDNS(t *testing.T) {
	in := sampleInput()
	in.DNS = gateway.DNSConfig{}
	p, err := edge.BuildDeviceProfile(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p.WireGuardQuickConfig(""), "DNS =") {
		t.Errorf("DNS line emitted with no DNS configured:\n%s", p.WireGuardQuickConfig(""))
	}
}

// TestAllocateDeviceIPs_MalformedPin covers the pin-parse error branch, and uses
// a long key to exercise key truncation in the error message.
func TestAllocateDeviceIPs_MalformedPin(t *testing.T) {
	longKey := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	in := []edge.DeviceClaim{{Key: longKey, Address: "garbage"}}
	if _, err := edge.AllocateDeviceIPs("100.71.0.0/24", in); err == nil {
		t.Fatal("AllocateDeviceIPs accepted a malformed pin")
	}
}
