package dns_test

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"

	mcsv1alpha1 "github.com/datawerx/datawerx/pkg/apis/multicluster/v1alpha1"
	"github.com/datawerx/datawerx/pkg/dns"
)

func TestFQDN(t *testing.T) {
	tests := []struct {
		name, ns, want string
	}{
		{"payments", "prod", "payments.prod.svc.clusterset.local."},
		{"db", "default", "db.default.svc.clusterset.local."},
	}
	for _, tt := range tests {
		if got := dns.FQDN(tt.name, tt.ns); got != tt.want {
			t.Errorf("FQDN(%q,%q) = %q, want %q", tt.name, tt.ns, got, tt.want)
		}
	}
}

func port(name string, p int32, proto corev1.Protocol) mcsv1alpha1.ServicePort {
	return mcsv1alpha1.ServicePort{Name: name, Port: p, Protocol: proto}
}

func TestPlanServiceImport(t *testing.T) {
	tests := []struct {
		name    string
		exports []dns.ExportedEndpoint
		want    dns.ImportPlan
	}{
		{
			name:    "no exports -> does not exist",
			exports: nil,
			want:    dns.ImportPlan{},
		},
		{
			name: "single cluster ClusterSetIP",
			exports: []dns.ExportedEndpoint{
				{Cluster: "a", Type: mcsv1alpha1.ClusterSetIP,
					Ports: []mcsv1alpha1.ServicePort{port("http", 80, corev1.ProtocolTCP)},
					IPs:   []string{"10.96.0.10"}},
			},
			want: dns.ImportPlan{
				Exists:        true,
				Type:          mcsv1alpha1.ClusterSetIP,
				Ports:         []mcsv1alpha1.ServicePort{port("http", 80, corev1.ProtocolTCP)},
				AggregatedIPs: []string{"10.96.0.10"},
				Clusters:      []string{"a"},
			},
		},
		{
			name: "two clusters merge ports and IPs, order-independent",
			exports: []dns.ExportedEndpoint{
				{Cluster: "b", Type: mcsv1alpha1.ClusterSetIP,
					Ports: []mcsv1alpha1.ServicePort{port("grpc", 9000, corev1.ProtocolTCP)},
					IPs:   []string{"10.97.0.5"}},
				{Cluster: "a", Type: mcsv1alpha1.ClusterSetIP,
					Ports: []mcsv1alpha1.ServicePort{port("http", 80, corev1.ProtocolTCP)},
					IPs:   []string{"10.96.0.10"}},
			},
			want: dns.ImportPlan{
				Exists: true,
				Type:   mcsv1alpha1.ClusterSetIP,
				Ports: []mcsv1alpha1.ServicePort{
					port("http", 80, corev1.ProtocolTCP),
					port("grpc", 9000, corev1.ProtocolTCP),
				},
				AggregatedIPs: []string{"10.96.0.10", "10.97.0.5"},
				Clusters:      []string{"a", "b"},
			},
		},
		{
			name: "duplicate identical port is de-duplicated",
			exports: []dns.ExportedEndpoint{
				{Cluster: "a", Type: mcsv1alpha1.ClusterSetIP, Ports: []mcsv1alpha1.ServicePort{port("http", 80, corev1.ProtocolTCP)}},
				{Cluster: "b", Type: mcsv1alpha1.ClusterSetIP, Ports: []mcsv1alpha1.ServicePort{port("http", 80, corev1.ProtocolTCP)}},
			},
			want: dns.ImportPlan{
				Exists:   true,
				Type:     mcsv1alpha1.ClusterSetIP,
				Ports:    []mcsv1alpha1.ServicePort{port("http", 80, corev1.ProtocolTCP)},
				Clusters: []string{"a", "b"},
			},
		},
		{
			name: "empty protocol defaults to TCP",
			exports: []dns.ExportedEndpoint{
				{Cluster: "a", Type: mcsv1alpha1.ClusterSetIP, Ports: []mcsv1alpha1.ServicePort{port("http", 80, "")}},
			},
			want: dns.ImportPlan{
				Exists:   true,
				Type:     mcsv1alpha1.ClusterSetIP,
				Ports:    []mcsv1alpha1.ServicePort{port("http", 80, corev1.ProtocolTCP)},
				Clusters: []string{"a"},
			},
		},
		{
			name: "type conflict -> lowest cluster wins, loser reported",
			exports: []dns.ExportedEndpoint{
				{Cluster: "z", Type: mcsv1alpha1.Headless, Ports: []mcsv1alpha1.ServicePort{port("http", 80, corev1.ProtocolTCP)}},
				{Cluster: "a", Type: mcsv1alpha1.ClusterSetIP, Ports: []mcsv1alpha1.ServicePort{port("http", 80, corev1.ProtocolTCP)}},
			},
			want: dns.ImportPlan{
				Exists:    true,
				Type:      mcsv1alpha1.ClusterSetIP,
				Ports:     []mcsv1alpha1.ServicePort{port("http", 80, corev1.ProtocolTCP)},
				Clusters:  []string{"a"},
				Conflicts: []string{`cluster "z": type "Headless" conflicts with resolved type "ClusterSetIP"`},
			},
		},
		{
			name: "port name conflict on same port/proto is reported",
			exports: []dns.ExportedEndpoint{
				{Cluster: "a", Type: mcsv1alpha1.ClusterSetIP, Ports: []mcsv1alpha1.ServicePort{port("http", 80, corev1.ProtocolTCP)}},
				{Cluster: "b", Type: mcsv1alpha1.ClusterSetIP, Ports: []mcsv1alpha1.ServicePort{port("web", 80, corev1.ProtocolTCP)}},
			},
			want: dns.ImportPlan{
				Exists:    true,
				Type:      mcsv1alpha1.ClusterSetIP,
				Ports:     []mcsv1alpha1.ServicePort{port("http", 80, corev1.ProtocolTCP)},
				Clusters:  []string{"a", "b"},
				Conflicts: []string{`cluster "b": port 80/TCP name "web" conflicts with "http"`},
			},
		},
		{
			name: "headless service, no IPs aggregated",
			exports: []dns.ExportedEndpoint{
				{Cluster: "a", Type: mcsv1alpha1.Headless, Ports: []mcsv1alpha1.ServicePort{port("http", 80, corev1.ProtocolTCP)}},
			},
			want: dns.ImportPlan{
				Exists:   true,
				Type:     mcsv1alpha1.Headless,
				Ports:    []mcsv1alpha1.ServicePort{port("http", 80, corev1.ProtocolTCP)},
				Clusters: []string{"a"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dns.PlanServiceImport(tt.exports)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("PlanServiceImport() mismatch\n got: %#v\nwant: %#v", got, tt.want)
			}
		})
	}
}

