package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ServiceExportConditionType enumerates the well-known condition types reported
// on a ServiceExport, mirroring the MCS API.
const (
	// ServiceExportValid is True once the referenced Service exists and the
	// export has been accepted and published to the mesh.
	ServiceExportValid = "Valid"

	// ServiceExportConflict is True when another cluster exports the same
	// namespaced name with an incompatible definition (type or port mismatch).
	ServiceExportConflict = "Conflict"
)

// ServiceExportStatus describes the observed state of a ServiceExport.
type ServiceExportStatus struct {
	// Conditions describe the current state of the export. Standard condition
	// types are Valid and Conflict.
	//
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=svcex
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ServiceExport declares that the Service with the same name and namespace
// should be exported to - made reachable from - all other clusters in the mesh.
//
// ServiceExport has no spec; it is a marker. The Service it refers to is
// identified solely by matching metadata.name and metadata.namespace, exactly
// as defined by the MCS API. This keeps the user model simple.
type ServiceExport struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +optional
	Status ServiceExportStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ServiceExportList contains a list of ServiceExport objects.
type ServiceExportList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ServiceExport `json:"items"`
}
