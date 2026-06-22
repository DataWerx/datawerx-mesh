package edge

import (
	"fmt"
	"net"
	"strings"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/DataWerx/datawerx-mesh/pkg/topology"
)

// Keypair is a freshly generated WireGuard keypair for a device. The private key
// must be stored on the device and never uploaded; only the public key goes into
// the EdgeDevice CRD and the profile. Mirrors bootstrap.Keypair.
type Keypair struct {
	PrivateKey string
	PublicKey  string
}

// GenerateKeypair mints a new Curve25519 WireGuard keypair for a device. It is
// the only non-deterministic function in the package; it is crypto-only (no
// socket) so it belongs here rather than in the kernel-touching pkg/wg. The CLI's
// `--generate` path calls this device-side and prints the private key to stderr.
func GenerateKeypair() (Keypair, error) {
	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		return Keypair{}, fmt.Errorf("generating WireGuard key: %w", err)
	}
	return Keypair{PrivateKey: key.String(), PublicKey: key.PublicKey().String()}, nil
}

// ValidateEdgeCIDR screens the edge address pool at terminator startup. It must
// be a well-formed range, never one that is unsafe to route (default route,
// loopback, link-local, multicast), and must not overlap any reserved range
// (the node's local pod/service CIDRs or any peer CIDR) — otherwise the edge
// masquerade scope would collide with mesh traffic. Malformed reserved entries
// are ignored so a single bad input can't wedge startup; the agent fails closed
// on the edge CIDR itself.
func ValidateEdgeCIDR(cidr string, reserved []string) error {
	cidr = strings.TrimSpace(cidr)
	if cidr == "" {
		return fmt.Errorf("edge CIDR is empty")
	}
	_, n, err := net.ParseCIDR(cidr)
	if err != nil {
		return fmt.Errorf("edge CIDR %q is malformed: %w", cidr, err)
	}
	if topology.IsDangerousCIDR(n) {
		return fmt.Errorf("edge CIDR %q is never safe to route into the mesh", cidr)
	}
	ones, bits := n.Mask.Size()
	if ones >= bits {
		return fmt.Errorf("edge CIDR %q has no host addresses to assign", cidr)
	}

	var reservedNets []*net.IPNet
	for _, r := range reserved {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if _, rn, err := net.ParseCIDR(r); err == nil {
			reservedNets = append(reservedNets, rn)
		}
	}
	if topology.OverlapsAny(n, reservedNets) {
		return fmt.Errorf("edge CIDR %q overlaps a local or peer range (must be disjoint)", cidr)
	}
	return nil
}
