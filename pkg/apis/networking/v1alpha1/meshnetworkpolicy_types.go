package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MeshNetworkPolicyPhase is the high-level state of a MeshNetworkPolicy.
type MeshNetworkPolicyPhase string

const (
	// MeshNetworkPolicyPhasePending means the policy has not yet been compiled
	// into the data plane.
	MeshNetworkPolicyPhasePending MeshNetworkPolicyPhase = "Pending"
	// MeshNetworkPolicyPhaseReady means the policy is programmed.
	MeshNetworkPolicyPhaseReady MeshNetworkPolicyPhase = "Ready"
	// MeshNetworkPolicyPhaseError means compilation or programming failed.
	MeshNetworkPolicyPhaseError MeshNetworkPolicyPhase = "Error"
)

// MeshNetworkPolicySpec declares which remote sources may reach which local
// destinations over the mesh. Semantics mirror Kubernetes NetworkPolicy: a
// destination selected by any MeshNetworkPolicy is default-deny for mesh
// ingress and reachable only via the union of that destination's ingress allow
// rules. Destinations selected by no policy are unaffected.
type MeshNetworkPolicySpec struct {
	// Destinations are the local CIDRs this policy protects (e.g. a namespace's
	// pod range, or a service CIDR). Empty Destinations protects ALL mesh
	// ingress on this cluster — a default-deny posture — so use it deliberately.
	//
	// +optional
	// +listType=set
	Destinations []string `json:"destinations,omitempty"`

	// Ingress is the list of allow rules. A mesh packet to a protected
	// destination is permitted iff it matches at least one ingress rule.
	//
	// +optional
	// +listType=atomic
	Ingress []MeshIngressRule `json:"ingress,omitempty"`
}

// MeshIngressRule allows traffic from any of From (OR-ed) on any of Ports.
type MeshIngressRule struct {
	// From lists the allowed sources. An empty From matches no source (the rule
	// allows nothing); to allow from anywhere, use a CIDR of 0.0.0.0/0.
	//
	// +optional
	// +listType=atomic
	From []MeshPeerSelector `json:"from,omitempty"`

	// Ports restricts the rule to these L4 ports. Empty Ports allows all ports.
	//
	// +optional
	// +listType=atomic
	Ports []MeshNetworkPolicyPort `json:"ports,omitempty"`
}

// MeshPeerSelector names allowed sources by mesh cluster ID (resolved to that
// cluster's advertised CIDRs) and/or by explicit CIDR.
type MeshPeerSelector struct {
	// ClusterIDs are remote mesh cluster IDs whose ranges are allowed.
	//
	// +optional
	// +listType=set
	ClusterIDs []string `json:"clusterIDs,omitempty"`

	// CIDRs are explicit source ranges allowed (e.g. 10.40.0.0/16).
	//
	// +optional
	// +listType=set
	CIDRs []string `json:"cidrs,omitempty"`
}

// MeshNetworkPolicyPort is an allowed protocol/port.
type MeshNetworkPolicyPort struct {
	// Protocol is TCP, UDP, or SCTP. Defaults to TCP.
	//
	// +kubebuilder:validation:Enum=TCP;UDP;SCTP
	// +optional
	Protocol string `json:"protocol,omitempty"`

	// Port is the destination port (1-65535).
	//
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
}

// MeshNetworkPolicyStatus reports compilation/programming state.
type MeshNetworkPolicyStatus struct {
	// Phase is Pending, Ready, or Error.
	//
	// +optional
	Phase MeshNetworkPolicyPhase `json:"phase,omitempty"`

	// Message carries human-readable detail, especially on Error or when some
	// inputs were skipped (e.g. non-IPv4 CIDRs on an IPv4-only data plane).
	//
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the .metadata.generation last acted on.
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=mnp
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// MeshNetworkPolicy is a cluster-scoped, cross-cluster L3/L4 ingress policy for
// mesh traffic. It is the free-tier complement to Kubernetes NetworkPolicy,
// expressing allow rules at mesh granularity (cluster IDs + CIDRs) that the
// agent compiles into iptables filter rules on the WireGuard device.
type MeshNetworkPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MeshNetworkPolicySpec   `json:"spec,omitempty"`
	Status MeshNetworkPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// MeshNetworkPolicyList contains a list of MeshNetworkPolicy objects.
type MeshNetworkPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MeshNetworkPolicy `json:"items"`
}
