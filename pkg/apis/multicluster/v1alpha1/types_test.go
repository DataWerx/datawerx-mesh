package v1alpha1_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	mcsv1alpha1 "github.com/datawerx/datawerx/pkg/apis/multicluster/v1alpha1"
)

func TestServiceImport_DeepCopyIndependence(t *testing.T) {
	appProto := "http"
	original := &mcsv1alpha1.ServiceImport{
		ObjectMeta: metav1.ObjectMeta{Name: "payments", Namespace: "prod"},
		Spec: mcsv1alpha1.ServiceImportSpec{
			Type: mcsv1alpha1.ClusterSetIP,
			IPs:  []string{"241.0.0.5"},
			Ports: []mcsv1alpha1.ServicePort{
				{Name: "http", Port: 80, Protocol: corev1.ProtocolTCP, AppProtocol: &appProto},
			},
		},
		Status: mcsv1alpha1.ServiceImportStatus{
			Clusters: []mcsv1alpha1.ClusterStatus{{Cluster: "a"}},
		},
	}

	clone := original.DeepCopy()
	clone.Spec.IPs[0] = "241.0.0.99"
	*clone.Spec.Ports[0].AppProtocol = "grpc"
	clone.Spec.Ports[0].Name = "changed"
	clone.Status.Clusters[0].Cluster = "b"

	if original.Spec.IPs[0] != "241.0.0.5" {
		t.Errorf("IPs leaked: %v", original.Spec.IPs)
	}
	if *original.Spec.Ports[0].AppProtocol != "http" {
		t.Errorf("AppProtocol pointer leaked: %q", *original.Spec.Ports[0].AppProtocol)
	}
	if original.Spec.Ports[0].Name != "http" {
		t.Errorf("Port name leaked: %q", original.Spec.Ports[0].Name)
	}
	if original.Status.Clusters[0].Cluster != "a" {
		t.Errorf("Status cluster leaked: %q", original.Status.Clusters[0].Cluster)
	}
}

func TestServiceExport_DeepCopyObject(t *testing.T) {
	se := &mcsv1alpha1.ServiceExport{
		ObjectMeta: metav1.ObjectMeta{Name: "payments", Namespace: "prod"},
		Status: mcsv1alpha1.ServiceExportStatus{
			Conditions: []metav1.Condition{{Type: mcsv1alpha1.ServiceExportValid, Status: metav1.ConditionTrue}},
		},
	}
	clone, ok := se.DeepCopyObject().(*mcsv1alpha1.ServiceExport)
	if !ok {
		t.Fatal("DeepCopyObject returned wrong type")
	}
	clone.Status.Conditions[0].Status = metav1.ConditionFalse
	if se.Status.Conditions[0].Status != metav1.ConditionTrue {
		t.Error("condition leaked into original")
	}

	list := &mcsv1alpha1.ServiceImportList{Items: []mcsv1alpha1.ServiceImport{{}}}
	if l, ok := list.DeepCopyObject().(*mcsv1alpha1.ServiceImportList); !ok || len(l.Items) != 1 {
		t.Error("ServiceImportList.DeepCopyObject failed")
	}
}
