// Package syncer contains the premium-tier topology syncer: a manager.Runnable
// that mirrors a centralized control plane's topology into local MeshPeer CRDs,
// which the tier-agnostic reconciler then programs into the data plane.
//
// It lives behind the ControlPlaneClient seam — the free tier never constructs
// a Syncer, so all premium logic stays additive. Extracted from cmd/manager
// so its non-trivial behavior.  Revision short-circuiting, stale-peer pruning,
// and conflict detection are unit testable against a fake client with no API server.
package syncer

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	networkingv1alpha1 "github.com/datawerx/datawerx/pkg/apis/networking/v1alpha1"
	dwxclient "github.com/datawerx/datawerx/pkg/client"
	"github.com/datawerx/datawerx/pkg/topology"
)

const (
	// managedByLabel marks MeshPeers authored by this syncer so pruning can
	// safely delete only peers it owns, never CRDs a human or GitOps authored.
	managedByLabel = "app.kubernetes.io/managed-by"
	managedByValue = "datawerx-topology-syncer"
)

// ConflictObserver is notified of the topology conflicts detected on each sync.
// It lets the observability layer gauge conflicts without this package taking a
// metrics dependency. Optional; nil disables the callback.
type ConflictObserver func(conflicts []topology.TopologyConflict)

// Syncer periodically pulls topology from a control plane and converges the set
// of MeshPeer CRDs to match it: upserting current peers, pruning peers it
// previously authored that have disappeared, and validating the advertised
// topology for conflicts. It satisfies manager.Runnable.
//
// NOTE: In a multi-node DaemonSet, authoring CRDs is a cluster-singleton
// concern; a production deployment gates this behind leader election or a
// dedicated controller pod. It is colocated here to keep the agent a single
// binary; the operations are idempotent so concurrent writers converge.
type Syncer struct {
	CP       dwxclient.ControlPlaneClient
	K8s      ctrlclient.Client
	Interval time.Duration
	Log      logr.Logger

	// LocalClusterID, when set, is skipped during upserts so a control plane
	// that returns the full mesh (including this cluster) does not make a node
	// peer with itself.
	LocalClusterID string

	// OnConflicts, if set, is invoked with the conflicts found on every sync.
	OnConflicts ConflictObserver

	// lastRevision caches the most recently applied topology revision so an
	// unchanged revision short-circuits the upsert/prune work.
	lastRevision string
	// applied is true once at least one sync has succeeded, so the very first
	// sync always runs even if the control plane reports an empty revision.
	applied bool
}

// Start runs the sync loop until the context is cancelled. It primes once
// immediately so a freshly started agent converges without waiting a full
// interval.
func (s *Syncer) Start(ctx context.Context) error {
	if s.Interval <= 0 {
		s.Interval = 30 * time.Second
	}
	ticker := time.NewTicker(s.Interval)
	defer ticker.Stop()

	s.syncOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.syncOnce(ctx)
		}
	}
}

// syncOnce performs a single fetch-validate-converge pass, logging, rather than
// returning errors, so a transient failure never tears down the loop.
func (s *Syncer) syncOnce(ctx context.Context) {
	peers, revision, err := s.fetch(ctx)
	if err != nil {
		s.Log.Error(err, "fetching topology")
		return
	}

	// Skip the converge when the control plane reports an unchanged revision —
	// but only once we've successfully applied at least one revision, so the
	// first sync always runs.
	if s.applied && revision != "" && revision == s.lastRevision {
		s.Log.V(1).Info("topology revision unchanged; skipping", "revision", revision, "peers", len(peers))
		return
	}

	s.reportConflicts(peers)

	desired, ok := s.upsertDesired(ctx, peers)
	if !ok {
		// At least one upsert failed in a transient API error. Don't prune and
		// don't record the revision: pruning now could delete a live peer whose
		// update merely failed, and recording the revision would make the next
		// tick short-circuit and never retry. Leaving lastRevision unchanged makes
		// the next tick re-run the full converge.
		s.Log.Info("topology converge incomplete; will retry next interval", "revision", revision)
		return
	}

	if err := s.prune(ctx, desired); err != nil {
		// Prune failed: a stale peer may linger, but that is safe. Don't record
		// the revision so the next tick retries the prune.
		s.Log.Error(err, "pruning stale MeshPeers; will retry next interval")
		return
	}

	s.lastRevision = revision
	s.applied = true
	s.Log.Info("topology synced", "peers", len(desired), "revision", revision)
}

