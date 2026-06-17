// Package v1alpha1 contains the API schema definitions for the networking
// v1alpha1 API group of DataWerx Mesh.
//
// The types in this package back the `MeshPeer` Custom Resource Definition,
// which is the single source of truth for the reconciliation engine.
// Both the Free (LocalGitOps) and Premium (EnterpriseControlPlane) tiers
// ultimately project their desired state into objects of this group so
// that the core reconciler never has to know where the topology originated.
//
// +kubebuilder:object:generate=true
// +groupName=networking.datawerx.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the group/version used to register these objects with
	// the runtime scheme.
	GroupVersion = schema.GroupVersion{Group: "networking.datawerx.io", Version: "v1alpha1"}

	// SchemeBuilder collects the functions that add the types in this group to
	// a Scheme. It is consumed by the manager bootstrap in cmd/manager.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme registers all types of this API group into the supplied
	// Scheme. Exposed as a package-level variable so callers can pass it
	// directly to clientgoscheme-style aggregation.
	AddToScheme = SchemeBuilder.AddToScheme
)

func init() {
	SchemeBuilder.Register(
		&MeshPeer{}, &MeshPeerList{},
		&EndpointExport{}, &EndpointExportList{},
		&MeshNetworkPolicy{}, &MeshNetworkPolicyList{},
	)
}
