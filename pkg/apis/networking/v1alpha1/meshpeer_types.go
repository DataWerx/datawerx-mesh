package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MeshPeerPhase enumerates the lifecycle states a peer link can occupy. It is
// surfaced on the resource status subresource and is the primary signal that
// GitOps pipelines and dashboards key off of.
type MeshPeerPhase string

const (
	// MeshPeerPhasePending indicates the peer has been accepted by the API
	// server but the local agent has not yet programmed the WireGuard device
	// or installed routes for it.
	MeshPeerPhasePending MeshPeerPhase = "Pending"

	// MeshPeerPhaseConnected indicates the peer has been programmed into the
	// `dwx-mesh0` device and, where observable, a recent cryptographic
	// handshake has been recorded.
	MeshPeerPhaseConnected MeshPeerPhase = "Connected"

	// MeshPeerPhaseError indicates reconciliation failed. The accompanying
	// Status.Message field carries a human readable description of the fault.
	MeshPeerPhaseError MeshPeerPhase = "Error"
)

// MeshPeerSpec defines the desired state of a single remote cluster that the
// local node should establish an encrypted WireGuard tunnel to.
//
// Every field here is intentionally declarative: the reconciler treats the
// spec as the authoritative description of what the data plane should look
// like and converges the kernel state toward it.
type MeshPeerSpec struct {
	// ClusterID is the stable, globally unique identifier of the remote
	// cluster. It is used as the correlation key for NAT bookkeeping and for
	// human-facing diagnostics. It must be stable across the lifetime of the
	// peering relationship.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	ClusterID string `json:"clusterID"`

	// PublicKey is the base64-encoded Curve25519 WireGuard public key of the
	// remote peer. The local node uses this as the cryptographic identity when
	// programming the peer into the `dwx-mesh0` interface.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	PublicKey string `json:"publicKey"`

	// Endpoint is the publicly reachable `host:port` or `ip:port` of the
	// remote cluster's ingress relay or gateway node. WireGuard will direct
	// outbound encrypted UDP traffic here. When empty, the peer is treated as
	// "roaming" and the kernel will learn the endpoint from inbound handshakes.
	//
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// PodCIDRs is the list of pod network ranges served by the remote cluster.
	// Traffic destined for any of these ranges is routed into the mesh device.
	//
	// +optional
	// +listType=set
	PodCIDRs []string `json:"podCIDRs,omitempty"`

	// ServiceCIDRs is the list of service (ClusterIP) ranges served by the
	// remote cluster. These are programmed as allowed-IPs / routes identically
	// to PodCIDRs.
	//
	// +optional
	// +listType=set
	ServiceCIDRs []string `json:"serviceCIDRs,omitempty"`
}

// MeshPeerStatus defines the observed state of a MeshPeer as last reconciled by
// the local node agent.
type MeshPeerStatus struct {
	// Phase is the coarse-grained lifecycle state of the peering.
	//
	// +optional
	// +kubebuilder:validation:Enum=Pending;Connected;Error
	Phase MeshPeerPhase `json:"phase,omitempty"`

	// LastHandshakeTime is the Unix epoch (seconds) of the most recent
	// successful WireGuard handshake observed for this peer. A value of 0 means
	// no handshake has been observed yet. It is stored as an int64 rather than
	// metav1.Time so that the lean DaemonSet agent can update it cheaply from
	// raw netlink statistics without allocating wrapper types.
	//
	// +optional
	LastHandshakeTime int64 `json:"lastHandshakeTime,omitempty"`

	// LastProbeAttempt is the Unix epoch (seconds) of the most recent active
	// synthetic probe of this peer, regardless of outcome. 0 means the peer has
	// not been actively probed — probing is disabled, or no cycle has run. When
	// recent it tells the read surfaces to trust the probe over the handshake;
	// when it ages out, liveness falls back to the handshake on its own. Written
	// by the prober (DataWerx_PROBE_ENABLE), an int64 like LastHandshakeTime.
	//
	// +optional
	LastProbeAttempt int64 `json:"lastProbeAttempt,omitempty"`

	// LastProbeTime is the Unix epoch (seconds) of the most recent *successful*
	// active probe — a workload in this cluster actually answered across the
	// mesh, not merely that the tunnel handshook. 0 means no probe has succeeded
	// yet. It is the probe-observed analog of LastHandshakeTime.
	//
	// +optional
	LastProbeTime int64 `json:"lastProbeTime,omitempty"`

	// Message is a human readable description of the current condition. On the
	// Error phase it carries the failure reason; on Connected it may carry
	// lightweight health metrics (e.g. observed RX/TX byte counters).
	//
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the metadata.generation that was most recently
	// acted upon by the reconciler. It lets controllers and humans detect
	// stale status.
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=mp
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.clusterID`
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.spec.endpoint`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// MeshPeer is the Schema for the meshpeers API. It is cluster-scoped because a
// peering relationship is a property of the cluster as a whole rather than of
// any single namespace, and every node-local agent watches the same set.
type MeshPeer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MeshPeerSpec   `json:"spec,omitempty"`
	Status MeshPeerStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// MeshPeerList contains a list of MeshPeer objects.
type MeshPeerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MeshPeer `json:"items"`
}

// AllCIDRs returns the union of pod and service CIDRs declared on the spec.
// This is the canonical set of destinations that should be routed into the
// mesh device for this peer and is used by the reconciler when calling
// ConfigurePeer.
func (s *MeshPeerSpec) AllCIDRs() []string {
	out := make([]string, 0, len(s.PodCIDRs)+len(s.ServiceCIDRs))
	out = append(out, s.PodCIDRs...)
	out = append(out, s.ServiceCIDRs...)
	return out
}
