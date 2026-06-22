package edge_test

import (
	"testing"

	networkingv1alpha1 "github.com/DataWerx/datawerx-mesh/pkg/apis/networking/v1alpha1"
	"github.com/DataWerx/datawerx-mesh/pkg/edge"
)

func TestEdgeDeviceObject(t *testing.T) {
	obj := edge.EdgeDeviceObject(networkingv1alpha1.EdgeDeviceSpec{
		DeviceID:  "Press Line 7!",
		PublicKey: validKey,
	})
	if obj.Kind != "EdgeDevice" || obj.APIVersion != networkingv1alpha1.GroupVersion.String() {
		t.Errorf("TypeMeta not set: %+v", obj.TypeMeta)
	}
	// Name is sanitized to a DNS-safe form deterministically.
	if obj.Name == "" || obj.Name == "Press Line 7!" {
		t.Errorf("name not sanitized: %q", obj.Name)
	}
	if obj.Labels[edge.ManagedByLabel] != edge.ManagedByEdge {
		t.Errorf("managed-by label = %q, want %q", obj.Labels[edge.ManagedByLabel], edge.ManagedByEdge)
	}
	if obj.Spec.PublicKey != validKey {
		t.Errorf("spec not carried through: %+v", obj.Spec)
	}

	// Same device ID → same name (idempotent upsert).
	obj2 := edge.EdgeDeviceObject(networkingv1alpha1.EdgeDeviceSpec{DeviceID: "Press Line 7!", PublicKey: validKey2})
	if obj2.Name != obj.Name {
		t.Errorf("name not deterministic: %q vs %q", obj.Name, obj2.Name)
	}
}
