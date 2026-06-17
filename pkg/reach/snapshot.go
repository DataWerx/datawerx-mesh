package reach

import (
	"github.com/datawerx/datawerx/pkg/meshfw"
	"github.com/datawerx/datawerx/pkg/verify"
)

// FromSnapshot builds the reachability matrix from a mesh snapshot, so the
// `dwxctl reach` command and the read-only MCP server compute it from the same
// source of truth as every other read surface. The snapshot already carries the
// peers (with phase and CIDRs), the topology conflicts, and — since the policy
// sources were added to the contract — each MeshNetworkPolicy's ingress rules,
// which is everything Plan needs.
func FromSnapshot(snap verify.Snapshot) Matrix {
	conflicted := map[string]bool{}
	for _, c := range snap.Conflicts {
		conflicted[c.ClusterID] = true
	}

	clusterCIDRs := map[string][]string{}
	peers := make([]Peer, 0, len(snap.Peers))
	for _, p := range snap.Peers {
		cidrs := append(append([]string(nil), p.PodCIDRs...), p.ServiceCIDRs...)
		clusterCIDRs[p.ClusterID] = cidrs
		peers = append(peers, Peer{
			ClusterID: p.ClusterID,
			Connected: p.Phase == "Connected",
			Conflict:  conflicted[p.ClusterID],
			CIDRs:     cidrs,
		})
	}

	return Plan(Input{
		Peers:        peers,
		Policies:     policiesFromSnapshot(snap.Policies),
		ClusterCIDRs: clusterCIDRs,
	})
}

// policiesFromSnapshot projects the snapshot's policy records back onto the
// meshfw.Policy inputs the firewall compiler consumes.
func policiesFromSnapshot(in []verify.PolicySnapshot) []meshfw.Policy {
	out := make([]meshfw.Policy, 0, len(in))
	for _, p := range in {
		fw := meshfw.Policy{Name: p.Name, Destinations: p.Destinations}
		for _, ing := range p.Ingress {
			rule := meshfw.IngressRule{}
			for _, f := range ing.From {
				rule.From = append(rule.From, meshfw.PeerSelector{ClusterIDs: f.ClusterIDs, CIDRs: f.CIDRs})
			}
			for _, pt := range ing.Ports {
				rule.Ports = append(rule.Ports, meshfw.Port{Protocol: pt.Protocol, Port: pt.Port})
			}
			fw.Ingress = append(fw.Ingress, rule)
		}
		out = append(out, fw)
	}
	return out
}
