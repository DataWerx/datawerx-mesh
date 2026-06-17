// Package metrics defines DataWerx Mesh's Prometheus instrumentation and
// registers it on the controller-runtime metrics registry, which the manager
// already serves at /metrics exposed by the Helm chart's metrics Service.
//
// Two kinds of metric live here:
//
//   - Event metrics (counters/histograms) incremented inline by the DNS server
//     and the NAT reconciler.
//   - State metrics, produced at scrape time by a cache-backed collector that
//     lists the current objects — always accurate, with no per-reconcile
//     bookkeeping or stale series.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

const namespace = "dwx"

var (
	// DNSQueries counts clusterset.local DNS queries answered, by response code.
	DNSQueries = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "dns", Name: "queries_total",
		Help: "Total clusterset.local DNS queries answered, labeled by response code.",
	}, []string{"rcode"})

	// DNSQueryDuration observes clusterset.local query handling latency.
	DNSQueryDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "dns", Name: "query_duration_seconds",
		Help:    "clusterset.local DNS query handling latency in seconds.",
		Buckets: []float64{0.0001, 0.0005, 0.001, 0.005, 0.01, 0.05, 0.1},
	})

	// NATSyncs counts ClusterSetIP NAT data-plane syncs, by result.
	NATSyncs = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "clusterset_nat", Name: "syncs_total",
		Help: "Total ClusterSetIP NAT data-plane syncs, labeled by result (success|error).",
	}, []string{"result"})

	// RemapSyncs counts overlapping-CIDR NETMAP data-plane syncs, by result.
	RemapSyncs = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "remap", Name: "syncs_total",
		Help: "Total overlap NETMAP data-plane syncs, labeled by result (success|error).",
	}, []string{"result"})

	// RemapEntries is the number of active local real⇄virtual NETMAP pairs the
	// node is currently programming for overlapping peers. A gauge: set to the
	// full-state count on each successful remap sync.
	RemapEntries = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: "remap", Name: "active_entries",
		Help: "Number of overlapping-CIDR NETMAP entries currently programmed.",
	})

	// ProbeResults counts active synthetic probes the node has dialed, by the
	// cluster probed and the result (success|failure). A growing failure series
	// for a connected peer is the live "should reach, can't" signal.
	ProbeResults = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "probe", Name: "results_total",
		Help: "Total active synthetic probes dialed, labeled by target cluster and result (success|failure).",
	}, []string{"cluster", "result"})

	// ProbeRTT observes the round-trip latency of successful probes, the
	// application-layer counterpart to the WireGuard handshake age.
	ProbeRTT = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "probe", Name: "rtt_seconds",
		Help:    "Round-trip latency of successful active mesh probes in seconds.",
		Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1},
	})
)

// Register installs all DataWerx metrics — the event collectors above plus the
// cache-backed state collector — onto the controller-runtime registry. It
// tolerates AlreadyRegistered so repeated calls (e.g. in tests) are safe.
func Register(reader client.Reader) error {
	collectors := []prometheus.Collector{
		DNSQueries,
		DNSQueryDuration,
		NATSyncs,
		RemapSyncs,
		RemapEntries,
		ProbeResults,
		ProbeRTT,
		newMeshCollector(reader),
	}
	for _, c := range collectors {
		if err := ctrlmetrics.Registry.Register(c); err != nil {
			if _, ok := err.(prometheus.AlreadyRegisteredError); !ok {
				return err
			}
		}
	}
	return nil
}
