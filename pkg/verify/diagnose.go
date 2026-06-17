package verify

import (
	"fmt"
	"sort"
	"strings"
)

// Diagnose is a deterministic "obvious cause" checker. Given a mesh
// snapshot, it returns the grounded reasons the mesh is unhealthy. A CIDR
// overlap, a dead tunnel, an invalid export — each one citing the concrete
// signal it read. There is no model here and there never will be: this is the
// rule-based floor that an operator (or a higher layer) gets for free, and the
// Signal field is the contract that any explanation must stay grounded in.
//
// It is a pure function of the snapshot, so it is exhaustively table-testable
// with no cluster, like everything else in this package.

// Severity ranks a finding. Findings are returned most-severe first.
type Severity int

const (
	// SeverityInfo is contextual: worth knowing, not itself a fault.
	SeverityInfo Severity = iota
	// SeverityWarning is a degradation that may be transient or benign.
	SeverityWarning
	// SeverityCritical is a fault that breaks (or will break) connectivity.
	SeverityCritical
)

func (s Severity) String() string {
	switch s {
	case SeverityCritical:
		return "CRITICAL"
	case SeverityWarning:
		return "WARNING"
	default:
		return "INFO"
	}
}

// MarshalText renders the severity by name in JSON.
func (s Severity) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// Finding is one diagnosed cause. Signal is the concrete observation it is
// grounded in (a phase, a handshake age, a conflict reason); Remedy is the
// suggested next step. Both are plain text so the finding is useful on a
// terminal and as a contract a consumer can render.
type Finding struct {
	Severity Severity `json:"severity"`
	Title    string   `json:"title"`
	Detail   string   `json:"detail,omitempty"`
	Signal   string   `json:"signal"`
	Remedy   string   `json:"remedy,omitempty"`
}

// Diagnose walks the snapshot and returns the obvious causes, most severe
// first. An empty result means nothing rule-detectable is wrong.
func Diagnose(s Snapshot) []Finding {
	var f []Finding
	f = append(f, diagnoseHealth(s)...)
	f = append(f, diagnoseConflicts(s)...)
	f = append(f, diagnosePeers(s)...)
	f = append(f, diagnoseExports(s)...)
	f = append(f, diagnosePolicies(s)...)
	f = append(f, diagnoseEvents(s)...)

	sort.SliceStable(f, func(i, j int) bool {
		if f[i].Severity != f[j].Severity {
			return f[i].Severity > f[j].Severity // critical first
		}
		if f[i].Title != f[j].Title {
			return f[i].Title < f[j].Title
		}
		return f[i].Signal < f[j].Signal
	})
	return f
}

// diagnoseHealth lifts every failed health check into a critical finding so the
// foundational faults surface first and
// already explain themselves via the check detail.
func diagnoseHealth(s Snapshot) []Finding {
	var out []Finding
	for _, c := range s.Health.Checks {
		if c.Status != StatusFail {
			continue
		}
		out = append(out, Finding{
			Severity: SeverityCritical,
			Title:    "Health check failing: " + c.Name,
			Signal:   fmt.Sprintf("%s = FAIL (%s)", c.Name, c.Detail),
			Remedy:   "resolve the failing check before diagnosing peers; nothing routes until the agent and CRDs are healthy",
		})
	}
	return out
}

// diagnoseConflicts reports every advertised-topology conflict. These are the
// canonical "A can't reach B" root cause: an overlap makes a packet's
// destination cluster undecidable, so the peer is refused.
func diagnoseConflicts(s Snapshot) []Finding {
	var out []Finding
	for _, c := range s.Conflicts {
		out = append(out, Finding{
			Severity: SeverityCritical,
			Title:    "Topology conflict on cluster " + c.ClusterID,
			Detail:   c.Reason,
			Signal:   fmt.Sprintf("conflict[%s]: %s", c.ClusterID, c.Reason),
			Remedy:   conflictRemedy(c.Reason),
		})
	}
	return out
}

// diagnosePeers reports per-peer faults: a peer the agent refused (Error), a
// Connected peer whose tunnel looks dead, and a peer stuck Pending.
func diagnosePeers(s Snapshot) []Finding {
	var out []Finding
	for _, p := range s.Peers {
		switch p.Phase {
		case "Error":
			out = append(out, Finding{
				Severity: SeverityCritical,
				Title:    "Peer " + p.ClusterID + " in Error",
				Detail:   p.Message,
				Signal:   fmt.Sprintf("peer[%s].phase=Error: %s", p.ClusterID, p.Message),
				Remedy:   peerErrorRemedy(p.Message),
			})
		case "Connected":
			if stale, why := staleTunnel(p); stale {
				out = append(out, Finding{
					Severity: SeverityWarning,
					Title:    "Tunnel to " + p.ClusterID + " may be down",
					Detail:   why,
					Signal:   fmt.Sprintf("peer[%s] connected, %s", p.ClusterID, why),
					Remedy:   "verify the peer endpoint is reachable (UDP/firewall, NAT, correct host:port); an idle tunnel can also legitimately go quiet",
				})
			}
		case "Pending", "":
			out = append(out, Finding{
				Severity: SeverityWarning,
				Title:    "Peer " + p.ClusterID + " not programmed",
				Detail:   p.Message,
				Signal:   fmt.Sprintf("peer[%s].phase=%q", p.ClusterID, phaseOrUnset(p.Phase)),
				Remedy:   "the node agent has not converged this peer yet; check the agent pod on this node has NET_ADMIN and is running",
			})
		}
	}
	return out
}