// TestPlanServiceImport_OldestExportWins verifies the canonical MCS rule: on a
// type disagreement the oldest ServiceExport wins, overriding the lowest-cluster
// tie-breaker. Here cluster "a" is alphabetically first but exported later, so
// the older "z" export decides the resolved type.
func TestPlanServiceImport_OldestExportWins(t *testing.T) {
	plan := dns.PlanServiceImport([]dns.ExportedEndpoint{
		{Cluster: "a", Type: mcsv1alpha1.Headless, ExportTime: 200},
		{Cluster: "z", Type: mcsv1alpha1.ClusterSetIP, ExportTime: 100}, // older
	})
	if plan.Type != mcsv1alpha1.ClusterSetIP {
		t.Errorf("oldest export should win: want ClusterSetIP, got %q", plan.Type)
	}
	if len(plan.Clusters) != 1 || plan.Clusters[0] != "z" {
		t.Errorf("only the older export should contribute: %v", plan.Clusters)
	}
	if !plan.HasConflicts() {
		t.Error("the younger, differently-typed export should be reported as a conflict")
	}
}

func TestImportPlan_HasConflicts(t *testing.T) {
	clean := dns.PlanServiceImport([]dns.ExportedEndpoint{
		{Cluster: "a", Type: mcsv1alpha1.ClusterSetIP},
	})
	if clean.HasConflicts() {
		t.Error("did not expect conflicts")
	}
	conflicted := dns.PlanServiceImport([]dns.ExportedEndpoint{
		{Cluster: "a", Type: mcsv1alpha1.ClusterSetIP},
		{Cluster: "b", Type: mcsv1alpha1.Headless},
	})
	if !conflicted.HasConflicts() {
		t.Error("expected conflicts")
	}
}
