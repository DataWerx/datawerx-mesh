package edge

import (
	"fmt"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// DevicePeer is the terminator-side description of a device peer: the device's
// public key and the single host AllowedIP it is permitted to source from. The
// terminator programs exactly this onto `dwx-edge0` (an addressless device,
// learning the device's real endpoint from the inbound handshake), so a device
// cannot spoof another device's source address — authorization is the WireGuard
// AllowedIPs ACL, cryptographically enforced.
type DevicePeer struct {
	// PublicKey is the device's Curve25519 WireGuard public key.
	PublicKey string
	// AllowedIPs is the device's assigned host route (a single /32 or /128). It
	// is deliberately just the device's own address — the reachable mesh is
	// granted by the gateway masquerade and routes, not by widening this ACL.
	AllowedIPs []string
}

// PlanDevicePeer computes the terminator-side peer config for a device from its
// public key and resolved tunnel address. The address may be a bare IP or a host
// CIDR; it is normalized to the canonical host route. It is a pure function of
// its inputs so the reconciler stays a thin shell over it.
func PlanDevicePeer(publicKey, address string) (DevicePeer, error) {
	if _, err := wgtypes.ParseKey(publicKey); err != nil {
		return DevicePeer{}, fmt.Errorf("device publicKey is not a valid WireGuard key: %w", err)
	}
	host, err := hostCIDR(address)
	if err != nil {
		return DevicePeer{}, fmt.Errorf("device address: %w", err)
	}
	return DevicePeer{
		PublicKey:  publicKey,
		AllowedIPs: []string{host},
	}, nil
}