// staleTunnel reports whether a Connected peer's handshake looks dead and why.
// A never-handshaked Connected peer is the strongest signal; otherwise the age
// must exceed the same threshold the health report uses.
func staleTunnel(p PeerSnapshot) (bool, string) {
	if p.LastHandshake <= 0 {
		return true, "no handshake recorded yet"
	}
	if p.HandshakeAge > StaleHandshakeSeconds {
		return true, fmt.Sprintf("last handshake %ds ago (> %ds)", p.HandshakeAge, StaleHandshakeSeconds)
	}
	return false, ""
}

// diagnoseExports reports invalid or conflicting service exports — the discovery
// counterpart to a connectivity fault: the tunnel may be up but the name won't
// resolve.
func diagnoseExports(s Snapshot) []Finding {
	var out []Finding
	for _, e := range s.Exports {
		switch {
		case e.Conflict:
			out = append(out, Finding{
				Severity: SeverityWarning,
				Title:    "Export conflict: " + e.Namespace + "/" + e.Name,
				Detail:   e.Message,
				Signal:   fmt.Sprintf("export[%s/%s].conflict: %s", e.Namespace, e.Name, e.Message),
				Remedy:   "clusters exported this name with incompatible type/ports; align the Service definitions across clusters",
			})
		case !e.Valid:
			out = append(out, Finding{
				Severity: SeverityWarning,
				Title:    "Export invalid: " + e.Namespace + "/" + e.Name,
				Detail:   e.Message,
				Signal:   fmt.Sprintf("export[%s/%s].valid=false: %s", e.Namespace, e.Name, e.Message),
				Remedy:   "ensure a Service of the same name/namespace exists and is selectable; the export marks itself Valid once it resolves",
			})
		}
	}
	return out
}

// diagnosePolicies reports mesh network policies that failed to compile/program.
func diagnosePolicies(s Snapshot) []Finding {
	var out []Finding
	for _, p := range s.Policies {
		if p.Phase != "Error" {
			continue
		}
		out = append(out, Finding{
			Severity: SeverityCritical,
			Title:    "Policy " + p.Name + " failed to program",
			Detail:   p.Message,
			Signal:   fmt.Sprintf("policy[%s].phase=Error: %s", p.Name, p.Message),
			Remedy:   "fix the policy spec (likely a malformed or non-IPv4 CIDR); until it programs, its protected destinations may be exposed or fail closed",
		})
	}
	return out
}

// diagnoseEvents surfaces recent Warning events as corroborating context. They
// are never themselves the verdict — they point at where a fault was last seen.
func diagnoseEvents(s Snapshot) []Finding {
	var out []Finding
	for _, e := range s.Events {
		if e.Type != "Warning" {
			continue
		}
		out = append(out, Finding{
			Severity: SeverityInfo,
			Title:    "Recent warning: " + e.Reason,
			Detail:   e.Message,
			Signal:   fmt.Sprintf("event[%s] %s: %s", e.Object, e.Reason, e.Message),
		})
	}
	return out
}

// conflictRemedy tailors the fix advice to the kind of topology conflict.
func conflictRemedy(reason string) string {
	switch {
	case strings.Contains(reason, "overlap"):
		return "the two clusters reuse a range; renumber one, or enable basic NAT remap (DataWerx_REMAP_CIDR) to route them under virtual ranges"
	case strings.Contains(reason, "public key"):
		return "two peers advertise the same WireGuard key; regenerate one cluster's key so the kernel can tell them apart"
	case strings.Contains(reason, "duplicate cluster ID"):
		return "two peers share a clusterID; give each cluster a unique, stable ID"
	default:
		return "correct the advertised peer fields so the topology is unambiguous"
	}
}

// peerErrorRemedy tailors advice to a peer Error message.
func peerErrorRemedy(msg string) string {
	switch {
	case strings.Contains(msg, "dangerous"):
		return "the peer advertised a default/loopback/link-local/multicast range that is never routed into the mesh; remove it from the peer's CIDRs"
	case strings.Contains(msg, "overlap") || strings.Contains(msg, "remap"):
		return "the peer's CIDR overlaps a local range; renumber, or enable DataWerx_REMAP_CIDR to route it under a virtual range"
	case strings.Contains(msg, "publicKey"):
		return "set spec.publicKey on the MeshPeer to the remote cluster's WireGuard public key"
	default:
		return "inspect the MeshPeer status message and correct the offending spec field"
	}
}

func phaseOrUnset(phase string) string {
	if phase == "" {
		return "<unset>"
	}
	return phase
}
