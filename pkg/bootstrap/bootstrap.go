// Package bootstrap is the pure planner behind `dwxctl join`. It turns the
// hand-authored "write a MeshPeer and swap WireGuard keys" routine into a single
// exchangeable bundle. One cluster mints a bundle describing itself; the other
// consumes it and authors the reciprocal MeshPeer. Doing it twice - once each
// way - forms a mesh with no hand-written CRDs.
//
// Everything here is pure and deterministic — bundle encode/decode, validation,
// and MeshPeer authoring — so it is exhaustively table-testable with no cluster.
// The CLI is a thin shell that gathers inputs, calls this, and applies the
// resulting object. Key generation is the one non-deterministic helper. It is
// crypto-only (no socket) so it still belongs here rather than in the
// kernel-touching pkg/wg.
package bootstrap

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strings"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	networkingv1alpha1 "github.com/datawerx/datawerx/pkg/apis/networking/v1alpha1"
	"github.com/datawerx/datawerx/pkg/topology"
)

const (
	// BundleVersion is the schema version of a join bundle. It prefixes the token
	// so a consumer rejects a format it does not understand instead of decoding
	// garbage.
	BundleVersion = "dwxmesh.v1"

	// ManagedByLabel marks MeshPeers authored by `dwxctl join`, distinguishing
	// them from GitOps- or syncer-authored peers for later cleanup/audit.
	ManagedByLabel = "app.kubernetes.io/managed-by"
	// ManagedByJoin is the value ManagedByLabel carries on join-authored peers.
	ManagedByJoin = "dwxctl-join"
)

// Bundle is one cluster's self-description, exchanged to bootstrap a peering. It
// carries exactly what the other side needs to author a MeshPeer for this
// cluster — and nothing secret: the public key is shareable by design, and no
// private key is ever placed in a bundle.
type Bundle struct {
	Version      string   `json:"version"`
	ClusterID    string   `json:"clusterID"`
	PublicKey    string   `json:"publicKey"`
	Endpoint     string   `json:"endpoint"`
	PodCIDRs     []string `json:"podCIDRs,omitempty"`
	ServiceCIDRs []string `json:"serviceCIDRs,omitempty"`
}

// Keypair is a freshly generated WireGuard keypair. The private key must be
// stored as the node's DataWerx_WG_PRIVATE_KEY secret; only the public key goes
// into a bundle.
type Keypair struct {
	PrivateKey string
	PublicKey  string
}

// GenerateKeypair mints a new Curve25519 WireGuard keypair. It is the only
// non-deterministic function in the package; callers that already have a node
// key skip it and pass their existing public key into NewBundle.
func GenerateKeypair() (Keypair, error) {
	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		return Keypair{}, fmt.Errorf("generating WireGuard key: %w", err)
	}
	return Keypair{PrivateKey: key.String(), PublicKey: key.PublicKey().String()}, nil
}

// NewBundle assembles and validates a bundle describing the local cluster.
func NewBundle(clusterID, publicKey, endpoint string, podCIDRs, serviceCIDRs []string) (Bundle, error) {
	b := Bundle{
		Version:      BundleVersion,
		ClusterID:    strings.TrimSpace(clusterID),
		PublicKey:    strings.TrimSpace(publicKey),
		Endpoint:     strings.TrimSpace(endpoint),
		PodCIDRs:     normalizeCIDRs(podCIDRs),
		ServiceCIDRs: normalizeCIDRs(serviceCIDRs),
	}
	if err := b.Validate(); err != nil {
		return Bundle{}, err
	}
	return b, nil
}

