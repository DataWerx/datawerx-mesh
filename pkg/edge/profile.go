// Package edge is the pure planner behind the edge device connector. It lets a
// single non-Kubernetes device such as IoT device, factory gateway, VM, or a
// developer's laptop reach mesh services by name over a WireGuard tunnel the
// device dials outbound to a dedicated edge-ingress terminator, separate from
// the node-to-node mesh data plane.
//
// Everything in this package is pure and deterministic — device-IP allocation,
// the terminator-side peer plan, the device-side profile/wg-quick rendering, and
// the enrollment-token codec. It is exhaustively table-testable with no
// cluster and no kernel. It is the reusable contract half of the connector: the
// open-core build ships it, alongside the EdgeDevice CRD, as the tier-agnostic
// integration point, while the managed terminator and reconciler that consume it
// are wired in only via pkg/agent.Options.RegisterPremium. The capability of edge
// reach also remains free via the BYO-overlay + gateway role; this package is the
// shared decision logic both paths build on.
//
// The device's access transport tunnel on `dwx-edge0` is decoupled from the
// mesh's internal transport, so the device-side setup is byte-identical whether
// the cluster runs native WireGuard or a bring-your-own overlay.
package edge

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"strings"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/DataWerx/datawerx-mesh/pkg/gateway"
	"github.com/DataWerx/datawerx-mesh/pkg/topology"
)

// ProfileVersion is the schema version of an edge enrollment token. It prefixes
// the token so a consumer rejects a foreign format (e.g. a `dwx mesh join` bundle)
// instead of decoding garbage. The shape mirrors bootstrap's `dwxmesh.v1` token
// but the distinct prefix keeps the two enrollment surfaces from cross-decoding.
const ProfileVersion = "dwxedge.v1"

// ManagedByLabel marks EdgeDevices and the value the free `dwx edge` path
// stamps on them, distinguishing manually-enrolled devices from control-plane-
// materialized ones for later cleanup/audit.
const (
	ManagedByLabel = "app.kubernetes.io/managed-by"
	// ManagedByEdge is the value ManagedByLabel carries on `dwx edge`-authored
	// EdgeDevices, mirroring bootstrap.ManagedByJoin.
	ManagedByEdge = "dwxctl-edge"
)

// DeviceProfile is the device-side artifact handed to an enrolled device: enough
// to configure stock `wg-quick` and reach the mesh, and nothing secret. The
// device's private key is generated on the device and never appears here — the
// profile carries only the device's public key (its shareable identity), the
// terminator's public key for the [Peer] block, the assigned tunnel address, the
// edge endpoint to dial, and the reachable routes/DNS (the reused
// gateway.AccessProfile).
//
// It is JSON-serializable with no Kubernetes machinery so the same shape can be
// rendered by the free CLI or served by the managed control plane unchanged, and
// it round-trips through a single shareable `dwxedge.v1` token.
type DeviceProfile struct {
	Version string `json:"version"`
	// DeviceID is the stable, human-facing identity of the device.
	DeviceID string `json:"deviceID"`
	// PublicKey is the device's own Curve25519 WireGuard public key (its identity).
	PublicKey string `json:"publicKey"`
	// Address is the device's assigned tunnel address rendered as a host CIDR
	// (/32 for IPv4, /128 for IPv6) — the device's interface address.
	Address string `json:"address"`
	// EdgeEndpoint is the public host:port of the edge-ingress terminator
	// (`dwx-edge0`) the device dials outbound.
	EdgeEndpoint string `json:"edgeEndpoint"`
	// PeerPublicKey is the terminator's WireGuard public key, the device's [Peer].
	PeerPublicKey string `json:"peerPublicKey"`
	// Access carries the reachable route CIDRs and clusterset DNS the device
	// routes through the tunnel — the gateway access profile, reused unchanged.
	Access gateway.AccessProfile `json:"access"`
}

