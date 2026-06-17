package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	mcsv1alpha1 "github.com/DataWerx/datawerx-mesh/pkg/apis/multicluster/v1alpha1"
)

// EndpointExportSpec is the broker-less "wire format" for one cluster's export
// of one service. It is produced by the ServiceExport controller from a local
// Service and is the tier-agnostic integration point for cross-cluster DNS —
// exactly analogous to how MeshPeer carries peer topology:
//
//   - Free tier: the export controller writes EndpointExports locally, and the
//     user's GitOps pipeline mirrors them between clusters.
//   - Premium tier: a SaaS syncer materializes remote EndpointExports locally.
//
// Either way the ServiceImport controller only ever reads
// EndpointExport objects, so its logic is identical across tiers.
type EndpointExportSpec struct {
	// ClusterID is the mesh ID of the cluster that owns this export. It makes
	// each cluster's contribution to a service distinct after mirroring.
	//
	// +kubebuilder:validation:Required
	ClusterID string `json:"clusterID"`

	// ServiceNamespace is the namespace of the exported Service.
	//
	// +kubebuilder:validation:Required
	ServiceNamespace string `json:"serviceNamespace"`

	// ServiceName is the name of the exported Service.
	//
	// +kubebuilder:validation:Required
	ServiceName string `json:"serviceName"`

	// Type is the consumption model contributed for this service
	// (ClusterSetIP or Headless).
	//
	// +kubebuilder:validation:Enum=ClusterSetIP;Headless
	Type mcsv1alpha1.ServiceImportType `json:"type"`

	// ExportedAtUnix is the Unix epoch (seconds) the owning ServiceExport was
	// created. It is the key for MCS "oldest export wins" conflict resolution; an
	// int64 (rather than metav1.Time) keeps the lean agent allocation-free, as
	// MeshPeer.Status.LastHandshakeTime does. 0 means unknown.
	//
	// +optional
	ExportedAtUnix int64 `json:"exportedAtUnix,omitempty"`

	// Ports is this cluster's view of the service's ports.
	//
	// +optional
	// +listType=atomic
	Ports []mcsv1alpha1.ServicePort `json:"ports,omitempty"`

	// IPs is the reachable address(es) for the service in the owning cluster
	// ClusterIP(s) for a ClusterSetIP service, empty for headless, whose
	// pod IPs propagate via EndpointSlices.
	//
	// +optional
	// +listType=set
	IPs []string `json:"ips,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=epx
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.clusterID`
// +kubebuilder:printcolumn:name="Service",type=string,JSONPath=`.spec.serviceName`
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`

// EndpointExport is one cluster's published contribution to a cross-cluster
// service. It is machine-managed. The export controller, GitOps, or the
// premium syncer manage it, and it's consumed by the import controller to
// build ServiceImports.
type EndpointExport struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec EndpointExportSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// EndpointExportList contains a list of EndpointExport objects.
type EndpointExportList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EndpointExport `json:"items"`
}

// ServiceKey returns the namespaced name of the Service this export contributes to.
func (s *EndpointExportSpec) ServiceKey() (namespace, name string) {
	return s.ServiceNamespace, s.ServiceName
}
