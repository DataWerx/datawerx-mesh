package metrics

import (
	"sort"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	mcsv1alpha1 "github.com/datawerx/datawerx/pkg/apis/multicluster/v1alpha1"
	networkingv1alpha1 "github.com/datawerx/datawerx/pkg/apis/networking/v1alpha1"
)

func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := networkingv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("networking scheme: %v", err)
	}
	if err := mcsv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("mcs scheme: %v", err)
	}
	return s
}

// flatten gathers a registry into a name{sortedLabels} -> value map.
func flatten(t *testing.T, reg *prometheus.Registry) map[string]float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	out := map[string]float64{}
	for _, mf := range mfs {
		for _, m := range mf.Metric {
			labels := make([]string, 0, len(m.Label))
			for _, l := range m.Label {
				labels = append(labels, l.GetName()+"="+l.GetValue())
			}
			sort.Strings(labels)
			key := mf.GetName()
			if len(labels) > 0 {
				key += "{" + strings.Join(labels, ",") + "}"
			}
			out[key] = metricValue(m)
		}
	}
	return out
}

func metricValue(m *dto.Metric) float64 {
	switch {
	case m.Gauge != nil:
		return m.Gauge.GetValue()
	case m.Counter != nil:
		return m.Counter.GetValue()
	default:
		return 0
	}
}

func TestMeshCollector(t *testing.T) {
	objs := []client.Object{
		&networkingv1alpha1.MeshPeer{
			ObjectMeta: metav1.ObjectMeta{Name: "a"},
			Spec:       networkingv1alpha1.MeshPeerSpec{ClusterID: "ca", PublicKey: "ka"},
			Status:     networkingv1alpha1.MeshPeerStatus{Phase: networkingv1alpha1.MeshPeerPhaseConnected, LastHandshakeTime: 1717000000},
		},
		&networkingv1alpha1.MeshPeer{
			ObjectMeta: metav1.ObjectMeta{Name: "b"},
			Spec:       networkingv1alpha1.MeshPeerSpec{ClusterID: "cb", PublicKey: "kb"},
			Status:     networkingv1alpha1.MeshPeerStatus{Phase: networkingv1alpha1.MeshPeerPhaseError},
		},
		&mcsv1alpha1.ServiceExport{
			ObjectMeta: metav1.ObjectMeta{Name: "payments", Namespace: "prod"},
			Status: mcsv1alpha1.ServiceExportStatus{Conditions: []metav1.Condition{
				{Type: mcsv1alpha1.ServiceExportValid, Status: metav1.ConditionTrue, Reason: "Exported", LastTransitionTime: metav1.Now()},
			}},
		},
		&mcsv1alpha1.ServiceExport{
			ObjectMeta: metav1.ObjectMeta{Name: "ghost", Namespace: "prod"},
			Status: mcsv1alpha1.ServiceExportStatus{Conditions: []metav1.Condition{
				{Type: mcsv1alpha1.ServiceExportValid, Status: metav1.ConditionFalse, Reason: "ServiceNotFound", LastTransitionTime: metav1.Now()},
			}},
		},
		&mcsv1alpha1.ServiceImport{
			ObjectMeta: metav1.ObjectMeta{Name: "payments", Namespace: "prod"},
			Spec:       mcsv1alpha1.ServiceImportSpec{Type: mcsv1alpha1.ClusterSetIP, IPs: []string{"241.0.0.5"}},
		},
		&mcsv1alpha1.ServiceImport{
			ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "data"},
			Spec:       mcsv1alpha1.ServiceImportSpec{Type: mcsv1alpha1.Headless},
		},
		&networkingv1alpha1.EndpointExport{ObjectMeta: metav1.ObjectMeta{Name: "a-payments", Namespace: "prod"}},
		&networkingv1alpha1.EndpointExport{ObjectMeta: metav1.ObjectMeta{Name: "b-payments", Namespace: "prod"}},
	}

	fc := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(objs...).Build()
	reg := prometheus.NewRegistry()
	reg.MustRegister(newMeshCollector(fc))

	got := flatten(t, reg)
	want := map[string]float64{
		"dwx_meshpeers{phase=Connected}":                               1,
		"dwx_meshpeers{phase=Pending}":                                 0,
		"dwx_meshpeers{phase=Error}":                                   1,
		"dwx_meshpeer_last_handshake_timestamp_seconds{cluster_id=ca}": 1717000000,
		"dwx_meshpeer_last_handshake_timestamp_seconds{cluster_id=cb}": 0,
		"dwx_serviceexports{valid=true}":                               1,
		"dwx_serviceexports{valid=false}":                              1,
		"dwx_serviceimports{type=ClusterSetIP}":                        1,
		"dwx_serviceimports{type=Headless}":                            1,
		"dwx_endpointexports":                                          2,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
}

