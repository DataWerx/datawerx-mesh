//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	mcsv1alpha1 "github.com/DataWerx/datawerx-mesh/pkg/apis/multicluster/v1alpha1"
	networkingv1alpha1 "github.com/DataWerx/datawerx-mesh/pkg/apis/networking/v1alpha1"
	"github.com/DataWerx/datawerx-mesh/pkg/controllers"
)

func req(ns, name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}
}

// TestMeshPeerReconciler_Integration drives the reconciler against a real API
// server: finalizer installation, status subresource patch, and convergence.
func TestMeshPeerReconciler_Integration(t *testing.T) {
	c, scheme, stop := startEnv(t)
	defer stop()
	ctx := context.Background()

	peer := &networkingv1alpha1.MeshPeer{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-b"},
		Spec: networkingv1alpha1.MeshPeerSpec{
			ClusterID: "cluster-b", PublicKey: "kb", PodCIDRs: []string{"10.50.0.0/16"},
		},
	}
	if err := c.Create(ctx, peer); err != nil {
		t.Fatalf("create MeshPeer: %v", err)
	}

	dp := newFakePeerDataPlane()
	r := &controllers.MeshPeerReconciler{Client: c, Scheme: scheme, DataPlane: dp}

	// Pass 1 installs the finalizer.
	if _, err := r.Reconcile(ctx, req("", "cluster-b")); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	var got networkingv1alpha1.MeshPeer
	if err := c.Get(ctx, types.NamespacedName{Name: "cluster-b"}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Finalizers) == 0 {
		t.Fatalf("expected finalizer to be persisted by the API server")
	}

	// Pass 2 programs the peer and patches status (real status subresource).
	if _, err := r.Reconcile(ctx, req("", "cluster-b")); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	if !dp.wasConfigured("kb") {
		t.Error("expected ConfigurePeer to be called")
	}
	if err := c.Get(ctx, types.NamespacedName{Name: "cluster-b"}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.Phase != networkingv1alpha1.MeshPeerPhaseConnected {
		t.Errorf("phase = %q, want Connected", got.Status.Phase)
	}
}

// TestServiceExportImport_Integration runs the export and import controllers
// end to end through the API server: export validates and publishes an
// EndpointExport, import aggregates it into a ServiceImport with a ClusterSetIP.
func TestServiceExportImport_Integration(t *testing.T) {
	c, scheme, stop := startEnv(t)
	defer stop()
	ctx := context.Background()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "prod"}}
	if err := c.Create(ctx, ns); err != nil {
		t.Fatalf("create ns: %v", err)
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "payments", Namespace: "prod"},
		Spec: corev1.ServiceSpec{
			ClusterIP: "10.96.0.10",
			Ports:     []corev1.ServicePort{{Name: "http", Port: 80, Protocol: corev1.ProtocolTCP}},
		},
	}
	if err := c.Create(ctx, svc); err != nil {
		t.Fatalf("create svc: %v", err)
	}
	exp := &mcsv1alpha1.ServiceExport{ObjectMeta: metav1.ObjectMeta{Name: "payments", Namespace: "prod"}}
	if err := c.Create(ctx, exp); err != nil {
		t.Fatalf("create ServiceExport: %v", err)
	}

	export := &controllers.ServiceExportReconciler{Client: c, Scheme: scheme, ClusterID: "cluster-a"}
	// Pass 1 finalizer, pass 2 publish + validate.
	for i := 0; i < 2; i++ {
		if _, err := export.Reconcile(ctx, req("prod", "payments")); err != nil {
			t.Fatalf("export reconcile %d: %v", i, err)
		}
	}

	// The Valid condition must be persisted on the status subresource.
	var gotExp mcsv1alpha1.ServiceExport
	if err := c.Get(ctx, types.NamespacedName{Namespace: "prod", Name: "payments"}, &gotExp); err != nil {
		t.Fatalf("get export: %v", err)
	}
	if cond := apimeta.FindStatusCondition(gotExp.Status.Conditions, mcsv1alpha1.ServiceExportValid); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("expected Valid=True condition, got %#v", gotExp.Status.Conditions)
	}

	// The EndpointExport must have been created.
	var ee networkingv1alpha1.EndpointExport
	if err := c.Get(ctx, types.NamespacedName{Namespace: "prod", Name: "cluster-a-payments"}, &ee); err != nil {
		t.Fatalf("expected EndpointExport to be published: %v", err)
	}

	// Now the import controller should converge a ClusterSetIP ServiceImport.
	imp := &controllers.ServiceImportReconciler{Client: c, Scheme: scheme}
	if _, err := imp.Reconcile(ctx, req("prod", "payments")); err != nil {
		t.Fatalf("import reconcile: %v", err)
	}
	var si mcsv1alpha1.ServiceImport
	if err := waitGet(ctx, c, "prod", "payments", &si); err != nil {
		t.Fatalf("expected ServiceImport: %v", err)
	}
	if si.Spec.Type != mcsv1alpha1.ClusterSetIP || len(si.Spec.IPs) == 0 {
		t.Errorf("ServiceImport = %#v, want ClusterSetIP with an allocated IP", si.Spec)
	}
}

// TestCRDValidation_Integration confirms the API server enforces our CRD
// schema: required fields and enums are rejected.
func TestCRDValidation_Integration(t *testing.T) {
	c, _, stop := startEnv(t)
	defer stop()
	ctx := context.Background()

	// MeshPeer requires clusterID + publicKey.
	bad := &networkingv1alpha1.MeshPeer{ObjectMeta: metav1.ObjectMeta{Name: "bad"}}
	if err := c.Create(ctx, bad); err == nil {
		t.Error("expected API server to reject MeshPeer missing required fields")
	}

	// ServiceImport.spec.type is an enum.
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "v"}}); err != nil {
		t.Fatalf("create ns: %v", err)
	}
	badType := &mcsv1alpha1.ServiceImport{
		ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "v"},
		Spec:       mcsv1alpha1.ServiceImportSpec{Type: mcsv1alpha1.ServiceImportType("Bogus")},
	}
	if err := c.Create(ctx, badType); err == nil {
		t.Error("expected API server to reject ServiceImport with an invalid type enum")
	}
}

// waitGet polls for an object to appear (controllers may need a beat).
func waitGet(ctx context.Context, c client.Client, ns, name string, obj client.Object) error {
	deadline := time.Now().Add(10 * time.Second)
	var err error
	for time.Now().Before(deadline) {
		err = c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, obj)
		if err == nil {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return err
}
