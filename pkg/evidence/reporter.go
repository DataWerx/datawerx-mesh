// Package evidence contains the premium-tier evidence reporter: a
// manager.Runnable that periodically assembles this cluster's grounded Evidence
// and pushes it to the managed control plane for the premium "DataWerx Signal"
// fleet view.
//
// It is the producer side of the /api/v1/evidence contract; the control plane
// (datawerx-admin) is the consumer. Like the topology syncer, it is open-core
// code that only runs in the premium tier — the free tier never constructs it —
// and it lives behind a small Sink seam so the agent stays decoupled from the
// transport. It is read-only with respect to the mesh: it reports state, it never
// mutates it.
package evidence

import (
	"context"
	"crypto/sha256"
	"time"

	"github.com/go-logr/logr"

	"github.com/DataWerx/datawerx-mesh/pkg/signal"
	"github.com/DataWerx/datawerx-mesh/pkg/verify"
)

// Sink pushes a serialized evidence report to the control plane. The premium
// EnterpriseControlPlaneClient satisfies it via PushEvidence; injecting it as an
// interface keeps this package free of any HTTP/transport dependency and makes
// the reporter testable without a control plane.
type Sink interface {
	PushEvidence(ctx context.Context, payload []byte) error
}

// SnapshotFunc gathers the current mesh snapshot. It is injected (rather than the
// reporter building a Kubernetes client itself) so the reporter is unit-testable
// with no cluster, mirroring how dwx-mcp injects its snapshot source.
type SnapshotFunc func(ctx context.Context) (verify.Snapshot, error)

// Reporter periodically assembles and pushes this cluster's grounded evidence.
// It satisfies manager.Runnable.
//
// NOTE: like the topology syncer, in a multi-node DaemonSet this is a
// cluster-singleton concern colocated in the agent binary to keep it a single
// process. The control plane upserts the latest report per cluster, so concurrent
// reporters on every node converge harmlessly (the report is cluster-scoped state
// read identically on each node); the unchanged-evidence short-circuit below keeps
// the redundant pushes cheap. A production deployment may gate this behind leader
// election or a dedicated pod.
type Reporter struct {
	Snapshot SnapshotFunc
	Sink     Sink
	Interval time.Duration
	Log      logr.Logger

	// lastHash is the digest of the most recently pushed payload, so an unchanged
	// evidence report short-circuits the push — the same idea as the syncer's
	// topology-revision skip.
	lastHash [32]byte
	// pushed is true once at least one report has been pushed, so the first
	// report always sends even if it happens to hash to the zero value.
	pushed bool
}

// Start runs the report loop until the context is cancelled. It primes once
// immediately so a freshly started agent reports without waiting a full interval.
func (r *Reporter) Start(ctx context.Context) error {
	if r.Interval <= 0 {
		r.Interval = 30 * time.Second
	}
	ticker := time.NewTicker(r.Interval)
	defer ticker.Stop()

	r.reportOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			r.reportOnce(ctx)
		}
	}
}

// reportOnce assembles the evidence and pushes it, logging rather than returning
// errors so a transient failure never tears down the loop. The next interval
// re-reports, and the control plane upserts, so a dropped push self-heals.
func (r *Reporter) reportOnce(ctx context.Context) {
	snap, err := r.Snapshot(ctx)
	if err != nil {
		// Info, not Error: gathering can fail transiently (API blip) and the next
		// tick recovers; this is an expected, recoverable degradation.
		r.Log.Info("evidence report skipped: snapshot gather failed", "err", err)
		return
	}

	payload, err := signal.BuildEvidence(snap).JSON()
	if err != nil {
		r.Log.Error(err, "marshaling evidence report")
		return
	}

	// Short-circuit an unchanged report so a steady-state mesh does not push the
	// same evidence every interval (and, on a multi-node DaemonSet, from every
	// node).
	hash := sha256.Sum256(payload)
	if r.pushed && hash == r.lastHash {
		r.Log.V(1).Info("evidence unchanged; skipping push", "peers", len(snap.Peers))
		return
	}

	if err := r.Sink.PushEvidence(ctx, payload); err != nil {
		r.Log.Info("evidence push failed; will retry next interval", "err", err)
		return
	}
	r.lastHash = hash
	r.pushed = true
	r.Log.V(1).Info("evidence reported", "bytes", len(payload), "peers", len(snap.Peers))
}
