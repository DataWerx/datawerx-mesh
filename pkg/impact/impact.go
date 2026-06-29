// Package impact is the pure dry-run/impact analyzer behind `dwx mesh policy
// --dry-run`. Given a proposed change to the mesh's policy or peer set, it
// reports what the change would expose, what it would conflict with, and what it
// would break — before anything is applied.
//
// It owns no new policy semantics.  It composes the existing pure compilers
// (meshfw.BuildFirewall for the firewall, topology.PlanPeer /
// DetectTopologyConflicts for peers) and diffs their outputs. That keeps the
// analysis provably consistent with what the data plane will actually program,
// and, like every analyzer in this repo, side-effect-free and exhaustively
// table-testable with no cluster.
package impact

import (
	"fmt"
	"sort"
	"strings"

	networkingv1alpha1 "github.com/DataWerx/datawerx-mesh/pkg/apis/networking/v1alpha1"
	"github.com/DataWerx/datawerx-mesh/pkg/meshfw"
	"github.com/DataWerx/datawerx-mesh/pkg/topology"
)

// Exposure is one (source → destination : protocol/port) reachability the
// firewall would permit. An empty field means "any": empty Source is any
// source, empty Dest is every protected destination, empty Port is all ports.
type Exposure struct {
	Source   string `json:"source,omitempty"`
	Dest     string `json:"dest,omitempty"`
	Protocol string `json:"protocol,omitempty"`
	Port     string `json:"port,omitempty"`
}

func (e Exposure) String() string {
	src := orAny(e.Source)
	dst := orAny(e.Dest)
	pp := "all ports"
	if e.Port != "" {
		pp = e.Protocol + "/" + e.Port
	}
	return fmt.Sprintf("%s → %s (%s)", src, dst, pp)
}

// PolicyImpact is the result of analyzing a proposed firewall policy set against
// the current one.
type PolicyImpact struct {
	// NewlyExposed are reachabilities the proposed set permits that the current
	// set did not — the blast radius of the change.
	NewlyExposed []Exposure `json:"newlyExposed,omitempty"`
	// NewlyDenied are reachabilities the current set permits that the proposed
	// set drops — what the change would *break*.
	NewlyDenied []Exposure `json:"newlyDenied,omitempty"`
	// NewlyProtected are destinations that become default-deny under the proposed
	// set (were unprotected before). "" means all mesh ingress.
	NewlyProtected []string `json:"newlyProtected,omitempty"`
	// NoLongerProtected are destinations that stop being default-deny.
	NoLongerProtected []string `json:"noLongerProtected,omitempty"`
	// Warnings flag over-broad or footgun constructs in the proposed set.
	Warnings []string `json:"warnings,omitempty"`
	// Skipped lists proposed inputs dropped from the plan (non-IPv4 today).
	Skipped []string `json:"skipped,omitempty"`
}

// AnalyzePolicyChange diffs the firewall the proposed policy set would program
// against the current one, and flags over-broad constructs. Both arguments are
// full policy sets; the caller (CLI) builds `proposed` by replacing or adding
// the policy under review into the current set. clusterCIDRs resolves cluster-ID
// selectors, exactly as the real compiler resolves them.
func AnalyzePolicyChange(current, proposed []meshfw.Policy, clusterCIDRs map[string][]string) PolicyImpact {
	cur := parseRuleset(meshfw.BuildFirewall(current, clusterCIDRs))
	prop := parseRuleset(meshfw.BuildFirewall(proposed, clusterCIDRs))

	imp := PolicyImpact{
		NewlyExposed:      diffExposures(cur.accepts, prop.accepts),
		NewlyDenied:       diffExposures(prop.accepts, cur.accepts),
		NewlyProtected:    diffStrings(cur.protected, prop.protected),
		NoLongerProtected: diffStrings(prop.protected, cur.protected),
		Skipped:           meshfw.BuildFirewall(proposed, clusterCIDRs).Skipped,
		Warnings:          policyWarnings(proposed, clusterCIDRs),
	}
	return imp
}

// rulesetView is the structured projection of a compiled Ruleset the diff works
// over: the set of permitted exposures and the set of protected destinations.
type rulesetView struct {
	accepts   map[Exposure]struct{}
	protected map[string]struct{}
}

// parseRuleset interprets a compiled meshfw.Ruleset into structured accept and
// protected sets, composing meshfw.Interpret so impact reads the same
// data-plane-consistent decisions the reachability analyzer does.
func parseRuleset(rs meshfw.Ruleset) rulesetView {
	d := meshfw.Interpret(rs)
	v := rulesetView{accepts: map[Exposure]struct{}{}, protected: map[string]struct{}{}}
	for _, a := range d.Accepts {
		v.accepts[Exposure{Source: a.Source, Dest: a.Dest, Protocol: a.Protocol, Port: a.Port}] = struct{}{}
	}
	for _, p := range d.Protected {
		v.protected[p] = struct{}{}
	}
	return v
}

