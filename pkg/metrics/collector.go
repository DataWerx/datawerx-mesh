package metrics

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	mcsv1alpha1 "github.com/datawerx/datawerx/pkg/apis/multicluster/v1alpha1"
	networkingv1alpha1 "github.com/datawerx/datawerx/pkg/apis/networking/v1alpha1"
)

// meshCollector emits DataWerx state gauges at scrape time by listing the
// current objects from the controller-runtime cache. Listing is read-only and
// cheap (in-memory cache). List errors are tolerated: the affected series are
// simply omitted from that scrape.
type meshCollector struct {
	reader client.Reader

	peers        *prometheus.Desc
	handshake    *prometheus.Desc
	exportsValid *prometheus.Desc
	imports      *prometheus.Desc
	endpoints    *prometheus.Desc
}

func newMeshCollector(reader client.Reader) *meshCollector {
	return &meshCollector{
		reader: reader,
		peers: prometheus.NewDesc(
			"dwx_meshpeers", "Number of MeshPeers by status phase.",
			[]string{"phase"}, nil),
		handshake: prometheus.NewDesc(
			"dwx_meshpeer_last_handshake_timestamp_seconds",
			"Unix time of the last WireGuard handshake per peer (0 if none).",
			[]string{"cluster_id"}, nil),
		exportsValid: prometheus.NewDesc(
			"dwx_serviceexports", "Number of ServiceExports by Valid condition.",
			[]string{"valid"}, nil),
		imports: prometheus.NewDesc(
			"dwx_serviceimports", "Number of ServiceImports by type.",
			[]string{"type"}, nil),
		endpoints: prometheus.NewDesc(
			"dwx_endpointexports", "Number of EndpointExport objects.",
			nil, nil),
	}
}

func (c *meshCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.peers
	ch <- c.handshake
	ch <- c.exportsValid
	ch <- c.imports
	ch <- c.endpoints
}

func (c *meshCollector) Collect(ch chan<- prometheus.Metric) {
	ctx := context.Background()

	c.collectPeers(ctx, ch)
	c.collectExports(ctx, ch)
	c.collectImports(ctx, ch)
	c.collectEndpoints(ctx, ch)
}

func (c *meshCollector) collectPeers(ctx context.Context, ch chan<- prometheus.Metric) {
	var peers networkingv1alpha1.MeshPeerList
	if err := c.reader.List(ctx, &peers); err != nil {
		return
	}
	// Seed the known phases so each series is always present (0 when empty).
	byPhase := map[string]int{
		string(networkingv1alpha1.MeshPeerPhasePending):   0,
		string(networkingv1alpha1.MeshPeerPhaseConnected): 0,
		string(networkingv1alpha1.MeshPeerPhaseError):     0,
	}
	// The handshake gauge is labeled solely by cluster_id, but nothing enforces
	// ClusterID uniqueness across MeshPeers. Emitting one series per peer would
	// make Gather() fail ("collected before with the same name and label values")
	// on any duplicate — aborting the WHOLE collector and dropping every dwx_*
	// series, not just the dup. Dedup first, keeping the most recent handshake.
	handshakeByCluster := map[string]int64{}
	seen := map[string]struct{}{}
	for i := range peers.Items {
		p := &peers.Items[i]
		phase := string(p.Status.Phase)
		if phase == "" {
			phase = string(networkingv1alpha1.MeshPeerPhasePending)
		}
		byPhase[phase]++

		cid := p.Spec.ClusterID
		if _, ok := seen[cid]; !ok || p.Status.LastHandshakeTime > handshakeByCluster[cid] {
			handshakeByCluster[cid] = p.Status.LastHandshakeTime
			seen[cid] = struct{}{}
		}
	}
	for cid, ts := range handshakeByCluster {
		ch <- prometheus.MustNewConstMetric(
			c.handshake, prometheus.GaugeValue, float64(ts), cid)
	}
	for phase, n := range byPhase {
		ch <- prometheus.MustNewConstMetric(c.peers, prometheus.GaugeValue, float64(n), phase)
	}
}

func (c *meshCollector) collectExports(ctx context.Context, ch chan<- prometheus.Metric) {
	var exports mcsv1alpha1.ServiceExportList
	if err := c.reader.List(ctx, &exports); err != nil {
		return
	}
	var valid, invalid int
	for i := range exports.Items {
		cond := apimeta.FindStatusCondition(exports.Items[i].Status.Conditions, mcsv1alpha1.ServiceExportValid)
		if cond != nil && cond.Status == metav1.ConditionTrue {
			valid++
		} else {
			invalid++
		}
	}
	ch <- prometheus.MustNewConstMetric(c.exportsValid, prometheus.GaugeValue, float64(valid), "true")
	ch <- prometheus.MustNewConstMetric(c.exportsValid, prometheus.GaugeValue, float64(invalid), "false")
}

func (c *meshCollector) collectImports(ctx context.Context, ch chan<- prometheus.Metric) {
	var imports mcsv1alpha1.ServiceImportList
	if err := c.reader.List(ctx, &imports); err != nil {
		return
	}
	var clusterSetIP, headless int
	for i := range imports.Items {
		switch imports.Items[i].Spec.Type {
		case mcsv1alpha1.ClusterSetIP:
			clusterSetIP++
		case mcsv1alpha1.Headless:
			headless++
		}
	}
	ch <- prometheus.MustNewConstMetric(c.imports, prometheus.GaugeValue, float64(clusterSetIP), "ClusterSetIP")
	ch <- prometheus.MustNewConstMetric(c.imports, prometheus.GaugeValue, float64(headless), "Headless")
}

func (c *meshCollector) collectEndpoints(ctx context.Context, ch chan<- prometheus.Metric) {
	var eps networkingv1alpha1.EndpointExportList
	if err := c.reader.List(ctx, &eps); err != nil {
		return
	}
	ch <- prometheus.MustNewConstMetric(c.endpoints, prometheus.GaugeValue, float64(len(eps.Items)))
}
