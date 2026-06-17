package v1alpha1_test

import (
	"reflect"
	"testing"

	networkingv1alpha1 "github.com/datawerx/datawerx/pkg/apis/networking/v1alpha1"
)

func TestMeshPeerSpec_AllCIDRs(t *testing.T) {
	tests := []struct {
		name string
		spec networkingv1alpha1.MeshPeerSpec
		want []string
	}{
		{
			name: "pod and service combined in order",
			spec: networkingv1alpha1.MeshPeerSpec{
				PodCIDRs:     []string{"10.50.0.0/16", "10.51.0.0/16"},
				ServiceCIDRs: []string{"10.96.0.0/12"},
			},
			want: []string{"10.50.0.0/16", "10.51.0.0/16", "10.96.0.0/12"},
		},
		{
			name: "only pods",
			spec: networkingv1alpha1.MeshPeerSpec{PodCIDRs: []string{"10.50.0.0/16"}},
			want: []string{"10.50.0.0/16"},
		},
		{
			name: "none",
			spec: networkingv1alpha1.MeshPeerSpec{},
			want: []string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.spec.AllCIDRs(); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("AllCIDRs() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestDeepCopy verifies the hand-written deepcopy produces an independent copy.
// Mutating the copy must not affect the original's slices.
func TestMeshPeer_DeepCopy(t *testing.T) {
	original := &networkingv1alpha1.MeshPeer{
		Spec: networkingv1alpha1.MeshPeerSpec{
			ClusterID:    "c1",
			PublicKey:    "k1",
			PodCIDRs:     []string{"10.0.0.0/16"},
			ServiceCIDRs: []string{"10.96.0.0/12"},
		},
		Status: networkingv1alpha1.MeshPeerStatus{
			Phase:   networkingv1alpha1.MeshPeerPhaseConnected,
			Message: "ok",
		},
	}

	clone := original.DeepCopy()
	clone.Spec.PodCIDRs[0] = "192.168.0.0/16"
	clone.Spec.ServiceCIDRs = append(clone.Spec.ServiceCIDRs, "extra")
	clone.Status.Message = "changed"

	if original.Spec.PodCIDRs[0] != "10.0.0.0/16" {
		t.Errorf("mutation of clone leaked into original PodCIDRs: %v", original.Spec.PodCIDRs)
	}
	if len(original.Spec.ServiceCIDRs) != 1 {
		t.Errorf("mutation of clone leaked into original ServiceCIDRs: %v", original.Spec.ServiceCIDRs)
	}
	if original.Status.Message != "ok" {
		t.Errorf("mutation of clone leaked into original Status: %q", original.Status.Message)
	}
}

// TestDeepCopyObject ensures the runtime.Object contract returns a usable,
// independent object for both the singular and list types.
func TestDeepCopyObject(t *testing.T) {
	mp := &networkingv1alpha1.MeshPeer{Spec: networkingv1alpha1.MeshPeerSpec{ClusterID: "c1"}}
	if obj := mp.DeepCopyObject(); obj == nil {
		t.Fatal("MeshPeer.DeepCopyObject returned nil")
	}

	list := &networkingv1alpha1.MeshPeerList{Items: []networkingv1alpha1.MeshPeer{*mp}}
	clone, ok := list.DeepCopyObject().(*networkingv1alpha1.MeshPeerList)
	if !ok {
		t.Fatal("MeshPeerList.DeepCopyObject returned wrong type")
	}
	if len(clone.Items) != 1 {
		t.Fatalf("expected 1 item in cloned list, got %d", len(clone.Items))
	}
}
