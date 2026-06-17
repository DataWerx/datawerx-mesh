// Package v1alpha1 contains DataWerx Mesh's implementation of the Kubernetes
// Multi-Cluster Services (MCS) API (KEP-1645): ServiceExport and ServiceImport.
//
// These types use the upstream SIG-Multicluster group name
// `multicluster.x-k8s.io` so that DataWerx Mesh is a drop-in MCS provider and
// remains compatible with the `*.clusterset.local` DNS convention and any
// tooling that already understands MCS. They are hand-written, no controller-gen
// codegen, in order to match this repository's conventions.
// Keep the deepcopy and CRD YAML in sync with the structs by hand.
//
// +kubebuilder:object:generate=true
// +groupName=multicluster.x-k8s.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the group/version for the MCS types as registered with
	// the runtime scheme.
	GroupVersion = schema.GroupVersion{Group: "multicluster.x-k8s.io", Version: "v1alpha1"}

	// SchemeBuilder collects the functions that add these types to a Scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme registers all types of this API group into the supplied
	// Scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func init() {
	SchemeBuilder.Register(
		&ServiceExport{}, &ServiceExportList{},
		&ServiceImport{}, &ServiceImportList{},
	)
}