// Validate checks a bundle is complete and well-formed: the identity fields are
// present, the public key parses as a WireGuard key, the endpoint is a host:port,
// and every advertised CIDR parses and is safe to route.
func (b Bundle) Validate() error {
	if b.Version != BundleVersion {
		return fmt.Errorf("unsupported bundle version %q (want %q)", b.Version, BundleVersion)
	}
	if b.ClusterID == "" {
		return fmt.Errorf("bundle is missing clusterID")
	}
	if b.PublicKey == "" {
		return fmt.Errorf("bundle is missing publicKey")
	}
	if _, err := wgtypes.ParseKey(b.PublicKey); err != nil {
		return fmt.Errorf("bundle publicKey is not a valid WireGuard key: %w", err)
	}
	if b.Endpoint == "" {
		return fmt.Errorf("bundle is missing endpoint")
	}
	if _, _, err := net.SplitHostPort(b.Endpoint); err != nil {
		return fmt.Errorf("bundle endpoint %q is not host:port: %w", b.Endpoint, err)
	}
	for _, c := range b.allCIDRs() {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			return fmt.Errorf("bundle advertises malformed CIDR %q: %w", c, err)
		}
		if topology.IsDangerousCIDR(n) {
			return fmt.Errorf("bundle advertises CIDR %q which is never safe to route into the mesh", c)
		}
	}
	return nil
}

// Encode renders the bundle as a single shareable token: a version tag, a dot,
// and the base64url-encoded JSON. The tag lets a human eyeball what it is and a
// decoder reject a foreign format early.
func (b Bundle) Encode() (string, error) {
	if err := b.Validate(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return "", fmt.Errorf("encoding bundle: %w", err)
	}
	return BundleVersion + "." + base64.RawURLEncoding.EncodeToString(raw), nil
}

// Decode parses a token produced by Encode back into a validated Bundle.
func Decode(token string) (Bundle, error) {
	token = strings.TrimSpace(token)
	prefix := BundleVersion + "."
	if !strings.HasPrefix(token, prefix) {
		return Bundle{}, fmt.Errorf("token is not a %s join bundle", BundleVersion)
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(token, prefix))
	if err != nil {
		return Bundle{}, fmt.Errorf("decoding bundle token: %w", err)
	}
	var b Bundle
	if err := json.Unmarshal(raw, &b); err != nil {
		return Bundle{}, fmt.Errorf("parsing bundle JSON: %w", err)
	}
	if err := b.Validate(); err != nil {
		return Bundle{}, err
	}
	return b, nil
}

// PeerSpec projects a remote cluster's bundle onto the MeshPeerSpec the local
// cluster should apply to reach it.
func (b Bundle) PeerSpec() networkingv1alpha1.MeshPeerSpec {
	return networkingv1alpha1.MeshPeerSpec{
		ClusterID:    b.ClusterID,
		PublicKey:    b.PublicKey,
		Endpoint:     b.Endpoint,
		PodCIDRs:     append([]string(nil), b.PodCIDRs...),
		ServiceCIDRs: append([]string(nil), b.ServiceCIDRs...),
	}
}

// PeerObject builds the full MeshPeer object to apply for a remote cluster,
// naming it deterministically from the cluster ID. Re-importing the same
// bundle is an idempotent upsert, and tagging it as join-authored. The TypeMeta
// is set so the object rendered by `dwxctl join import --dry-run` is a valid,
// self-describing manifest a user can pipe straight into `kubectl apply -f -`.
func (b Bundle) PeerObject() *networkingv1alpha1.MeshPeer {
	return &networkingv1alpha1.MeshPeer{
		TypeMeta: metav1.TypeMeta{
			APIVersion: networkingv1alpha1.GroupVersion.String(),
			Kind:       "MeshPeer",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:   topology.SanitizeName(b.ClusterID),
			Labels: map[string]string{ManagedByLabel: ManagedByJoin},
		},
		Spec: b.PeerSpec(),
	}
}

func (b Bundle) allCIDRs() []string {
	out := make([]string, 0, len(b.PodCIDRs)+len(b.ServiceCIDRs))
	out = append(out, b.PodCIDRs...)
	out = append(out, b.ServiceCIDRs...)
	return out
}

// normalizeCIDRs trims, drops empties, sorts, and dedupes so a bundle for a
// given set of ranges encodes identically regardless of input order.
func normalizeCIDRs(in []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, c := range in {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if _, ok := seen[c]; ok {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}
