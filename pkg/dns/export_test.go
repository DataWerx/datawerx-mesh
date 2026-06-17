package dns_test

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"

	mcsv1alpha1 "github.com/datawerx/datawerx/pkg/apis/multicluster/v1alpha1"
	"github.com/datawerx/datawerx/pkg/dns"
)

func strptr(s string) *string { return &s }

func TestServiceIsHeadless(t *testing.T) {
	headless := &corev1.Service{Spec: corev1.ServiceSpec{ClusterIP: corev1.ClusterIPNone}}
	if !dns.ServiceIsHeadless(headless) {
		t.Error("expected headless")
	}
	normal := &corev1.Service{Spec: corev1.ServiceSpec{ClusterIP: "10.96.0.1"}}
	if dns.ServiceIsHeadless(normal) {
		t.Error("did not expect headless")
	}
}

func TestBuildExportedEndpoint(t *testing.T) {
	tests := []struct {
		name string
		svc  *corev1.Service
		want dns.ExportedEndpoint
	}{
		{
			name: "ClusterIP service with single IP and appProtocol",
			svc: &corev1.Service{
				Spec: corev1.ServiceSpec{
					ClusterIP: "10.96.0.10",
					Ports: []corev1.ServicePort{
						{Name: "http", Protocol: corev1.ProtocolTCP, Port: 80, AppProtocol: strptr("http")},
					},
				},
			},
			want: dns.ExportedEndpoint{
				Cluster: "c1",
				Type:    mcsv1alpha1.ClusterSetIP,
				IPs:     []string{"10.96.0.10"},
				Ports: []mcsv1alpha1.ServicePort{
					{Name: "http", Protocol: corev1.ProtocolTCP, Port: 80, AppProtocol: strptr("http")},
				},
			},
		},
		{
			name: "dual-stack ClusterIPs preferred over singular",
			svc: &corev1.Service{
				Spec: corev1.ServiceSpec{
					ClusterIP:  "10.96.0.10",
					ClusterIPs: []string{"10.96.0.10", "fd00::10"},
					Ports:      []corev1.ServicePort{{Port: 443, Protocol: corev1.ProtocolTCP}},
				},
			},
			want: dns.ExportedEndpoint{
				Cluster: "c1",
				Type:    mcsv1alpha1.ClusterSetIP,
				IPs:     []string{"10.96.0.10", "fd00::10"},
				Ports:   []mcsv1alpha1.ServicePort{{Port: 443, Protocol: corev1.ProtocolTCP}},
			},
		},
		{
			name: "headless service has no IPs and is Headless type",
			svc: &corev1.Service{
				Spec: corev1.ServiceSpec{
					ClusterIP: corev1.ClusterIPNone,
					Ports:     []corev1.ServicePort{{Name: "db", Protocol: corev1.ProtocolTCP, Port: 5432}},
				},
			},
			want: dns.ExportedEndpoint{
				Cluster: "c1",
				Type:    mcsv1alpha1.Headless,
				Ports:   []mcsv1alpha1.ServicePort{{Name: "db", Protocol: corev1.ProtocolTCP, Port: 5432}},
			},
		},
		{
			name: "empty/None clusterIP entries are filtered",
			svc: &corev1.Service{
				Spec: corev1.ServiceSpec{
					ClusterIPs: []string{"", "10.96.0.20"},
					Ports:      []corev1.ServicePort{{Port: 8080}},
				},
			},
			want: dns.ExportedEndpoint{
				Cluster: "c1",
				Type:    mcsv1alpha1.ClusterSetIP,
				IPs:     []string{"10.96.0.20"},
				Ports:   []mcsv1alpha1.ServicePort{{Port: 8080}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dns.BuildExportedEndpoint("c1", tt.svc)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("BuildExportedEndpoint() mismatch\n got: %#v\nwant: %#v", got, tt.want)
			}
		})
	}
}

// TestBuildExportedEndpoint_AppProtocolIndependent ensures the appProtocol
// pointer is copied, not aliased to the Service's.
func TestBuildExportedEndpoint_AppProtocolIndependent(t *testing.T) {
	svc := &corev1.Service{Spec: corev1.ServiceSpec{
		ClusterIP: "10.96.0.10",
		Ports:     []corev1.ServicePort{{Port: 80, AppProtocol: strptr("http")}},
	}}
	ep := dns.BuildExportedEndpoint("c1", svc)
	*svc.Spec.Ports[0].AppProtocol = "grpc"
	if *ep.Ports[0].AppProtocol != "http" {
		t.Errorf("appProtocol aliased the source: %q", *ep.Ports[0].AppProtocol)
	}
}
