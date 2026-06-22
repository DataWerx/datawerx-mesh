package edge_test

import (
	"testing"

	"github.com/DataWerx/datawerx-mesh/pkg/edge"
)

func TestPlanDevicePeer(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		address string
		wantIP  string
		wantErr bool
	}{
		{"bare ipv4", validKey, "100.71.0.5", "100.71.0.5/32", false},
		{"host cidr ipv4", validKey, "100.71.0.5/32", "100.71.0.5/32", false},
		{"bare ipv6", validKey, "fd00::5", "fd00::5/128", false},
		{"host cidr ipv6", validKey, "fd00::5/128", "fd00::5/128", false},
		{"bad key", "not-a-key", "100.71.0.5", "", true},
		{"subnet not host", validKey, "100.71.0.0/24", "", true},
		{"garbage address", validKey, "nope", "", true},
		{"empty address", validKey, "", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			peer, err := edge.PlanDevicePeer(tc.key, tc.address)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %s", tc.name)
				}
				return
			}
			if err != nil {
				t.Fatalf("PlanDevicePeer: %v", err)
			}
			if peer.PublicKey != tc.key {
				t.Errorf("PublicKey = %q, want %q", peer.PublicKey, tc.key)
			}
			if len(peer.AllowedIPs) != 1 || peer.AllowedIPs[0] != tc.wantIP {
				t.Errorf("AllowedIPs = %v, want [%s] (exactly the device's host route)", peer.AllowedIPs, tc.wantIP)
			}
		})
	}
}