// DeviceProfileInput is the set of resolved values BuildDeviceProfile assembles
// into a device profile. The caller (the terminator/reconciler or the CLI) has
// already resolved the assigned address and the reachable routes.
type DeviceProfileInput struct {
	DeviceID      string
	PublicKey     string
	Address       string // assigned host address (bare IP or host CIDR)
	EdgeEndpoint  string
	PeerPublicKey string
	RouteCIDRs    []string // clusterset VIP ranges plus reachable mesh CIDRs
	DNS           gateway.DNSConfig
}

// BuildDeviceProfile assembles and validates a device profile. The route set is
// de-duplicated and sorted (via gateway.BuildAccessProfile) so a given device's
// profile encodes identically regardless of input order, keeping the published
// artifact stable.
func BuildDeviceProfile(in DeviceProfileInput) (DeviceProfile, error) {
	addr, err := hostCIDR(strings.TrimSpace(in.Address))
	if err != nil {
		return DeviceProfile{}, fmt.Errorf("device address: %w", err)
	}
	p := DeviceProfile{
		Version:       ProfileVersion,
		DeviceID:      strings.TrimSpace(in.DeviceID),
		PublicKey:     strings.TrimSpace(in.PublicKey),
		Address:       addr,
		EdgeEndpoint:  strings.TrimSpace(in.EdgeEndpoint),
		PeerPublicKey: strings.TrimSpace(in.PeerPublicKey),
		// GatewayEndpoints is set to the edge endpoint so the profile is
		// self-describing; RouteCIDRs is the device's reachable set.
		Access: gateway.BuildAccessProfile([]string{strings.TrimSpace(in.EdgeEndpoint)}, in.RouteCIDRs, nil, in.DNS),
	}
	if err := p.Validate(); err != nil {
		return DeviceProfile{}, err
	}
	return p, nil
}

// Validate checks a profile is complete and well-formed: identity fields are
// present, both public keys parse as WireGuard keys, the edge endpoint is a
// host:port, the address parses as a single host, and every advertised route is
// safe to carry into the mesh.
func (p DeviceProfile) Validate() error {
	if p.Version != ProfileVersion {
		return fmt.Errorf("unsupported edge profile version %q (want %q)", p.Version, ProfileVersion)
	}
	if p.DeviceID == "" {
		return fmt.Errorf("edge profile is missing deviceID")
	}
	if p.PublicKey == "" {
		return fmt.Errorf("edge profile is missing publicKey")
	}
	if _, err := wgtypes.ParseKey(p.PublicKey); err != nil {
		return fmt.Errorf("edge profile publicKey is not a valid WireGuard key: %w", err)
	}
	if p.PeerPublicKey == "" {
		return fmt.Errorf("edge profile is missing peerPublicKey")
	}
	if _, err := wgtypes.ParseKey(p.PeerPublicKey); err != nil {
		return fmt.Errorf("edge profile peerPublicKey is not a valid WireGuard key: %w", err)
	}
	if p.EdgeEndpoint == "" {
		return fmt.Errorf("edge profile is missing edgeEndpoint")
	}
	if _, _, err := net.SplitHostPort(p.EdgeEndpoint); err != nil {
		return fmt.Errorf("edge profile edgeEndpoint %q is not host:port: %w", p.EdgeEndpoint, err)
	}
	if _, err := hostCIDR(p.Address); err != nil {
		return fmt.Errorf("edge profile address %q: %w", p.Address, err)
	}
	for _, c := range p.Access.RouteCIDRs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			return fmt.Errorf("edge profile advertises malformed route CIDR %q: %w", c, err)
		}
		if topology.IsDangerousCIDR(n) {
			return fmt.Errorf("edge profile advertises route CIDR %q which is never safe to route into the mesh", c)
		}
	}
	return nil
}

// Encode renders the profile as a single shareable token: a version tag, a dot,
// and the base64url-encoded JSON — the same discipline as a join bundle.
func (p DeviceProfile) Encode() (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("encoding edge profile: %w", err)
	}
	return ProfileVersion + "." + base64.RawURLEncoding.EncodeToString(raw), nil
}

