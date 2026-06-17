// Package probe is the pure core of DataWerx's active synthetic prober: the
// runtime, application-layer counterpart to pkg/reach's expected reachability.
// reach says a peer *should* be able to reach this cluster; the prober dials a
// tiny per-node responder to learn whether it actually *can*, and records the
// outcome as the same slo.Liveness signal the WireGuard-handshake provider
// already feeds slo.Assess. That makes the prober a drop-in second source of
// observed connectivity, exactly as design 0011 anticipated.
//
// Following the repo-wide discipline, every decision lives here as a
// side-effect-free function — which peers to dial (PlanTargets), how to
// classify a dial outcome (Classify), and how the latest results become a
// liveness signal (Observations.Liveness). The runtime shell in this package
// (Responder, Prober) only performs the I/O; the live cross-cluster dialing is
// validated by the kind e2e, not by these hermetic unit tests.
package probe

import (
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/DataWerx/datawerx-mesh/pkg/slo"
)

// Peer is a remote cluster's probe-relevant state. It mirrors reach.Peer but
// carries the responder address the prober dials rather than the policy inputs.
type Peer struct {
	// ClusterID is the mesh ID of the remote cluster.
	ClusterID string
	// Connected is true when the peer's tunnel is up (MeshPeer phase Connected).
	Connected bool
	// Conflict is true when the peer is named in a topology conflict (overlap),
	// which makes any probe result ambiguous.
	Conflict bool
	// ProbeAddress is the host:port of the peer's probe responder, reachable
	// across the mesh. Empty means the peer advertises no responder.
	ProbeAddress string
}

// Target is one responder the prober should dial this cycle.
type Target struct {
	ClusterID string
	Address   string
}

// PlanTargets selects which peers to actively probe. A peer is dialed only when
// the data plane should already be carrying traffic to it: connected, free of a
// CIDR conflict, and advertising a responder address. Probing an unconnected or
// conflicted peer would only re-report what reach already explains, so it is
// skipped. The result is sorted by cluster ID so a cycle is deterministic.
func PlanTargets(peers []Peer) []Target {
	out := make([]Target, 0, len(peers))
	for _, p := range peers {
		if !p.Connected || p.Conflict || p.ProbeAddress == "" {
			continue
		}
		out = append(out, Target{ClusterID: p.ClusterID, Address: p.ProbeAddress})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ClusterID < out[j].ClusterID })
	return out
}

// Result is the outcome of one dial of one responder.
type Result struct {
	// ClusterID is the cluster the prober dialed.
	ClusterID string
	// OK is true when the responder answered as healthy, from the expected
	// cluster, within the deadline.
	OK bool
	// RTTMillis is the round-trip latency of a successful probe; 0 on failure.
	RTTMillis int64
	// Reason explains a failure and is empty on success.
	Reason string
	// ObservedAtUnix is when the probe completed, in epoch seconds.
	ObservedAtUnix int64
}

// Classify turns one dial outcome into a Result. It is the whole verdict logic
// of a probe, kept pure so the runtime adapter (httpProbe) only has to gather
// the raw outcome. A nil dialErr with a 200 response from the expected cluster
// is the only success; everything else is a grounded failure. The cluster-ID
// cross-check is deliberate: reaching a 200 from the *wrong* cluster means
// traffic is misrouted (often a CIDR overlap), which is a failure even though a
// responder answered.
func Classify(clusterID string, statusCode int, body []byte, rtt time.Duration, now int64, dialErr error) Result {
	r := Result{ClusterID: clusterID, ObservedAtUnix: now}
	switch {
	case dialErr != nil:
		r.Reason = "dial failed: " + dialErr.Error()
	case statusCode != http.StatusOK:
		r.Reason = fmt.Sprintf("responder returned HTTP %d, want %d", statusCode, http.StatusOK)
	default:
		answered, ok := parseProbeBody(body)
		switch {
		case !ok:
			r.Reason = "responder body was not a recognizable DataWerx probe envelope"
		case answered != clusterID:
			r.Reason = fmt.Sprintf("responder identified as cluster %q but %q was dialed; traffic is misrouted", answered, clusterID)
		default:
			r.OK = true
			r.RTTMillis = rtt.Milliseconds()
		}
	}
	return r
}

// ProbeStatus is the minimal probe observation persisted per peer, the input
// and output of the churn-control fold. It maps directly onto the MeshPeer
// status fields the prober writes (LastProbeAttempt, LastProbeTime).
type ProbeStatus struct {
	// LastAttemptUnix is the epoch of the most recent probe of the peer, any
	// outcome.
	LastAttemptUnix int64
	// LastSuccessUnix is the epoch of the most recent successful probe.
	LastSuccessUnix int64
}

// NextProbeStatus folds a probe result into the stored per-peer status and
// reports whether the change is worth persisting. Because every node probes and
// would otherwise write the shared cluster-scoped MeshPeer status on every
// cycle, it persists only when the healthy/unhealthy state flips or the stored
// attempt has aged past refresh seconds — enough to keep the observed age fresh
// without a write per cycle per node.
func NextProbeStatus(cur ProbeStatus, r Result, refresh int64) (ProbeStatus, bool) {
	next := cur
	next.LastAttemptUnix = r.ObservedAtUnix
	if r.OK {
		next.LastSuccessUnix = r.ObservedAtUnix
	}
	wasHealthy := cur.LastAttemptUnix > 0 && cur.LastSuccessUnix == cur.LastAttemptUnix
	stateFlipped := wasHealthy != r.OK
	aged := cur.LastAttemptUnix == 0 || r.ObservedAtUnix-cur.LastAttemptUnix >= refresh
	return next, stateFlipped || aged
}

// Observations is the prober's in-memory record of the most recent result per
// cluster, the store from which the slo.Liveness signal is derived. The zero
// value is ready to use, but it is not safe for concurrent use; the Prober
// owns one and only touches it from its own goroutine.
type Observations struct {
	latest map[string]Result
}

// Record stores r as the most recent result for its cluster, replacing any
// earlier one.
func (o *Observations) Record(r Result) {
	if o.latest == nil {
		o.latest = make(map[string]Result)
	}
	o.latest[r.ClusterID] = r
}

// Latest returns a copy of the most recent result for cluster, and whether one
// has been recorded.
func (o *Observations) Latest(cluster string) (Result, bool) {
	r, ok := o.latest[cluster]
	return r, ok
}

// Liveness projects the recorded results into the slo.Liveness signal keyed by
// cluster, so the active prober drops straight into slo.Assess alongside the
// WireGuard-handshake provider. A successful probe yields an age of now minus
// the observation time; a failed probe yields a negative age, which slo reads
// as "not live" exactly as it reads a stale or absent handshake. A cluster that
// has never been probed is simply absent from the map, which slo also treats as
// not live.
func (o *Observations) Liveness(now int64) map[string]slo.Liveness {
	out := make(map[string]slo.Liveness, len(o.latest))
	for cluster, r := range o.latest {
		if !r.OK {
			out[cluster] = slo.Liveness{HandshakeAge: -1}
			continue
		}
		age := now - r.ObservedAtUnix
		if age < 0 {
			age = 0
		}
		out[cluster] = slo.Liveness{HandshakeAge: age}
	}
	return out
}
