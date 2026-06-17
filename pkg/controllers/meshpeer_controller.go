// Package controllers contains the open-source reconciliation core of DataWerx
// Mesh. The MeshPeerReconciler is the heart of the operator: it watches
// MeshPeer custom resources and converges the node-local WireGuard data plane
// toward the declared desired state.
//
// The reconciler is deliberately agnostic to how MeshPeer objects came to
// exist. In the Free tier, a GitOps pipeline writes them directly. In the
// Premium tier, a sync loop mirrors the centralized SaaS topology into identical
// CRDs. Either way the control flow below is unchanged.  This is the whole
// point of the control-plane abstraction.
package controllers

import (
	"context"
	"fmt"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	networkingv1alpha1 "github.com/datawerx/datawerx/pkg/apis/networking/v1alpha1"
	"github.com/datawerx/datawerx/pkg/logging"
	"github.com/datawerx/datawerx/pkg/topology"
)

// meshPeerFinalizer guarantees the reconciler gets a chance to withdraw kernel
// state (peer and routes) before the API object disappears. Without it, a delete
// could race ahead of data-plane cleanup and strand routes in the host table.
const meshPeerFinalizer = "networking.datawerx.io/meshpeer-cleanup"

// handshakeRefreshInterval is how often a programmed peer is re-reconciled so its
// LastHandshakeTime status stays current without waiting on a spec change or the
// controller-runtime cache resync.
const handshakeRefreshInterval = 60 * time.Second

// PeerDataPlane is the narrow surface of the WireGuard manager that the
// reconciler depends on. Depending on an interface keeps the controller
// unit-testable with a fake and reinforces the separation between control
// logic and kernel mechanics.
type PeerDataPlane interface {
	ConfigurePeer(peerKey, endpoint string, allowedIPs []string) error
	RemovePeer(peerKey string) error
	PeerHandshake(peerKey string) (int64, error)
}

// MeshPeerReconciler reconciles MeshPeer objects against the local WireGuard
// data plane.
type MeshPeerReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// DataPlane programs and tears down WireGuard peers and routes.
	DataPlane PeerDataPlane

	// LocalCIDRs are this cluster's own baseline runtime ranges. A
	// remote peer advertising a CIDR that overlaps one of these cannot
	// be plainly routed without hijacking local traffic, so overlaps are
	// flagged for the NAT-translation path described in Reconcile.
	LocalCIDRs []string

	// ClusterID identifies this cluster; used to derive deterministic virtual
	// ranges when overlap remap is enabled.
	ClusterID string

	// RemapPool, when non-empty, enables basic overlapping-CIDR remap: instead
	// of refusing a conflicting remote CIDR, the reconciler routes the peer's
	// deterministic *virtual* range carved from this pool and the NETMAP data
	// plane translates it 1:1. Empty keeps the safe refuse-and-Error behavior.
	RemapPool string

	// keyIndex remembers the WireGuard public key last programmed for each
	// MeshPeer, keyed by namespaced name. It exists so the NotFound branch of
	// Reconcile — where the object and its spec is already gone —
	// can still identify which peer to tear down. Guarded by keyIndexMu.
	keyIndex   map[types.NamespacedName]string
	keyIndexMu sync.Mutex
}

// +kubebuilder:rbac:groups=networking.datawerx.io,resources=meshpeers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.datawerx.io,resources=meshpeers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=networking.datawerx.io,resources=meshpeers/finalizers,verbs=update

// Reconcile drives a single MeshPeer toward its desired data-plane state.
//
// High-level flow:
//
//  1. Fetch the object. If it is gone (NotFound), withdraw any peer we had
//     previously programmed for that namespaced name and return.
//  2. If it is being deleted (deletion timestamp set), run finalizer cleanup:
//     remove the peer from the data plane, drop the finalizer, return.
//  3. Otherwise ensure our finalizer is present, validate CIDRs against the
//     local cluster ranges, program the peer + routes, and publish status.
func (r *MeshPeerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("meshpeer", req.NamespacedName)

	var peer networkingv1alpha1.MeshPeer
	if err := r.Get(ctx, req.NamespacedName, &peer); err != nil {
		if apierrors.IsNotFound(err) {
			// The object is gone. We can no longer read its spec, so we rely on
			// the key we cached the last time we programmed it. RemovePeer is
			// idempotent, so a cache miss (e.g. after an agent restart) simply
			// results in a safe no-op.
			if key, ok := r.lookupCachedKey(req.NamespacedName); ok {
				if err := r.DataPlane.RemovePeer(key); err != nil {
					logger.Error(err, "failed to remove peer for deleted MeshPeer; will retry")
					return ctrl.Result{}, err
				}
				r.forgetCachedKey(req.NamespacedName)
				logger.Info("removed peer for deleted MeshPeer")
			}
			return ctrl.Result{}, nil
		}
		// Transient API error: requeue with backoff.
		return ctrl.Result{}, fmt.Errorf("fetching MeshPeer: %w", err)
	}

	// Handle graceful deletion via finalizer. This path runs while the object
	// still exists so we have the spec, but is marked for deletion.
	if !peer.GetDeletionTimestamp().IsZero() {
		return r.reconcileDelete(ctx, &peer)
	}

	// Ensure our finalizer is registered before we program any kernel state, so
	// we are guaranteed a cleanup opportunity later.
	if !controllerutil.ContainsFinalizer(&peer, meshPeerFinalizer) {
		controllerutil.AddFinalizer(&peer, meshPeerFinalizer)
		if err := r.Update(ctx, &peer); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		// The update re-triggers reconciliation; return cleanly.
		return ctrl.Result{}, nil
	}

	return r.reconcileApply(ctx, &peer)
}

