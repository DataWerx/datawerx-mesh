// Package topology contains the pure, side-effect-free business logic of
// DataWerx Mesh. Given a MeshPeer specification and the local cluster's own
// network ranges, it computes the desired data-plane state - which CIDRs are
// safe to route, which conflict and need NAT remap - and the resulting phase.
//
// Nothing in this package touches the Kubernetes API, the kernel, or any
// network socket. That is deliberate: it is the layer that should carry the
// bulk of the unit-test coverage via plain table-driven tests, with no envtest
// or API server required. The reconciler is then a thin shell that calls
// PlanPeer and performs the resulting side effects.
package topology

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"strings"

	networkingv1alpha1 "github.com/DataWerx/datawerx-mesh/pkg/apis/networking/v1alpha1"
)

// Plan is the computed desired state for a single peer. It is the deterministic
// output of PlanPeer and is fully comparable in tests.
type Plan struct {
	// PublicKey is the WireGuard key the peer should be programmed with. Empty
	// means the spec was invalid and the peer cannot be programmed.
	PublicKey string
	// Endpoint is the remote host:port. May be empty for a roaming peer.
	Endpoint string
	// DesiredCIDRs is the full set of CIDRs the spec asked to route.
	DesiredCIDRs []string
	// RoutableCIDRs is the subset that can be safely programmed now.
	RoutableCIDRs []string
	// ConflictingCIDRs is the subset that overlaps a local range, or is
	// malformed, and is therefore withheld pending NAT remap.
	ConflictingCIDRs []string
	// Phase is the MeshPeer status phase implied by this plan.
	Phase networkingv1alpha1.MeshPeerPhase
	// Message is a human-readable summary of the plan.
	Message string
}

// Programmable reports whether the plan describes a peer that can actually be
// pushed into the data plane. A plan with no public key is not programmable.
func (p Plan) Programmable() bool {
	return p.PublicKey != ""
}

// HasConflicts reports whether any desired CIDR was withheld due to overlap.
func (p Plan) HasConflicts() bool {
	return len(p.ConflictingCIDRs) > 0
}

// PlanPeer computes the desired data-plane state for a single MeshPeer.
//
// Rules:
//   - A spec with no PublicKey is invalid: the returned plan is not
//     programmable and carries the Error phase.
//   - Each desired CIDR (pod + service) is checked against the local ranges.
//     A CIDR that overlaps any local range — or that fails to parse — is
//     withheld as a conflict; the rest are routable.
//   - If any conflict exists the phase is Error, The peer is reachable only
//     partially and needs the NAT-remap path; otherwise it is Connected.
func PlanPeer(spec networkingv1alpha1.MeshPeerSpec, localCIDRs []string) Plan {
	desired := spec.AllCIDRs()

	if spec.PublicKey == "" {
		return Plan{
			Endpoint:     spec.Endpoint,
			DesiredCIDRs: desired,
			Phase:        networkingv1alpha1.MeshPeerPhaseError,
			Message:      "spec.publicKey is required",
		}
	}

	routable, conflicts := Partition(desired, localCIDRs)

	plan := Plan{
		PublicKey:        spec.PublicKey,
		Endpoint:         spec.Endpoint,
		DesiredCIDRs:     desired,
		RoutableCIDRs:    routable,
		ConflictingCIDRs: conflicts,
	}

	if len(conflicts) > 0 {
		plan.Phase = networkingv1alpha1.MeshPeerPhaseError
		if dangerous := DangerousCIDRs(desired); len(dangerous) > 0 {
			plan.Message = fmt.Sprintf("refusing to route dangerous ranges %v (default/loopback/link-local/multicast are never steered into the mesh); unrouted: %v", dangerous, conflicts)
		} else {
			plan.Message = fmt.Sprintf("CIDR overlap with local cluster requires NAT remap; unrouted: %v", conflicts)
		}
	} else {
		plan.Phase = networkingv1alpha1.MeshPeerPhaseConnected
		plan.Message = fmt.Sprintf("peer programmed; %d/%d CIDRs routed", len(routable), len(desired))
	}
	return plan
}

// Partition splits the desired CIDRs into those safe to route and those that
// conflict with a local range or are malformed or dangerous. The relative
// order of the inputs is preserved in both outputs. Both slices are non-nil only
// when they contain elements, keeping zero-value comparisons in tests simple.
//
// A "dangerous" prefix - a default route, loopback, link-local, or multicast - is
// always withheld, independent of localCIDRs. This is a critical safety guard:
// without it, a MeshPeer advertising 0.0.0.0/0 (by mistake or malice) would make
// the agent install a default route into the mesh device and hijack ALL of the
// node's egress traffic — and since DataWerx_LOCAL_CIDRS is unset by default,
// the overlap check alone would not catch it.
func Partition(desired, localCIDRs []string) (routable, conflicts []string) {
	locals := ParseCIDRList(localCIDRs)
	for _, c := range desired {
		_, remote, err := net.ParseCIDR(c)
		if err != nil {
			// Malformed CIDRs are surfaced as conflicts rather than silently
			// dropped, so an operator notices the bad input.
			conflicts = append(conflicts, c)
			continue
		}
		if IsDangerousCIDR(remote) || OverlapsAny(remote, locals) {
			conflicts = append(conflicts, c)
			continue
		}
		routable = append(routable, c)
	}
	return routable, conflicts
}

