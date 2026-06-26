package edge

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	networkingv1alpha1 "github.com/DataWerx/datawerx-mesh/pkg/apis/networking/v1alpha1"
	"github.com/DataWerx/datawerx-mesh/pkg/topology"
)

// EdgeDeviceObject builds the cluster-scoped EdgeDevice object to apply for a
// device, naming it deterministically from the device ID and tagging it as
// authored by `dwx edge`. Re-enrolling the same device is therefore an
// idempotent upsert. The TypeMeta is set so the object rendered by
// `dwx edge enroll --dry-run` is a valid, self-describing manifest a user can
// pipe straight into `kubectl apply -f -`. It mirrors bootstrap.PeerObject.
func EdgeDeviceObject(spec networkingv1alpha1.EdgeDeviceSpec) *networkingv1alpha1.EdgeDevice {
	return &networkingv1alpha1.EdgeDevice{
		TypeMeta: metav1.TypeMeta{
			APIVersion: networkingv1alpha1.GroupVersion.String(),
			Kind:       "EdgeDevice",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:   topology.SanitizeName(spec.DeviceID),
			Labels: map[string]string{ManagedByLabel: ManagedByEdge},
		},
		Spec: spec,
	}
}