// reportConflicts logs any topology conflicts and notifies the OnConflicts hook.
// The hook is always invoked when set. With an empty/nil slice when there are no
// conflicts, so a previously-reported conflict state can be cleared.
func (s *Syncer) reportConflicts(peers []dwxclient.RemotePeerConfig) {
	conflicts := topology.DetectTopologyConflicts(toIdentities(peers))
	for _, c := range conflicts {
		s.Log.Info("topology conflict", "cluster", c.ClusterID, "reason", c.Reason)
	}
	if s.OnConflicts != nil {
		s.OnConflicts(conflicts)
	}
}

// upsertDesired upserts a MeshPeer for every valid remote peer skipping invalid
// entries and our own cluster, returning the set of MeshPeer names written so
// prune knows which to retain. ok is false if any upsert failed, so the caller
// can skip pruning and the revision short-circuit and retry on the next tick.
func (s *Syncer) upsertDesired(ctx context.Context, peers []dwxclient.RemotePeerConfig) (map[string]struct{}, bool) {
	desired := map[string]struct{}{}
	ok := true
	for i := range peers {
		if peers[i].ClusterID == "" || peers[i].ClusterID == s.LocalClusterID {
			continue // skip invalid peers and our own cluster
		}
		name := topology.SanitizeName(peers[i].ClusterID)
		if err := s.upsert(ctx, peers[i]); err != nil {
			s.Log.Error(err, "upserting MeshPeer", "clusterID", peers[i].ClusterID)
			ok = false
			continue
		}
		desired[name] = struct{}{}
	}
	return desired, ok
}

// fetch retrieves the topology, using the revision-aware path when the client
// supports it so syncOnce can short-circuit unchanged revisions.
func (s *Syncer) fetch(ctx context.Context) ([]dwxclient.RemotePeerConfig, string, error) {
	if rc, ok := s.CP.(dwxclient.RevisionedControlPlane); ok {
		return rc.FetchTopologyWithRevision(ctx)
	}
	peers, err := s.CP.FetchTopology(ctx)
	return peers, "", err
}

// upsert creates or updates the MeshPeer CRD for a remote peer, stamping the
// managed-by label so prune can recognize it later.
func (s *Syncer) upsert(ctx context.Context, rc dwxclient.RemotePeerConfig) error {
	mp := &networkingv1alpha1.MeshPeer{}
	mp.Name = topology.SanitizeName(rc.ClusterID)

	_, err := controllerutil.CreateOrUpdate(ctx, s.K8s, mp, func() error {
		if mp.Labels == nil {
			mp.Labels = map[string]string{}
		}
		mp.Labels[managedByLabel] = managedByValue
		mp.Spec.ClusterID = rc.ClusterID
		mp.Spec.PublicKey = rc.PublicKey
		mp.Spec.Endpoint = rc.Endpoint
		mp.Spec.PodCIDRs = rc.PodCIDRs
		mp.Spec.ServiceCIDRs = rc.ServiceCIDRs
		return nil
	})
	if err != nil {
		return fmt.Errorf("upserting MeshPeer %s: %w", mp.Name, err)
	}
	return nil
}

// prune deletes MeshPeers this syncer authored carrying the managed-by label
// whose names are absent from the desired set.  A cluster removed from the
// control plane's topology is torn down. Peers authored by a human or GitOps
// pipeline (without the label) are never touched.
func (s *Syncer) prune(ctx context.Context, desired map[string]struct{}) error {
	var list networkingv1alpha1.MeshPeerList
	if err := s.K8s.List(ctx, &list, ctrlclient.MatchingLabels{managedByLabel: managedByValue}); err != nil {
		return fmt.Errorf("listing managed MeshPeers: %w", err)
	}
	for i := range list.Items {
		mp := &list.Items[i]
		if _, keep := desired[mp.Name]; keep {
			continue
		}
		if mp.GetDeletionTimestamp() != nil {
			continue // already being torn down
		}
		if err := s.K8s.Delete(ctx, mp); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting stale MeshPeer %s: %w", mp.Name, err)
		}
		s.Log.Info("pruned stale MeshPeer", "name", mp.Name)
	}
	return nil
}

// toIdentities projects control-plane peer configs into the pure topology view
// used for conflict detection.
func toIdentities(peers []dwxclient.RemotePeerConfig) []topology.PeerIdentity {
	out := make([]topology.PeerIdentity, 0, len(peers))
	for _, p := range peers {
		out = append(out, topology.PeerIdentity{
			ClusterID: p.ClusterID,
			PublicKey: p.PublicKey,
			Endpoint:  p.Endpoint,
			CIDRs:     p.AllowedIPs(),
		})
	}
	return out
}
