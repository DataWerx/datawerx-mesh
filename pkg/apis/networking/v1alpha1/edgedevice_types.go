package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EdgeDevicePhase enumerates the lifecycle states an edge device connection can
// occupy. It mirrors MeshPeerPhase so the read surfaces (snapshot, dwx) treat
// device liveness with the same vocabulary they already use for cluster peers.
type EdgeDevicePhase string

const (
	// EdgeDevicePhasePending indicates the EdgeDevice has been accepted by the
	// API server but the edge-ingress terminator has not yet programmed the
	// device as a peer on `dwx-edge0`.
	EdgeDevicePhasePending EdgeDevicePhase = "Pending"

	// EdgeDevicePhaseConnected indicates the device has been programmed into the
	// terminator and, where observable, a recent cryptographic handshake has been
	// recorded over the access tunnel.
	EdgeDevicePhaseConnected EdgeDevicePhase = "Connected"

	// EdgeDevicePhaseError indicates reconciliation failed — a malformed key, an
	// address that does not fit the edge CIDR, or a data-plane fault. The
	// accompanying Status.Message carries a human readable description.
	EdgeDevicePhaseError EdgeDevicePhase = "Error"
)

// EdgeDeviceSpec describes a single non-Kubernetes device (an IoT box, a factory
// gateway, a VM, a laptop) that should reach mesh services by name over a tunnel
// the device dials outbound to the edge-ingress terminator.
//
// An EdgeDevice is NOT a cluster peer: it never joins `dwx-mesh0`, never exports
// Services, and never appears in the cluster topology graph. It is a client whose
// reachable set is bounded by the terminator's AllowedIPs ACL plus the gateway
// masquerade scope. The CRD is the tier-agnostic integration point — authored by
// the free `dwx edge` path or materialized by the premium control plane — and
// the reconciler programming it never branches on who wrote it.
type EdgeDeviceSpec struct {
	// DeviceID is the stable, human-facing identity of the device. It is the
	// correlation key for diagnostics and the basis for the deterministic object
	// name. It must be stable across the lifetime of the enrollment.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	DeviceID string `json:"deviceID"`

	// PublicKey is the base64-encoded Curve25519 WireGuard public key of the
	// device. It is the device's cryptographic identity; the matching private key
	// is generated on the device and never leaves it. The terminator uses this to
	// program the device as a peer on `dwx-edge0`.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	PublicKey string `json:"publicKey"`

	// Address optionally pins the device's tunnel address (a single host address,
	// with or without a /32 or /128 suffix). When empty the address is allocated
	// deterministically from the edge CIDR by every terminator independently, so
	// no central allocator is required.
	//
	// +optional
	Address string `json:"address,omitempty"`

	// AllowedServices optionally scopes the device to a set of clusterset service
	// names (globs against `<service>.<namespace>`). When empty the device may
	// reach every imported service the gateway exposes. The set is compiled into
	// the device profile and, in identity-preserving mode, into device-scoped
	// policy.
	//
	// +optional
	// +listType=set
	AllowedServices []string `json:"allowedServices,omitempty"`

	// AllowedCIDRs optionally grants the device extra raw destination ranges
	// beyond the clusterset/mesh CIDRs. Each entry is screened the same way mesh
	// CIDRs are: a range that is never safe to route (default route, loopback,
	// link-local, multicast) is rejected.
	//
	// +optional
	// +listType=set
	AllowedCIDRs []string `json:"allowedCIDRs,omitempty"`

	// IdentityPreserving requests that the gateway skip the client masquerade so
	// pods and MeshNetworkPolicy see the device's real tunnel IP. It requires the
	// premium per-node return-route component; without it the open-core MASQUERADE
	// path is used regardless of this flag.
	//
	// +optional
	IdentityPreserving bool `json:"identityPreserving,omitempty"`

	// ExpiresAt optionally bounds the enrollment. Past this instant the reconciler
	// tears the device peer down on the next reconcile, cutting the device off
	// without an explicit delete.
	//
	// +optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`
}

// EdgeDeviceStatus is the observed state of an EdgeDevice as last reconciled by
// the edge-ingress terminator.
type EdgeDeviceStatus struct {
	// Phase is the coarse-grained lifecycle state of the device connection.
	//
	// +optional
	// +kubebuilder:validation:Enum=Pending;Connected;Error
	Phase EdgeDevicePhase `json:"phase,omitempty"`

	// Address is the tunnel address assigned to the device (the allocated or
	// pinned host address, rendered as a /32 or /128). It is published so the
	// device-side profile and operators agree on the device's identity.
	//
	// +optional
	Address string `json:"address,omitempty"`

	// LastHandshakeTime is the Unix epoch (seconds) of the most recent successful
	// WireGuard handshake observed over the access tunnel. 0 means none yet. It is
	// an int64 (not metav1.Time) for the same cheap-update reason as MeshPeer.
	//
	// +optional
	LastHandshakeTime int64 `json:"lastHandshakeTime,omitempty"`

	// Message is a human readable description of the current condition. On the
	// Error phase it carries the failure reason.
	//
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration is the metadata.generation most recently acted upon by
	// the reconciler.
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=ed
// +kubebuilder:printcolumn:name="Device",type=string,JSONPath=`.spec.deviceID`
// +kubebuilder:printcolumn:name="Address",type=string,JSONPath=`.status.address`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// EdgeDevice is the Schema for the edgedevices API. It is cluster-scoped because
// a device enrollment is a property of the clusterset as a whole — every gateway
// node's terminator watches the same set and computes the same address
// assignment — rather than of any single namespace.
type EdgeDevice struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   EdgeDeviceSpec   `json:"spec,omitempty"`
	Status EdgeDeviceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// EdgeDeviceList contains a list of EdgeDevice objects.
type EdgeDeviceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EdgeDevice `json:"items"`
}
