package edge_test

import (
	"strings"
	"testing"

	"github.com/DataWerx/datawerx-mesh/pkg/edge"
	"github.com/DataWerx/datawerx-mesh/pkg/gateway"
)

// validKey is a parseable (all-zero) 32-byte Curve25519 key in base64. validKey2
// is a distinct parseable key (all-0x01).
const (
	validKey  = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	validKey2 = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="
)

func sampleInput() edge.DeviceProfileInput {
	return edge.DeviceProfileInput{
		DeviceID:      "press-line-7",
		PublicKey:     validKey,
		Address:       "100.71.0.5",
		EdgeEndpoint:  "gw.example.com:51821",
		PeerPublicKey: validKey2,
		RouteCIDRs:    []string{"241.0.0.0/8", "10.96.0.0/12"},
		DNS:           gateway.DNSConfig{Addr: "241.0.0.10:53", SearchDomains: []string{"clusterset.local"}},
	}
}

func TestBuildDeviceProfile_NormalizesAndValidates(t *testing.T) {
	p, err := edge.BuildDeviceProfile(sampleInput())
	if err != nil {
		t.Fatalf("BuildDeviceProfile: %v", err)
	}
	if p.Address != "100.71.0.5/32" {
		t.Errorf("Address = %q, want host /32", p.Address)
	}
	if p.Version != edge.ProfileVersion {
		t.Errorf("Version = %q, want %q", p.Version, edge.ProfileVersion)
	}
	// RouteCIDRs come back sorted/deduped via gateway.BuildAccessProfile.
	want := []string{"10.96.0.0/12", "241.0.0.0/8"}
	if strings.Join(p.Access.RouteCIDRs, ",") != strings.Join(want, ",") {
		t.Errorf("RouteCIDRs = %v, want sorted %v", p.Access.RouteCIDRs, want)
	}
}

func TestBuildDeviceProfile_OrderIndependent(t *testing.T) {
	in1 := sampleInput()
	in2 := sampleInput()
	in2.RouteCIDRs = []string{"10.96.0.0/12", "241.0.0.0/8"} // reversed
	p1, err := edge.BuildDeviceProfile(in1)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := edge.BuildDeviceProfile(in2)
	if err != nil {
		t.Fatal(err)
	}
	t1, _ := p1.Encode()
	t2, _ := p2.Encode()
	if t1 != t2 {
		t.Errorf("route order changed the token:\n %s\n %s", t1, t2)
	}
}

func TestDeviceProfile_RoundTrip(t *testing.T) {
	p, err := edge.BuildDeviceProfile(sampleInput())
	if err != nil {
		t.Fatal(err)
	}
	tok, err := p.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !strings.HasPrefix(tok, edge.ProfileVersion+".") {
		t.Errorf("token %q missing %q prefix", tok, edge.ProfileVersion)
	}
	got, err := edge.Decode(tok)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.DeviceID != p.DeviceID || got.Address != p.Address || got.PeerPublicKey != p.PeerPublicKey {
		t.Errorf("round-trip mismatch: %+v vs %+v", got, p)
	}
}

func TestDecode_RejectsForeignAndGarbage(t *testing.T) {
	for _, tok := range []string{
		"",
		"dwxmesh.v1.e30", // a join bundle prefix, not ours
		"dwxedge.v1.@@@", // bad base64
		"dwxedge.v9.e30", // wrong version (prefix mismatch)
		"not-a-token",
	} {
		if _, err := edge.Decode(tok); err == nil {
			t.Errorf("Decode(%q) accepted a token it should reject", tok)
		}
	}
}

func TestValidate_RejectsBadFields(t *testing.T) {
	tests := []struct {
		name  string
		mutet func(*edge.DeviceProfileInput)
	}{
		{"empty deviceID", func(in *edge.DeviceProfileInput) { in.DeviceID = "" }},
		{"empty publicKey", func(in *edge.DeviceProfileInput) { in.PublicKey = "" }},
		{"bad publicKey", func(in *edge.DeviceProfileInput) { in.PublicKey = "not-a-key" }},
		{"bad peerPublicKey", func(in *edge.DeviceProfileInput) { in.PeerPublicKey = "nope" }},
		{"endpoint not host:port", func(in *edge.DeviceProfileInput) { in.EdgeEndpoint = "gw.example.com" }},
		{"address not a host", func(in *edge.DeviceProfileInput) { in.Address = "100.71.0.0/24" }},
		{"dangerous route", func(in *edge.DeviceProfileInput) { in.RouteCIDRs = []string{"0.0.0.0/0"} }},
		{"loopback route", func(in *edge.DeviceProfileInput) { in.RouteCIDRs = []string{"127.0.0.0/8"} }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := sampleInput()
			tc.mutet(&in)
			if _, err := edge.BuildDeviceProfile(in); err == nil {
				t.Errorf("BuildDeviceProfile accepted %s", tc.name)
			}
		})
	}
}

func TestWireGuardQuickConfig(t *testing.T) {
	p, err := edge.BuildDeviceProfile(sampleInput())
	if err != nil {
		t.Fatal(err)
	}

	// Placeholder when no private key is supplied.
	cfg := p.WireGuardQuickConfig("")
	for _, want := range []string{
		"[Interface]",
		"REPLACE_WITH_DEVICE_PRIVATE_KEY",
		"Address = 100.71.0.5/32",
		"DNS = 241.0.0.10", // host extracted from host:port
		"[Peer]",
		"PublicKey = " + validKey2,
		"Endpoint = gw.example.com:51821",
		"AllowedIPs = 10.96.0.0/12, 241.0.0.0/8",
		"PersistentKeepalive = 25",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("wg-quick config missing %q:\n%s", want, cfg)
		}
	}

	// Supplied private key is injected; the placeholder must be gone.
	cfg2 := p.WireGuardQuickConfig(validKey)
	if !strings.Contains(cfg2, "PrivateKey = "+validKey) {
		t.Errorf("private key not injected:\n%s", cfg2)
	}
	if strings.Contains(cfg2, "REPLACE_WITH_DEVICE_PRIVATE_KEY") {
		t.Errorf("placeholder still present with a real key:\n%s", cfg2)
	}
}