// reconcileApply validates and programs a live (non-deleting) MeshPeer.
func (r *MeshPeerReconciler) reconcileApply(ctx context.Context, peer *networkingv1alpha1.MeshPeer) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// IP Overlap Mitigation
	// ------------------------------------------------------------------
	// Two independently-administered clusters frequently reuse the same private
	// ranges. The default pod CIDR 10.244.0.0/16 is the default, canonical clash.
	// Naively routing a remote 10.244.0.0/16 while our own pods live there would
	// hijack local traffic into the tunnel.
	//
	// Two behaviors, selected by RemapPool:
	//
	//   * RemapPool unset by default: the safe option is to refuse to route the
	//     overlapping CIDR and report Error so nothing breaks silently.
	//
	//   * RemapPool set route the peer's deterministic *virtual*
	//     range instead, and the NETMAP data plane (RemapReconciler →
	//     nat.Manager.SyncRemap) performs a stateless 1:1 source and destination
	//     translation. The premium eBPF engine is a higher-throughput drop-in
	//     for the same seam.
	//
	// The decision of what to route is delegated to the pure topology.PlanPeer
	// function.  This method only carries out the resulting side effects. That
	// split keeps the overlap/validation logic fully unit-testable without K8s.
	plan := topology.PlanPeer(peer.Spec, r.LocalCIDRs)

	// A non-programmable plan (e.g. missing public key) is a spec error we
	// cannot act on; surface it on status and requeue.
	if !plan.Programmable() {
		return r.fail(ctx, peer, plan.Message)
	}

	// Determine the CIDRs to route and the status to report. By default we route
	// only the non-conflicting CIDRs and report the (possibly Error) plan as-is.
	routeCIDRs := plan.RoutableCIDRs
	phase, message := plan.Phase, plan.Message

	if plan.HasConflicts() {
		if r.RemapPool == "" {
			logger.Info("remote CIDRs overlap local ranges; remap disabled, refusing overlaps",
				"conflicts", plan.ConflictingCIDRs, "localCIDRs", r.LocalCIDRs)
		} else {
			// Overlap remap enabled: route the peer's deterministic virtual ranges
			// in place of the conflicting real ones. The NETMAP data plane, synced
			// by the Remap reconciler, performs the 1:1 translation.
			rp, err := topology.PlanRemap(r.RemapPool, r.ClusterID, r.LocalCIDRs, peer.Spec.ClusterID, plan.ConflictingCIDRs)
			if err != nil {
				return r.fail(ctx, peer, fmt.Sprintf("planning overlap remap: %v", err))
			}
			routeCIDRs = append(append([]string(nil), plan.RoutableCIDRs...), rp.RouteVirtual...)
			phase = networkingv1alpha1.MeshPeerPhaseConnected
			message = fmt.Sprintf("peer programmed; %d CIDRs routed, %d overlapping remapped via %s",
				len(plan.RoutableCIDRs), len(plan.ConflictingCIDRs), r.RemapPool)
			// Re-evaluated on every reconcile - debug-level so Info stays a
			// feed of genuine state changes. See pkg/logging verbosity convention.
			logger.V(1).Info("remapped overlapping remote CIDRs", "remapped", plan.ConflictingCIDRs, "virtual", rp.RouteVirtual)
		}
	}

	// Key rotation: if this MeshPeer was previously programmed under a different
	// public key, tear the stale peer and its routes down first. Otherwise the
	// old WireGuard peer would linger forever, since ConfigurePeer only upserts
	// the new key. RemovePeer is idempotent.
	if old, ok := r.lookupCachedKey(peerKey(peer)); ok && old != plan.PublicKey {
		if err := r.DataPlane.RemovePeer(old); err != nil {
			return r.fail(ctx, peer, fmt.Sprintf("removing rotated peer key: %v", err))
		}
		logger.Info("peer public key rotated; removed stale peer", "old", logging.ShortKey(old), "new", logging.ShortKey(plan.PublicKey))
	}

	// Program the routable CIDRs plus any remapped virtual ranges into the
	// data plane.
	if err := r.DataPlane.ConfigurePeer(plan.PublicKey, plan.Endpoint, routeCIDRs); err != nil {
		return r.fail(ctx, peer, fmt.Sprintf("configuring peer data plane: %v", err))
	}
	r.rememberCachedKey(peerKey(peer), plan.PublicKey)

	// Pull the latest handshake timestamp. Absence is not an error because the peer may
	// simply not have handshaked yet. On a transient read error, keep the last
	// known value rather than clobbering a good timestamp with 0 — a read failure
	// is not evidence the handshake regressed.
	handshake := peer.Status.LastHandshakeTime
	if hs, err := r.DataPlane.PeerHandshake(plan.PublicKey); err != nil {
		logger.Error(err, "could not read peer handshake; keeping last known value")
	} else {
		handshake = hs
	}

	// Per-reconcile breadcrumb, consistent with the other controllers'
	// "reconciled" debug line. Healthy peers emit nothing at Info; raise the log
	// level to trace a peer's convergence (see pkg/logging verbosity convention).
	logger.V(1).Info("meshpeer reconciled",
		"phase", phase, "publicKey", logging.ShortKey(plan.PublicKey), "routes", len(routeCIDRs), "lastHandshake", handshake)

	// Requeue so LastHandshakeTime refreshes on its own. Without this, it would be
	// frozen until the next spec change or the ~10h cache resync, making a live
	// peer look stale.
	return ctrl.Result{RequeueAfter: handshakeRefreshInterval},
		r.patchStatus(ctx, peer, phase, handshake, message)
}

