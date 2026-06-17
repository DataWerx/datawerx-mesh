package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ServiceImportType describes how an imported service is consumed.
type ServiceImportType string

const (
	// ClusterSetIP exposes the imported service behind a single stable virtual
	// IP (the ClusterSetIP) that load-balances across all exporting clusters.
	ClusterSetIP ServiceImportType = "ClusterSetIP"

	// Headless exposes the imported service as the union of backing pod IPs
	// across all exporting clusters, with no virtual IP.
	Headless ServiceImportType = "Headless"
)

// MCS labels carried by the EndpointSlices a consuming cluster mirrors for an
// imported service (KEP-1645). They let native consumers and kube-proxy discover
// cross-cluster endpoints through the standard discovery.k8s.io API, and let the
// import controller find the slices it owns for a given import.
const (
	// LabelServiceName is set to the ServiceImport name on every mirrored slice.
	LabelServiceName = "multicluster.kubernetes.io/service-name"
	// LabelSourceCluster is the mesh ID of the cluster the mirrored endpoints
	// came from, so per-cluster contributions stay distinguishable.
	LabelSourceCluster = "multicluster.kubernetes.io/source-cluster"
)

// ServicePort represents a port exposed by an imported service. It mirrors the
// MCS API's port shape which is a subset of core/v1 ServicePort relevant across
// clusters.
type ServicePort struct {
	// Name is the optional, port-unique name. Required when more than one port
	// is present.
	//
	// +optional
	Name string `json:"name,omitempty"`

	// Protocol for this port. Defaults to TCP.
	//
	// +optional
	Protocol corev1.Protocol `json:"protocol,omitempty"`

	// AppProtocol is the application protocol hint for this port (e.g. "http").
	//
	// +optional
	AppProtocol *string `json:"appProtocol,omitempty"`

	// Port is the port number that is exposed.
	Port int32 `json:"port"`
}

// ClusterStatus tracks which clusters currently contribute to an import.
type ClusterStatus struct {
	// Cluster is the ID of an exporting cluster contributing to this import.
	Cluster string `json:"cluster"`
}

// ServiceImportSpec describes the desired state of an imported service.
type ServiceImportSpec struct {
	// Ports is the merged set of ports across all exporting clusters.
	//
	// +optional
	// +listType=atomic
	Ports []ServicePort `json:"ports,omitempty"`

	// IPs holds the virtual ClusterSetIP(s) for a ClusterSetIP-type import.
	// Empty for Headless imports.
	//
	// +optional
	// +listType=set
	IPs []string `json:"ips,omitempty"`

	// Type is the consumption model: ClusterSetIP or Headless.
	//
	// +kubebuilder:validation:Enum=ClusterSetIP;Headless
	Type ServiceImportType `json:"type"`

	// SessionAffinity mirrors the exported Service's affinity ClientIP/None.
	//
	// +optional
	SessionAffinity corev1.ServiceAffinity `json:"sessionAffinity,omitempty"`
}

// ServiceImportStatus describes the observed state of an imported service.
type ServiceImportStatus struct {
	// Clusters is the set of clusters currently exporting this service.
	//
	// +optional
	// +listType=map
	// +listMapKey=cluster
	Clusters []ClusterStatus `json:"clusters,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=svcim
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="IP",type=string,JSONPath=`.spec.ips[0]`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ServiceImport is the cluster-local representation of a service that has been
// exported by one or more other clusters in the mesh. It is created and
// reconciled by the import controller, never authored by users, and is what the
// clusterset.local DNS zone answers from.
type ServiceImport struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +optional
	Spec ServiceImportSpec `json:"spec,omitempty"`

	// +optional
	Status ServiceImportStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ServiceImportList contains a list of ServiceImport objects.
type ServiceImportList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ServiceImport `json:"items"`
}
