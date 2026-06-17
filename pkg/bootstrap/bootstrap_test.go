package bootstrap_test

import (
	"strings"
	"testing"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	networkingv1alpha1 "github.com/datawerx/datawerx/pkg/apis/networking/v1alpha1"
	"github.com/datawerx/datawerx/pkg/bootstrap"
)

// testKey returns a valid WireGuard public key for use in bundles.
func testKey(t *testing.T) string {
	t.Helper()
	k, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	return k.PublicKey().String()
}

func TestEncodeDecode_RoundTrip(t *testing.T) {
	b, err := bootstrap.NewBundle("cluster-a", testKey(t), "a.example.com:51820",
		[]string{"10.10.0.0/16"}, []string{"10.96.0.0/12"})
	if err != nil {
		t.Fatalf("NewBundle: %v", err)
	}

	token, err := b.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !strings.HasPrefix(token, bootstrap.BundleVersion+".") {
		t.Errorf("token missing version prefix: %q", token)
	}

	got, err := bootstrap.Decode(token)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.ClusterID != b.ClusterID || got.PublicKey != b.PublicKey || got.Endpoint != b.Endpoint {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got, b)
	}
}

func TestEncode_DeterministicRegardlessOfCIDROrder(t *testing.T) {
	key := testKey(t)
	b1, _ := bootstrap.NewBundle("c", key, "h:1", []string{"10.1.0.0/16", "10.2.0.0/16"}, nil)
	b2, _ := bootstrap.NewBundle("c", key, "h:1", []string{"10.2.0.0/16", "10.1.0.0/16"}, nil)
	t1, _ := b1.Encode()
	t2, _ := b2.Encode()
	if t1 != t2 {
		t.Errorf("bundle encoding should be order-independent:\n%s\n%s", t1, t2)
	}
}

func TestDecode_RejectsForeignToken(t *testing.T) {
	for _, tok := range []string{"", "hello", "dwxmesh.v0.abc", "notaprefix.xxxx"} {
		if _, err := bootstrap.Decode(tok); err == nil {
			t.Errorf("Decode(%q) should have failed", tok)
		}
	}
}

func TestValidate_RejectsBadFields(t *testing.T) {
	good := testKey(t)
	tests := []struct {
		name              string
		id, key, endpoint string
		pod               []string
	}{
		{"missing id", "", good, "h:1", nil},
		{"missing key", "c", "", "h:1", nil},
		{"bad key", "c", "not-a-key", "h:1", nil},
		{"missing endpoint", "c", good, "", nil},
		{"endpoint not host:port", "c", good, "justhost", nil},
		{"malformed cidr", "c", good, "h:1", []string{"10.0.0/33"}},
		{"dangerous cidr", "c", good, "h:1", []string{"0.0.0.0/0"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := bootstrap.NewBundle(tc.id, tc.key, tc.endpoint, tc.pod, nil); err == nil {
				t.Errorf("expected validation error for %s", tc.name)
			}
		})
	}
}

func TestPeerObject_DeterministicNameAndLabel(t *testing.T) {
	b, err := bootstrap.NewBundle("Cluster.A", testKey(t), "a:51820", []string{"10.10.0.0/16"}, nil)
	if err != nil {
		t.Fatalf("NewBundle: %v", err)
	}
	obj := b.PeerObject()
	if obj.Name == "" {
		t.Fatal("peer object has no name")
	}
	if obj.Labels[bootstrap.ManagedByLabel] != bootstrap.ManagedByJoin {
		t.Errorf("missing join managed-by label: %v", obj.Labels)
	}
	// Same bundle → same object name (idempotent upsert).
	if again := b.PeerObject(); again.Name != obj.Name {
		t.Errorf("peer name not deterministic: %q vs %q", obj.Name, again.Name)
	}
	if obj.Spec.ClusterID != "Cluster.A" || obj.Spec.PublicKey != b.PublicKey {
		t.Errorf("peer spec not projected from bundle: %+v", obj.Spec)
	}
	// TypeMeta is set so `dwxctl join import --dry-run` emits a self-describing
	// manifest that pipes into `kubectl apply -f -`.
	if obj.Kind != "MeshPeer" || obj.APIVersion != networkingv1alpha1.GroupVersion.String() {
		t.Errorf("peer object missing TypeMeta: apiVersion=%q kind=%q", obj.APIVersion, obj.Kind)
	}
}

func TestGenerateKeypair_ProducesParseableKeys(t *testing.T) {
	kp, err := bootstrap.GenerateKeypair()
	if err != nil {
		t.Fatalf("GenerateKeypair: %v", err)
	}
	if _, err := wgtypes.ParseKey(kp.PrivateKey); err != nil {
		t.Errorf("private key not parseable: %v", err)
	}
	if _, err := wgtypes.ParseKey(kp.PublicKey); err != nil {
		t.Errorf("public key not parseable: %v", err)
	}
	if kp.PrivateKey == kp.PublicKey {
		t.Errorf("private and public key should differ")
	}
}
