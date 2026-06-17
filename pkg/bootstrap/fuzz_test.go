package bootstrap_test

import (
	"testing"

	"github.com/datawerx/datawerx/pkg/bootstrap"
)

// validKey is a parseable (all-zero) 32-byte Curve25519 public key in base64.
const validKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

// FuzzDecode hammers the bundle decoder with arbitrary input. A join token comes
// from another cluster — untrusted — so Decode must never panic, and any bundle
// it accepts must be valid (non-empty identity, parseable key, host:port
// endpoint, safe CIDRs) and must re-encode to a token that decodes back to the
// same bundle.
func FuzzDecode(f *testing.F) {
	// A valid token to anchor the corpus.
	if b, err := bootstrap.NewBundle("cluster-a", validKey, "10.0.0.1:51820", []string{"10.244.0.0/16"}, nil); err == nil {
		if tok, err := b.Encode(); err == nil {
			f.Add(tok)
		}
	}
	for _, s := range []string{
		"", "dwxmesh.v1.", "dwxmesh.v1.@@@@", "dwxmesh.v9.AAAA",
		"not-a-bundle", "dwxmesh.v1." + "////", "   dwxmesh.v1.e30   ",
	} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, token string) {
		b, err := bootstrap.Decode(token)
		if err != nil {
			return // rejection is the common, fine case
		}
		// Accepted: it must be internally valid.
		if err := b.Validate(); err != nil {
			t.Fatalf("Decode accepted a token whose bundle fails Validate: %v", err)
		}
		if b.ClusterID == "" || b.PublicKey == "" || b.Endpoint == "" {
			t.Fatalf("Decode accepted a bundle missing identity: %+v", b)
		}
		// PeerObject must name it legally (no panic, non-empty name).
		if obj := b.PeerObject(); obj.Name == "" || len(obj.Name) > 253 {
			t.Fatalf("Decode produced an illegal peer name %q from %+v", obj.Name, b)
		}
		// Round-trip: a decoded bundle re-encodes and decodes back identically.
		tok, err := b.Encode()
		if err != nil {
			t.Fatalf("re-encoding a decoded bundle failed: %v", err)
		}
		again, err := bootstrap.Decode(tok)
		if err != nil {
			t.Fatalf("re-decoding a re-encoded bundle failed: %v", err)
		}
		if again.ClusterID != b.ClusterID || again.PublicKey != b.PublicKey || again.Endpoint != b.Endpoint {
			t.Errorf("round-trip changed the bundle: %+v -> %+v", b, again)
		}
	})
}