// Decode parses a token produced by Encode back into a validated DeviceProfile.
func Decode(token string) (DeviceProfile, error) {
	token = strings.TrimSpace(token)
	prefix := ProfileVersion + "."
	if !strings.HasPrefix(token, prefix) {
		return DeviceProfile{}, fmt.Errorf("token is not a %s edge profile", ProfileVersion)
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(token, prefix))
	if err != nil {
		return DeviceProfile{}, fmt.Errorf("decoding edge profile token: %w", err)
	}
	var p DeviceProfile
	if err := json.Unmarshal(raw, &p); err != nil {
		return DeviceProfile{}, fmt.Errorf("parsing edge profile JSON: %w", err)
	}
	if err := p.Validate(); err != nil {
		return DeviceProfile{}, err
	}
	return p, nil
}

// privateKeyPlaceholder is emitted into the rendered wg-quick config when no
// private key is supplied, so the operator can hand the file to the device and
// the device (or admin) fills in the key generated on the device.
const privateKeyPlaceholder = "REPLACE_WITH_DEVICE_PRIVATE_KEY"

// WireGuardQuickConfig renders the device-side `wg-quick` configuration. The
// private key is supplied separately (never stored in the profile, CRD, or
// token); pass the empty string to emit a placeholder for the operator to fill
// in. The output is deterministic so it is safe to diff and re-render.
func (p DeviceProfile) WireGuardQuickConfig(privateKey string) string {
	privateKey = strings.TrimSpace(privateKey)
	if privateKey == "" {
		privateKey = privateKeyPlaceholder
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# DataWerx Mesh edge device: %s\n", p.DeviceID)
	b.WriteString("[Interface]\n")
	fmt.Fprintf(&b, "PrivateKey = %s\n", privateKey)
	fmt.Fprintf(&b, "Address = %s\n", p.Address)
	if dns := dnsResolver(p.Access.DNS.Addr); dns != "" {
		fmt.Fprintf(&b, "DNS = %s\n", dns)
	}
	b.WriteString("\n[Peer]\n")
	fmt.Fprintf(&b, "PublicKey = %s\n", p.PeerPublicKey)
	fmt.Fprintf(&b, "Endpoint = %s\n", p.EdgeEndpoint)
	if len(p.Access.RouteCIDRs) > 0 {
		fmt.Fprintf(&b, "AllowedIPs = %s\n", strings.Join(p.Access.RouteCIDRs, ", "))
	}
	// The keepalive holds the NAT pinhole open so the mesh-side terminator can
	// reach a device that sits behind NAT/CGNAT with no inbound ports.
	b.WriteString("PersistentKeepalive = 25\n")
	return b.String()
}

// dnsResolver extracts the resolver host from a host:port clusterset DNS address.
// wg-quick's DNS directive takes a resolver address, not host:port; the search
// domains travel in the profile's Access.DNS for clients that support split DNS.
func dnsResolver(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

// hostCIDR normalizes a single host address (a bare IP or a host CIDR) into the
// canonical host-CIDR form: /32 for IPv4, /128 for IPv6. It rejects anything that
// is not exactly one host (a non-host-bit mask, an empty string, or garbage).
func hostCIDR(addr string) (string, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", fmt.Errorf("address is empty")
	}
	if !strings.Contains(addr, "/") {
		ip := net.ParseIP(addr)
		if ip == nil {
			return "", fmt.Errorf("%q is not an IP address", addr)
		}
		return ip.String() + hostMaskSuffix(ip), nil
	}
	ip, ipnet, err := net.ParseCIDR(addr)
	if err != nil {
		return "", fmt.Errorf("%q is not a valid address: %w", addr, err)
	}
	ones, bits := ipnet.Mask.Size()
	if ones != bits {
		return "", fmt.Errorf("%q is not a single host address (want a /%d host route)", addr, bits)
	}
	return ip.String() + hostMaskSuffix(ip), nil
}

// hostMaskSuffix returns the host-route suffix for an IP: "/32" for IPv4, "/128"
// for IPv6.
func hostMaskSuffix(ip net.IP) string {
	if ip.To4() != nil {
		return "/32"
	}
	return "/128"
}