// TestMeshCollector_DuplicateClusterID guards against a duplicate cluster_id
// aborting the entire scrape: nothing enforces ClusterID uniqueness, and
// emitting one handshake series per peer would make Gather() error on the dup.
func TestMeshCollector_DuplicateClusterID(t *testing.T) {
	objs := []client.Object{
		&networkingv1alpha1.MeshPeer{
			ObjectMeta: metav1.ObjectMeta{Name: "a"},
			Spec:       networkingv1alpha1.MeshPeerSpec{ClusterID: "dup", PublicKey: "ka"},
			Status:     networkingv1alpha1.MeshPeerStatus{Phase: networkingv1alpha1.MeshPeerPhaseConnected, LastHandshakeTime: 100},
		},
		&networkingv1alpha1.MeshPeer{
			ObjectMeta: metav1.ObjectMeta{Name: "b"},
			Spec:       networkingv1alpha1.MeshPeerSpec{ClusterID: "dup", PublicKey: "kb"},
			Status:     networkingv1alpha1.MeshPeerStatus{Phase: networkingv1alpha1.MeshPeerPhaseConnected, LastHandshakeTime: 200},
		},
	}
	fc := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(objs...).Build()
	reg := prometheus.NewRegistry()
	reg.MustRegister(newMeshCollector(fc))

	// flatten calls Gather, which previously errored on the duplicate label set.
	got := flatten(t, reg)
	if v := got["dwx_meshpeer_last_handshake_timestamp_seconds{cluster_id=dup}"]; v != 200 {
		t.Errorf("deduped handshake = %v, want 200 (most recent)", v)
	}
	if got["dwx_meshpeers{phase=Connected}"] != 2 {
		t.Errorf("connected peers = %v, want 2", got["dwx_meshpeers{phase=Connected}"])
	}
}

func TestMeshCollector_ListErrorOmitsSeries(t *testing.T) {
	// A scheme missing the types makes List fail; Collect must not panic and
	// simply emits nothing.
	fc := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
	reg := prometheus.NewRegistry()
	reg.MustRegister(newMeshCollector(fc))
	if got := flatten(t, reg); len(got) != 0 {
		t.Errorf("expected no series when lists fail, got %v", got)
	}
}

func TestEventCounters(t *testing.T) {
	NATSyncs.Reset()
	NATSyncs.WithLabelValues("success").Inc()
	NATSyncs.WithLabelValues("success").Inc()
	NATSyncs.WithLabelValues("error").Inc()
	if got := testutil.ToFloat64(NATSyncs.WithLabelValues("success")); got != 2 {
		t.Errorf("nat success counter = %v, want 2", got)
	}
	if got := testutil.ToFloat64(NATSyncs.WithLabelValues("error")); got != 1 {
		t.Errorf("nat error counter = %v, want 1", got)
	}

	DNSQueries.Reset()
	DNSQueries.WithLabelValues("NOERROR").Inc()
	if got := testutil.ToFloat64(DNSQueries.WithLabelValues("NOERROR")); got != 1 {
		t.Errorf("dns NOERROR counter = %v, want 1", got)
	}
}