// reconcileDelete performs finalizer-driven teardown for an object that is
// marked for deletion but still present.
func (r *MeshPeerReconciler) reconcileDelete(ctx context.Context, peer *networkingv1alpha1.MeshPeer) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if controllerutil.ContainsFinalizer(peer, meshPeerFinalizer) {
		// We still have the spec here, so prefer the authoritative key over the
		// cache.
		if peer.Spec.PublicKey != "" {
			if err := r.DataPlane.RemovePeer(peer.Spec.PublicKey); err != nil {
				logger.Error(err, "failed to remove peer during finalization; will retry")
				return ctrl.Result{}, err
			}
		}
		r.forgetCachedKey(peerKey(peer))

		controllerutil.RemoveFinalizer(peer, meshPeerFinalizer)
		if err := r.Update(ctx, peer); err != nil {
			return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
		}
		logger.Info("finalized MeshPeer; peer torn down")
	}
	return ctrl.Result{}, nil
}

// Fail records an Error phase with the given message and returns the message as
// an error so the work item is requeued with backoff. Status update failures
// are folded in so the caller sees a single error.
func (r *MeshPeerReconciler) fail(ctx context.Context, peer *networkingv1alpha1.MeshPeer, msg string) (ctrl.Result, error) {
	if serr := r.patchStatus(ctx, peer, networkingv1alpha1.MeshPeerPhaseError, peer.Status.LastHandshakeTime, msg); serr != nil {
		return ctrl.Result{}, fmt.Errorf("%s; additionally failed to update status: %w", msg, serr)
	}
	return ctrl.Result{}, fmt.Errorf("%s", msg)
}

// patchStatus writes the status subresource only when something actually
// changed, avoiding needless API writes and reconcile churn. It tolerates
// conflict errors by surfacing them for requeue.
func (r *MeshPeerReconciler) patchStatus(
	ctx context.Context,
	peer *networkingv1alpha1.MeshPeer,
	phase networkingv1alpha1.MeshPeerPhase,
	handshake int64,
	message string,
) error {
	if peer.Status.Phase == phase &&
		peer.Status.LastHandshakeTime == handshake &&
		peer.Status.Message == message &&
		peer.Status.ObservedGeneration == peer.Generation {
		return nil
	}

	base := peer.DeepCopy()
	peer.Status.Phase = phase
	peer.Status.LastHandshakeTime = handshake
	peer.Status.Message = message
	peer.Status.ObservedGeneration = peer.Generation

	if err := r.Status().Patch(ctx, peer, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("patching MeshPeer status: %w", err)
	}
	return nil
}

//cached-key thread-safe bookkeeping

func (r *MeshPeerReconciler) rememberCachedKey(nn types.NamespacedName, key string) {
	r.keyIndexMu.Lock()
	defer r.keyIndexMu.Unlock()
	if r.keyIndex == nil {
		r.keyIndex = make(map[types.NamespacedName]string)
	}
	r.keyIndex[nn] = key
}

func (r *MeshPeerReconciler) lookupCachedKey(nn types.NamespacedName) (string, bool) {
	r.keyIndexMu.Lock()
	defer r.keyIndexMu.Unlock()
	k, ok := r.keyIndex[nn]
	return k, ok
}

func (r *MeshPeerReconciler) forgetCachedKey(nn types.NamespacedName) {
	r.keyIndexMu.Lock()
	defer r.keyIndexMu.Unlock()
	delete(r.keyIndex, nn)
}

// SetupWithManager wires the reconciler into the controller-runtime manager,
// establishing the watch on MeshPeer objects.
func (r *MeshPeerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.keyIndex == nil {
		r.keyIndex = make(map[types.NamespacedName]string)
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&networkingv1alpha1.MeshPeer{}).
		Named("meshpeer").
		Complete(r)
}

func peerKey(peer *networkingv1alpha1.MeshPeer) types.NamespacedName {
	return types.NamespacedName{Namespace: peer.Namespace, Name: peer.Name}
}
