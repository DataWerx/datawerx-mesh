package probe

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/go-logr/logr"

	dwxmetrics "github.com/datawerx/datawerx/pkg/metrics"
)

const (
	// DefaultInterval is how often the prober dials every target.
	DefaultInterval = 30 * time.Second
	// DefaultTimeout caps a single dial so one dead peer cannot stall the cycle.
	DefaultTimeout = 5 * time.Second
	// maxProbeBody caps how much of a responder's body the prober reads, so a
	// hostile or broken endpoint cannot make the prober allocate without bound.
	maxProbeBody = 4 << 10
)

// ProbeFunc dials one target and returns the classified result. It is the seam
// that lets the Prober be unit-tested without a network; production uses
// httpProbe.
type ProbeFunc func(ctx context.Context, target Target) Result

// PeerLister returns the current set of probe-relevant peers. The runtime
// implementation reads MeshPeer objects from the cache and derives each peer's
// responder address; tests supply a fixed slice. Keeping it a function leaves
// the Prober free of any Kubernetes types.
type PeerLister func(ctx context.Context) ([]Peer, error)

// Publisher persists a cycle's results back onto the mesh so the read surfaces
// (dwxctl slo, mesh_connectivity) reflect probe-observed liveness, not just the
// handshake. The runtime implementation patches each peer's MeshPeer status;
// tests supply a recorder. Like PeerLister it is a function so the Prober stays
// free of Kubernetes types. Optional: when nil, results stay local (metrics and
// the in-memory Observations).
type Publisher func(ctx context.Context, results []Result) error

// Prober periodically dials every connected peer's responder and records the
// observed liveness, the runtime provider of the slo.Liveness signal. It is a
// thin controller-runtime Runnable: the decisions (which peers, how to classify)
// live in the pure functions above, and this shell only schedules the dials and
// updates the metrics. The live cross-cluster path is exercised by the e2e
// suite, not by unit tests.
type Prober struct {
	// Interval is the dial cadence; defaults to DefaultInterval.
	Interval time.Duration
	// Timeout caps each dial; defaults to DefaultTimeout. Ignored when Probe is
	// supplied by a test.
	Timeout time.Duration
	// Peers lists the peers to probe each cycle. Required.
	Peers PeerLister
	// Probe dials one target; defaults to httpProbe(Timeout).
	Probe ProbeFunc
	// Publish persists each cycle's results back onto the mesh. Optional.
	Publish Publisher
	// Now returns the current epoch seconds; defaults to time.Now().Unix.
	Now func() int64
	// Log is optional.
	Log logr.Logger

	obs Observations
}

// NeedLeaderElection makes the prober run on every pod: each node probes the
// mesh from its own vantage point.
func (p *Prober) NeedLeaderElection() bool { return false }

// Observations exposes the prober's current record so a reader (the metrics
// collector, or a future status writer) can project it into slo.Liveness.
func (p *Prober) Observations() *Observations { return &p.obs }

// Start runs the probe loop until the context is cancelled, satisfying
// manager.Runnable. It probes once immediately so the first signal is available
// without waiting a full interval, then on every tick.
func (p *Prober) Start(ctx context.Context) error {
	interval := p.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	if p.Probe == nil {
		timeout := p.Timeout
		if timeout <= 0 {
			timeout = DefaultTimeout
		}
		p.Probe = httpProbe(timeout)
	}
	if p.Now == nil {
		p.Now = func() int64 { return time.Now().Unix() }
	}

	p.Log.Info("active mesh prober started", "interval", interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	p.cycle(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			p.cycle(ctx)
		}
	}
}

// cycle plans the targets from the current peers and dials each in turn,
// recording every outcome. Dials are sequential: the target count is the small
// number of peer clusters, and a bounded per-dial timeout keeps a cycle short.
func (p *Prober) cycle(ctx context.Context) {
	peers, err := p.Peers(ctx)
	if err != nil {
		p.Log.Info("listing peers for probe cycle failed; skipping", "err", err)
		return
	}
	targets := PlanTargets(peers)
	results := make([]Result, 0, len(targets))
	for _, t := range targets {
		res := p.Probe(ctx, t)
		p.obs.Record(res)
		p.record(res)
		results = append(results, res)
	}
	if p.Publish != nil && len(results) > 0 {
		if err := p.Publish(ctx, results); err != nil {
			p.Log.Info("publishing probe results to MeshPeer status failed", "err", err)
		}
	}
}

// record updates the metrics and logs for one result.
func (p *Prober) record(res Result) {
	if res.OK {
		dwxmetrics.ProbeResults.WithLabelValues(res.ClusterID, "success").Inc()
		dwxmetrics.ProbeRTT.Observe(float64(res.RTTMillis) / 1000.0)
		p.Log.V(1).Info("probe ok", "clusterID", res.ClusterID, "rttMillis", res.RTTMillis)
		return
	}
	dwxmetrics.ProbeResults.WithLabelValues(res.ClusterID, "failure").Inc()
	p.Log.Info("probe failed", "clusterID", res.ClusterID, "reason", res.Reason)
}

// httpProbe is the production dialer: a GET to the responder path, classified by
// the pure Classify. The body read is capped so a misbehaving endpoint cannot
// exhaust memory.
func httpProbe(timeout time.Duration) ProbeFunc {
	client := &http.Client{Timeout: timeout}
	return func(ctx context.Context, t Target) Result {
		now := time.Now()
		url := "http://" + t.Address + ResponderPath
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return Classify(t.ClusterID, 0, nil, 0, now.Unix(), err)
		}
		resp, err := client.Do(req)
		rtt := time.Since(now)
		if err != nil {
			return Classify(t.ClusterID, 0, nil, rtt, now.Unix(), err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxProbeBody))
		return Classify(t.ClusterID, resp.StatusCode, body, rtt, now.Unix(), nil)
	}
}
