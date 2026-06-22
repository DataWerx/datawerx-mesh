package edge_test

import (
	"testing"

	"github.com/DataWerx/datawerx-mesh/pkg/edge"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func TestValidateEdgeCIDR(t *testing.T) {
	tests := []struct {
		name     string
		cidr     string
		reserved []string
		wantErr  bool
	}{
		{"ok disjoint", "100.71.0.0/16", []string{"10.244.0.0/16", "10.96.0.0/12"}, false},
		{"empty", "", nil, true},
		{"malformed", "100.71.0.0/x", nil, true},
		{"default route", "0.0.0.0/0", nil, true},
		{"loopback", "127.0.0.0/8", nil, true},
		{"no host bits", "100.71.0.5/32", nil, true},
		{"overlaps local", "10.244.0.0/16", []string{"10.244.0.0/16"}, true},
		{"overlaps within local", "10.244.5.0/24", []string{"10.244.0.0/16"}, true},
		{"malformed reserved ignored", "100.71.0.0/16", []string{"", "garbage", "10.0.0.0/8"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := edge.ValidateEdgeCIDR(tc.cidr, tc.reserved)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateEdgeCIDR(%q, %v) err = %v, wantErr = %v", tc.cidr, tc.reserved, err, tc.wantErr)
			}
		})
	}
}

func TestGenerateKeypair(t *testing.T) {
	kp, err := edge.GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	if kp.PrivateKey == "" || kp.PublicKey == "" {
		t.Fatalf("empty keypair: %+v", kp)
	}
	if _, err := wgtypes.ParseKey(kp.PublicKey); err != nil {
		t.Errorf("public key does not parse: %v", err)
	}
	if _, err := wgtypes.ParseKey(kp.PrivateKey); err != nil {
		t.Errorf("private key does not parse: %v", err)
	}
	// A second keypair must differ (non-deterministic).
	kp2, _ := edge.GenerateKeypair()
	if kp2.PrivateKey == kp.PrivateKey {
		t.Errorf("two generated keypairs are identical")
	}
}
