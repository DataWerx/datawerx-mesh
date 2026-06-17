package topology

import (
	"fmt"
	"net"
	"sort"
)

// PeerIdentity is the minimal, transport-neutral view of a remote peer that
// topology-level validation needs. The syncer projects each control-plane peer
// config into this shape so conflict detection stays a pure function with no
// dependency on the client package or Kubernetes types.
type PeerIdentity struct {
	ClusterID string
	PublicKey string
	Endpoint  string
	// CIDRs is the union of the peer's pod and service ranges.
	CIDRs []string
}

// TopologyConflict describes a problem in a set of remote peers that makes the
// mesh ambiguous or unsafe to program. It names the offending cluster or the
// pair of clusters and a human-readable reason.
type TopologyConflict struct {
	ClusterID string
	Reason    string
}

func (c TopologyConflict) String() string {
	return fmt.Sprintf("%s: %s", c.ClusterID, c.Reason)
}

// DetectTopologyConflicts validates a control plane's advertised topology and returns
// conflict found. It is a pure, deterministic function so the every syncer can
// log and gauge conflicts identically on every node without coordinating.
//
// It flags four classes of problem:
//
//   - A peer missing a required field (cluster ID, public key, or endpoint);
//   - Two peers sharing the same cluster ID (identity collision);
//   - Two peers sharing the same WireGuard public key (the kernel keys peers by
//     public key, so this would make them indistinguishable);
//   - Two distinct peers advertising overlapping CIDRs (ambiguous routing —
//     a packet's destination cluster would be undecidable).
//
// Malformed CIDRs are reported per-peer rather than silently dropped.
func DetectTopologyConflicts(peers []PeerIdentity) []TopologyConflict {
	var conflicts []TopologyConflict

	seenID := map[string]bool{}
	seenKey := map[string]string{} // public key -> first cluster ID that used it
	var parsed []parsedPeer

	for i, p := range peers {
		conflicts = append(conflicts, missingFieldConflicts(p, i)...)
		conflicts = append(conflicts, identityConflicts(p, seenID, seenKey)...)

		pp, cidrConflicts := parsePeerCIDRs(p, i)
		conflicts = append(conflicts, cidrConflicts...)
		parsed = append(parsed, pp)
	}

	conflicts = append(conflicts, cidrOverlapConflicts(parsed)...)

	sort.Slice(conflicts, func(i, j int) bool {
		if conflicts[i].ClusterID != conflicts[j].ClusterID {
			return conflicts[i].ClusterID < conflicts[j].ClusterID
		}
		return conflicts[i].Reason < conflicts[j].Reason
	})
	return conflicts
}

// parsedPeer retains a peer's parsed CIDRs for the cross-peer overlap pass.
type parsedPeer struct {
	id    string
	nets  []*net.IPNet
	order int
}

// missingFieldConflicts flags any required field - cluster ID, public key,
// endpoint absent on a single peer.
func missingFieldConflicts(p PeerIdentity, i int) []TopologyConflict {
	var out []TopologyConflict
	if p.ClusterID == "" {
		out = append(out, TopologyConflict{ClusterID: fmt.Sprintf("<peer #%d>", i), Reason: "missing cluster ID"})
	}
	if p.PublicKey == "" {
		out = append(out, TopologyConflict{ClusterID: p.ClusterID, Reason: "missing public key"})
	}
	if p.Endpoint == "" {
		out = append(out, TopologyConflict{ClusterID: p.ClusterID, Reason: "missing endpoint"})
	}
	return out
}

// identityConflicts flags duplicate cluster IDs and shared public keys,
// updating the seen maps as it inspects the peer.
func identityConflicts(p PeerIdentity, seenID map[string]bool, seenKey map[string]string) []TopologyConflict {
	var out []TopologyConflict
	if p.ClusterID != "" {
		if seenID[p.ClusterID] {
			out = append(out, TopologyConflict{ClusterID: p.ClusterID, Reason: "duplicate cluster ID"})
		}
		seenID[p.ClusterID] = true
	}
	if p.PublicKey != "" {
		if first, ok := seenKey[p.PublicKey]; ok {
			out = append(out, TopologyConflict{
				ClusterID: p.ClusterID,
				Reason:    fmt.Sprintf("public key %s also used by cluster %q", shortKey(p.PublicKey), first),
			})
		} else {
			seenKey[p.PublicKey] = p.ClusterID
		}
	}
	return out
}

// parsePeerCIDRs parses a peer's CIDRs into a parsedPeer, returning a conflict
// for each malformed entry reported per-peer rather than silently dropped.
func parsePeerCIDRs(p PeerIdentity, order int) (parsedPeer, []TopologyConflict) {
	pp := parsedPeer{id: p.ClusterID, order: order}
	var out []TopologyConflict
	for _, c := range p.CIDRs {
		_, ipnet, err := net.ParseCIDR(c)
		if err != nil {
			out = append(out, TopologyConflict{ClusterID: p.ClusterID, Reason: fmt.Sprintf("invalid CIDR %q: %v", c, err)})
			continue
		}
		pp.nets = append(pp.nets, ipnet)
	}
	return pp, out
}

// cidrOverlapConflicts flags every unordered pair of distinct peers whose CIDRs
// overlap. Each conflict is canonicalized to the smaller cluster ID so the
// result is independent of the order peers arrive in — every node agrees.
func cidrOverlapConflicts(parsed []parsedPeer) []TopologyConflict {
	var out []TopologyConflict
	for i := 0; i < len(parsed); i++ {
		for j := i + 1; j < len(parsed); j++ {
			a, b := parsed[i], parsed[j]
			if a.id == b.id {
				continue // same cluster advertised twice; identity dup already flagged
			}
			if a.id > b.id {
				a, b = b, a
			}
			if net := firstOverlap(a.nets, b.nets); net != "" {
				out = append(out, TopologyConflict{
					ClusterID: a.id,
					Reason:    fmt.Sprintf("CIDR %s overlaps cluster %q", net, b.id),
				})
			}
		}
	}
	return out
}

// shortKey truncates a WireGuard public key for logs/messages so full key
// material is never emitted (mirrors pkg/wg.shortKey).
func shortKey(k string) string {
	if len(k) <= 8 {
		return k
	}
	return k[:8] + "…"
}

// firstOverlap returns the CIDR string of the first network in a that overlaps
// any network in b, or "" if none do.
func firstOverlap(a, b []*net.IPNet) string {
	for _, x := range a {
		for _, y := range b {
			if cidrsOverlap(x, y) {
				return x.String()
			}
		}
	}
	return ""
}
