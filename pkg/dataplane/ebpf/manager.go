package ebpf

import (
	"fmt"

	"github.com/go-logr/logr"

	"github.com/DataWerx/datawerx-mesh/pkg/nat"
)

// MapOps is the seam over the kernel BPF maps. The premium build supplies a
// libbpf/cilium-ebpf-backed implementation that talks to the loaded program's
// maps via bpf(2); tests supply an in-memory fake. Keeping the Manager's
// reconcile logic above this interface means the full-state diff is
// unit-testable without a kernel.
type MapOps interface {
	// List returns the current entries in the given map, keyed by TrieKey.
	List(m MapID) (map[TrieKey]Rewrite, error)
	// Update inserts or overwrites one entry.
	Update(m MapID, key TrieKey, val Rewrite) error
	// Delete removes one entry. Removing a missing key must be a no-op.
	Delete(m MapID, key TrieKey) error
	// Close releases the program/link/map handles.
	Close() error
}

// Manager programs the eBPF overlap-remap datapath. It implements the same
// controllers.RemapDataPlane interface as nat.Manager, so it is a drop-in
// premium replacement for the iptables NETMAP backend selected at startup.
//
// SyncRemap is a full-state reconcile: it computes the desired map contents
// from the RemapEntry set and converges each map toward it with minimal
// add/update/delete operations (no flush-and-rebuild, so the datapath never has
// a window with zero rules — important for a kernel fast path).
type Manager struct {
	ops MapOps
	log logr.Logger
}

// NewManager builds a Manager over the given map-ops backend.
func NewManager(ops MapOps, log logr.Logger) *Manager {
	return &Manager{ops: ops, log: log.WithName("ebpf-remap")}
}

// SyncRemap converges both datapath maps to exactly realize entries.
func (m *Manager) SyncRemap(entries []nat.RemapEntry) error {
	maps, err := BuildRemapMaps(entries)
	if err != nil {
		return err
	}
	if err := m.reconcileMap(IngressMap, maps.Ingress); err != nil {
		return err
	}
	if err := m.reconcileMap(EgressMap, maps.Egress); err != nil {
		return err
	}
	// Full-state reconcile on every pass; debug-level (see pkg/logging).
	m.log.V(1).Info("ebpf remap synced", "entries", len(maps.Ingress))
	return nil
}

// reconcileMap diffs desired against the live map and applies the delta.
func (m *Manager) reconcileMap(id MapID, desired []MapEntry) error {
	current, err := m.ops.List(id)
	if err != nil {
		return fmt.Errorf("ebpf remap: listing %s map: %w", id, err)
	}

	want := make(map[TrieKey]Rewrite, len(desired))
	for _, e := range desired {
		want[e.Key] = e.Val
		cur, ok := current[e.Key]
		if ok && cur == e.Val {
			continue // already correct
		}
		if err := m.ops.Update(id, e.Key, e.Val); err != nil {
			return fmt.Errorf("ebpf remap: updating %s %s→%s: %w", id, e.KeyCIDR, e.ValCIDR, err)
		}
	}

	for key := range current {
		if _, keep := want[key]; keep {
			continue
		}
		if err := m.ops.Delete(id, key); err != nil {
			return fmt.Errorf("ebpf remap: deleting stale %s entry: %w", id, err)
		}
	}
	return nil
}

// Close releases the underlying map/program handles.
func (m *Manager) Close() error { return m.ops.Close() }