// dangerousNets are prefixes that must never be steered into the mesh device:
// default routes which would hijack all node egress, loopback, link-local, and
// multicast. Routing any of these across a tunnel is never legitimate and can
// disrupt the node itself.
var dangerousNets = mustParseCIDRs(
	"127.0.0.0/8", "::1/128", // loopback
	"169.254.0.0/16", "fe80::/10", // link-local
	"224.0.0.0/4", "ff00::/8", // multicast
)

// IsDangerousCIDR reports whether n is a prefix that must never be routed into
// the mesh: a default route (/0), or one that overlaps a loopback, link-local,
// or multicast range.
func IsDangerousCIDR(n *net.IPNet) bool {
	if n == nil {
		return false
	}
	if ones, _ := n.Mask.Size(); ones == 0 {
		return true // default route (0.0.0.0/0 or ::/0)
	}
	return OverlapsAny(n, dangerousNets)
}

// DangerousCIDRs returns the subset of cidrs that are dangerous to route
// Malformed entries are ignored here.
func DangerousCIDRs(cidrs []string) []string {
	var out []string
	for _, c := range cidrs {
		if _, n, err := net.ParseCIDR(c); err == nil && IsDangerousCIDR(n) {
			out = append(out, c)
		}
	}
	return out
}

// mustParseCIDRs parses CIDR literals, panicking on a bad one.
func mustParseCIDRs(cidrs ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic(fmt.Sprintf("topology: bad built-in CIDR %q: %v", c, err))
		}
		out = append(out, n)
	}
	return out
}

// ParseCIDRList parses a slice of CIDR strings, silently skipping any that are
// malformed. It is used for the local-range set, where bad entries are an
// operator configuration problem handled elsewhere.
func ParseCIDRList(cidrs []string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		if _, ipnet, err := net.ParseCIDR(c); err == nil {
			out = append(out, ipnet)
		}
	}
	return out
}

// OverlapsAny reports whether candidate overlaps any of the supplied networks.
// Two prefixes overlap iff either contains the other's base address.
func OverlapsAny(candidate *net.IPNet, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(candidate.IP) || candidate.Contains(n.IP) {
			return true
		}
	}
	return false
}

// SanitizeName maps an arbitrary cluster ID into a DNS-1123 compliant object
// name suitable for use as a MeshPeer metadata.name. It lowercases the input,
// collapses every non-alphanumeric rune to '-', trims leading/trailing '-',
// and falls back to "peer" when nothing usable remains.
//
// Sanitization is lossy: "cluster_a", "cluster-a", and "Cluster.A" all collapse
// to "cluster-a", so distinct cluster IDs could silently clobber each other's
// MeshPeer/EndpointExport objects. To keep the mapping injective, whenever
// sanitization changed the identifier at all we append a short deterministic
// hash of the ORIGINAL id. IDs that are already DNS-safe keep clean names, and
// because the hash is a pure function of the input every cluster in the mesh
// still computes the identical name with no central allocator needed.
// maxSanitizedNameLen is the Kubernetes object-name limit (RFC 1123 subdomain).
// SanitizeName never emits a longer name, even for an arbitrarily long input.
const maxSanitizedNameLen = 253

func SanitizeName(id string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(id) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		// Nothing usable survived (e.g. "" or "!!!"). An empty cluster ID is
		// rejected upstream, so a fixed fallback is adequate here.
		return "peer"
	}
	if out == id && len(out) <= maxSanitizedNameLen {
		return out // already DNS-safe, lowercase, and short enough; no change needed.
	}
	// The input wasn't already a usable name (it was rewritten, or is longer than
	// Kubernetes allows), so disambiguate with a short hash of the original and
	// cap the whole thing at the object-name limit. An untrusted join bundle can
	// carry an arbitrarily long cluster ID; the name it produces must still be
	// a legal object name.
	sum := sha256.Sum256([]byte(id))
	suffix := "-" + hex.EncodeToString(sum[:])[:8]
	if maxBase := maxSanitizedNameLen - len(suffix); len(out) > maxBase {
		out = strings.TrimRight(out[:maxBase], "-")
	}
	return out + suffix
}
