// Package gateway implements the remote-access gateway role: the bridge that
// lets a remote client (a developer laptop on a shared overlay such as
// Tailscale, NetBird, or a corporate VPN) reach in-cluster ClusterSetIP VIPs and
// cross-cluster pod/service ranges as if it were on a VPN to the cluster.
//
// The gateway rides whatever encrypted transport the client already has — it
// does NOT own a WireGuard device or terminate tunnels itself — and adds only
// the two things that make the multi-cluster service layer reachable from
// outside:
//
//   - a masquerade so client-sourced traffic forwarded into the mesh returns via
//     the gateway (the rule planner lives in pkg/nat: BuildGatewayMasqRules), and
//   - a published "access profile" (a ConfigMap) describing where to connect,
//     which CIDRs to route at the gateway, and how to reach clusterset DNS — so a
//     thin client (a kubectl plugin, authenticating with the user's own
//     kubeconfig) can configure itself without any new control-plane service.
//
// As elsewhere in DataWerx, the decision logic here is pure and table-tested;
// the kernel/sysctl side effects (forward.go) are thin and separately covered.
package gateway

import (
	"encoding/json"
	"fmt"
	"sort"
)

const (
	// DefaultProfileNamespace is where the access-profile ConfigMap is published
	// when no namespace is configured.
	DefaultProfileNamespace = "datawerx-system"

	// ProfileConfigMapName is the fixed name of the access-profile ConfigMap. A
	// well-known name lets a client locate it with read-only RBAC and no
	// discovery dance.
	ProfileConfigMapName = "dwx-remote-access"

	// ProfileConfigMapKey is the ConfigMap data key the JSON profile is stored
	// under.
	ProfileConfigMapKey = "profile.json"
)

// DNSConfig tells a remote client how to reach the clusterset.local responder so
// services resolve by name (e.g. payments.prod.svc.clusterset.local).
type DNSConfig struct {
	// Addr is the host:port of the clusterset DNS responder, reachable over the
	// overlay (typically the gateway address and the dnsserver port).
	Addr string `json:"addr,omitempty"`
	// SearchDomains are the DNS zones the client should resolve via Addr (split
	// DNS), e.g. ["clusterset.local"].
	SearchDomains []string `json:"searchDomains,omitempty"`
}

// AccessProfile is the transport-neutral description a remote client needs to
// join. It is a plain value type (JSON-serializable, no Kubernetes machinery) so
// the same shape can later be served by a managed control plane unchanged.
type AccessProfile struct {
	// GatewayEndpoints are the overlay-reachable addresses of the gateway(s) the
	// client routes mesh traffic at.
	GatewayEndpoints []string `json:"gatewayEndpoints"`
	// RouteCIDRs are the destination ranges the client should route via the
	// gateway: the ClusterSetIP VIP ranges plus the reachable pod/service ranges.
	RouteCIDRs []string `json:"routeCIDRs"`
	// DNS describes clusterset name resolution. Optional.
	DNS DNSConfig `json:"dns,omitempty"`
}

// BuildAccessProfile assembles the deterministic profile a client consumes.
// gatewayEndpoints is the set of overlay addresses clients connect to;
// clusterSetCIDRs are the VIP ranges; meshCIDRs are the reachable pod/service
// ranges. RouteCIDRs is their de-duplicated, sorted union (empty entries
// dropped), so the output is stable regardless of input order — which keeps the
// published ConfigMap from churning on every reconcile.
func BuildAccessProfile(gatewayEndpoints, clusterSetCIDRs, meshCIDRs []string, dns DNSConfig) AccessProfile {
	routes := append(append([]string(nil), clusterSetCIDRs...), meshCIDRs...)
	dns.SearchDomains = dedupeSortedNonEmpty(dns.SearchDomains)
	return AccessProfile{
		GatewayEndpoints: dedupeSortedNonEmpty(gatewayEndpoints),
		RouteCIDRs:       dedupeSortedNonEmpty(routes),
		DNS:              dns,
	}
}

// ProfileConfigMapData renders a profile into the ConfigMap data map (the JSON
// payload under ProfileConfigMapKey). Marshaling a value type with only sorted
// slices is deterministic, so an unchanged profile yields byte-identical data
// and the upsert is a no-op.
func ProfileConfigMapData(p AccessProfile) (map[string]string, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("gateway: marshaling access profile: %w", err)
	}
	return map[string]string{ProfileConfigMapKey: string(b)}, nil
}

// DecodeProfile parses a JSON access profile. It is the inverse of the marshaling
// in ProfileConfigMapData and is shared by the client so both ends of the wire
// agree on exactly one format.
func DecodeProfile(b []byte) (AccessProfile, error) {
	var p AccessProfile
	if err := json.Unmarshal(b, &p); err != nil {
		return AccessProfile{}, fmt.Errorf("gateway: decoding access profile: %w", err)
	}
	return p, nil
}

// ProfileFromConfigMapData extracts and decodes the access profile from a
// ConfigMap data map. It returns a clear error when the well-known key is
// absent, so a client can distinguish "no profile published" from "malformed
// profile".
func ProfileFromConfigMapData(data map[string]string) (AccessProfile, error) {
	raw, ok := data[ProfileConfigMapKey]
	if !ok {
		return AccessProfile{}, fmt.Errorf("gateway: access profile ConfigMap is missing key %q", ProfileConfigMapKey)
	}
	return DecodeProfile([]byte(raw))
}

// dedupeSortedNonEmpty returns the non-empty inputs, de-duplicated and sorted,
// so profile fields are stable regardless of input order.
func dedupeSortedNonEmpty(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