// policyWarnings flags the footguns the brief cares about: an any-source allow,
// a default-deny-everything posture, and selectors naming clusters the topology
// doesn't know (which silently resolve to no sources).
func policyWarnings(policies []meshfw.Policy, clusterCIDRs map[string][]string) []string {
	var w []string
	for _, p := range policies {
		name := p.Name
		if name == "" {
			name = "<unnamed>"
		}
		if len(p.Destinations) == 0 {
			w = append(w, fmt.Sprintf("policy %q has no destinations: it makes ALL mesh ingress default-deny on this cluster", name))
		}
		for _, ing := range p.Ingress {
			for _, sel := range ing.From {
				for _, c := range sel.CIDRs {
					if c == "0.0.0.0/0" || c == "::/0" {
						w = append(w, fmt.Sprintf("policy %q allows from %s: any source on the mesh is permitted", name, c))
					}
				}
				for _, id := range sel.ClusterIDs {
					if _, ok := clusterCIDRs[id]; !ok {
						w = append(w, fmt.Sprintf("policy %q references cluster %q which is not known to the topology: it resolves to no sources (the rule allows nothing)", name, id))
					}
				}
			}
		}
	}
	sort.Strings(w)
	return dedupeStrings(w)
}

// PeerImpact is the result of analyzing a proposed MeshPeer change.
type PeerImpact struct {
	// Phase is the status phase PlanPeer would assign the proposed peer.
	Phase string `json:"phase"`
	// Routable are the CIDRs that would be programmed.
	Routable []string `json:"routable,omitempty"`
	// Withheld are CIDRs the planner refuses to route (overlap/malformed).
	Withheld []string `json:"withheld,omitempty"`
	// Dangerous are CIDRs refused because they are never safe to route
	// (default/loopback/link-local/multicast).
	Dangerous []string `json:"dangerous,omitempty"`
	// NewConflicts are topology conflicts the proposed peer introduces against
	// the existing peer set (overlaps, duplicate ID, shared key) that do not
	// already exist among the current peers alone.
	NewConflicts []string `json:"newConflicts,omitempty"`
	// Message is the planner's human-readable summary.
	Message string `json:"message"`
}

// AnalyzePeerChange reports what programming the proposed peer would do: which
// CIDRs route, which are withheld, and any *new* topology conflict it creates
// against the existing peers. existing is the current advertised peer set
// (excluding the one being changed).
func AnalyzePeerChange(proposed networkingv1alpha1.MeshPeerSpec, localCIDRs []string, existing []topology.PeerIdentity) PeerImpact {
	plan := topology.PlanPeer(proposed, localCIDRs)
	imp := PeerImpact{
		Phase:     string(plan.Phase),
		Routable:  plan.RoutableCIDRs,
		Withheld:  plan.ConflictingCIDRs,
		Dangerous: topology.DangerousCIDRs(proposed.AllCIDRs()),
		Message:   plan.Message,
	}

	before := conflictSet(topology.DetectTopologyConflicts(existing))
	withProposed := append(append([]topology.PeerIdentity(nil), existing...), topology.PeerIdentity{
		ClusterID: proposed.ClusterID,
		PublicKey: proposed.PublicKey,
		Endpoint:  proposed.Endpoint,
		CIDRs:     proposed.AllCIDRs(),
	})
	for _, c := range topology.DetectTopologyConflicts(withProposed) {
		if _, existed := before[c.String()]; !existed {
			imp.NewConflicts = append(imp.NewConflicts, c.String())
		}
	}
	sort.Strings(imp.NewConflicts)
	return imp
}

// Safe reports whether the proposed peer change is clean: it programs at least
// as intended with no withheld/dangerous CIDRs and no new topology conflict.
func (p PeerImpact) Safe() bool {
	return len(p.Withheld) == 0 && len(p.Dangerous) == 0 && len(p.NewConflicts) == 0
}

func conflictSet(cs []topology.TopologyConflict) map[string]struct{} {
	m := make(map[string]struct{}, len(cs))
	for _, c := range cs {
		m[c.String()] = struct{}{}
	}
	return m
}

// diffExposures returns the exposures in b that are not in a, sorted.
func diffExposures(a, b map[Exposure]struct{}) []Exposure {
	var out []Exposure
	for e := range b {
		if _, ok := a[e]; !ok {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// diffStrings returns the keys in b not in a, sorted.
func diffStrings(a, b map[string]struct{}) []string {
	var out []string
	for s := range b {
		if _, ok := a[s]; !ok {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func orAny(s string) string {
	if s == "" {
		return "any"
	}
	return s
}

func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := in[:1]
	for _, s := range in[1:] {
		if s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}

// JoinWarnings renders warnings as a single newline-separated block (CLI helper).
func JoinWarnings(w []string) string { return strings.Join(w, "\n") }
