package edge_test

import (
	"net"
	"testing"

	"github.com/DataWerx/datawerx-mesh/pkg/edge"
)

// FuzzDecode hammers the edge profile decoder with arbitrary input. An enrollment
// token may be handed around untrusted, so Decode must never panic, and any
// profile it accepts must be internally valid and round-trip identically.
func FuzzDecode(f *testing.F) {
	if p, err := edge.BuildDeviceProfile(sampleInput()); err == nil {
		if tok, err := p.Encode(); err == nil {
			f.Add(tok)
		}
	}
	for _, s := range []string{
		"", "dwxedge.v1.", "dwxedge.v1.@@@@", "dwxedge.v9.e30",
		"dwxmesh.v1.e30", "not-a-token", "   dwxedge.v1.e30   ",
	} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, token string) {
		p, err := edge.Decode(token)
		if err != nil {
			return // rejection is the common, fine case
		}
		if err := p.Validate(); err != nil {
			t.Fatalf("Decode accepted a token whose profile fails Validate: %v", err)
		}
		if p.DeviceID == "" || p.PublicKey == "" || p.PeerPublicKey == "" || p.EdgeEndpoint == "" {
			t.Fatalf("Decode accepted a profile missing identity: %+v", p)
		}
		// Rendering wg-quick must never panic.
		_ = p.WireGuardQuickConfig("")
		// Round-trip.
		tok, err := p.Encode()
		if err != nil {
			t.Fatalf("re-encoding a decoded profile failed: %v", err)
		}
		again, err := edge.Decode(tok)
		if err != nil {
			t.Fatalf("re-decoding failed: %v", err)
		}
		if again.Address != p.Address || again.DeviceID != p.DeviceID {
			t.Errorf("round-trip changed the profile: %+v -> %+v", p, again)
		}
	})
}

// FuzzAllocateDeviceIPs ensures the allocator never panics and that any mapping
// it returns assigns unique, in-range, non-reserved addresses.
func FuzzAllocateDeviceIPs(f *testing.F) {
	f.Add("100.71.0.0/24", "a\nb\nc")
	f.Add("fd00::/120", "x\ny")
	f.Add("not-a-cidr", "a")

	f.Fuzz(func(t *testing.T, cidr, keyBlob string) {
		var cl []edge.DeviceClaim
		for i, k := range splitNonEmpty(keyBlob) {
			if i >= 32 { // keep the fuzz bounded
				break
			}
			cl = append(cl, edge.DeviceClaim{Key: k})
		}
		m, err := edge.AllocateDeviceIPs(cidr, cl)
		if err != nil {
			return
		}
		_, ipnet, perr := net.ParseCIDR(cidr)
		if perr != nil {
			t.Fatalf("allocator accepted a CIDR that net.ParseCIDR rejects: %q", cidr)
		}
		seen := map[string]bool{}
		for k, v := range m {
			ip := net.ParseIP(v)
			if ip == nil || !ipnet.Contains(ip) || ip.Equal(ipnet.IP) {
				t.Fatalf("device %q got invalid address %q for %s", k, v, cidr)
			}
			if seen[v] {
				t.Fatalf("address %q assigned twice", v)
			}
			seen[v] = true
		}
	})
}

func splitNonEmpty(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
