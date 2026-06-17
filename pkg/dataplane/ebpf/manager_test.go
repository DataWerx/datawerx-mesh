package ebpf_test

import (
	"errors"
	"testing"

	"github.com/go-logr/logr"

	"github.com/DataWerx/datawerx-mesh/pkg/dataplane/ebpf"
	"github.com/DataWerx/datawerx-mesh/pkg/nat"
)

// fakeMapOps is an in-memory MapOps that records operation counts so tests can
// assert the reconcile applies a minimal diff.
type fakeMapOps struct {
	maps     map[ebpf.MapID]map[ebpf.TrieKey]ebpf.Rewrite
	updates  int
	deletes  int
	failList bool
}

func newFakeMapOps() *fakeMapOps {
	return &fakeMapOps{maps: map[ebpf.MapID]map[ebpf.TrieKey]ebpf.Rewrite{
		ebpf.IngressMap: {},
		ebpf.EgressMap:  {},
	}}
}

func (f *fakeMapOps) List(m ebpf.MapID) (map[ebpf.TrieKey]ebpf.Rewrite, error) {
	if f.failList {
		return nil, errors.New("boom")
	}
	out := map[ebpf.TrieKey]ebpf.Rewrite{}
	for k, v := range f.maps[m] {
		out[k] = v
	}
	return out, nil
}

func (f *fakeMapOps) Update(m ebpf.MapID, key ebpf.TrieKey, val ebpf.Rewrite) error {
	f.maps[m][key] = val
	f.updates++
	return nil
}

func (f *fakeMapOps) Delete(m ebpf.MapID, key ebpf.TrieKey) error {
	delete(f.maps[m], key)
	f.deletes++
	return nil
}

func (f *fakeMapOps) Close() error { return nil }

func TestManager_SyncRemap_ProgramsBothMaps(t *testing.T) {
	ops := newFakeMapOps()
	m := ebpf.NewManager(ops, logr.Discard())

	err := m.SyncRemap([]nat.RemapEntry{{Real: "10.244.0.0/16", Virtual: "172.20.0.0/16"}})
	if err != nil {
		t.Fatalf("SyncRemap: %v", err)
	}
	if len(ops.maps[ebpf.IngressMap]) != 1 || len(ops.maps[ebpf.EgressMap]) != 1 {
		t.Errorf("expected one entry per map, got ingress=%d egress=%d",
			len(ops.maps[ebpf.IngressMap]), len(ops.maps[ebpf.EgressMap]))
	}
}

func TestManager_SyncRemap_IsIncremental(t *testing.T) {
	ops := newFakeMapOps()
	m := ebpf.NewManager(ops, logr.Discard())

	if err := m.SyncRemap([]nat.RemapEntry{{Real: "10.244.0.0/16", Virtual: "172.20.0.0/16"}}); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	updatesAfterFirst := ops.updates

	// Re-syncing identical state must be a no-op (no updates, no deletes).
	if err := m.SyncRemap([]nat.RemapEntry{{Real: "10.244.0.0/16", Virtual: "172.20.0.0/16"}}); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if ops.updates != updatesAfterFirst {
		t.Errorf("identical re-sync wrote %d extra updates, want 0", ops.updates-updatesAfterFirst)
	}
	if ops.deletes != 0 {
		t.Errorf("identical re-sync deleted %d, want 0", ops.deletes)
	}
}

func TestManager_SyncRemap_PrunesStale(t *testing.T) {
	ops := newFakeMapOps()
	m := ebpf.NewManager(ops, logr.Discard())

	if err := m.SyncRemap([]nat.RemapEntry{
		{Real: "10.244.0.0/16", Virtual: "172.20.0.0/16"},
		{Real: "10.60.0.0/16", Virtual: "172.30.0.0/16"},
	}); err != nil {
		t.Fatalf("seed sync: %v", err)
	}

	// Drop one entry; reconcile must delete exactly it from both maps.
	if err := m.SyncRemap([]nat.RemapEntry{{Real: "10.244.0.0/16", Virtual: "172.20.0.0/16"}}); err != nil {
		t.Fatalf("shrink sync: %v", err)
	}
	if len(ops.maps[ebpf.IngressMap]) != 1 || len(ops.maps[ebpf.EgressMap]) != 1 {
		t.Errorf("stale entry not pruned: ingress=%d egress=%d",
			len(ops.maps[ebpf.IngressMap]), len(ops.maps[ebpf.EgressMap]))
	}
	if ops.deletes != 2 { // one stale key in each of the two maps
		t.Errorf("deletes = %d, want 2", ops.deletes)
	}
}

func TestManager_SyncRemap_ListErrorSurfaces(t *testing.T) {
	ops := newFakeMapOps()
	ops.failList = true
	m := ebpf.NewManager(ops, logr.Discard())
	if err := m.SyncRemap([]nat.RemapEntry{{Real: "10.244.0.0/16", Virtual: "172.20.0.0/16"}}); err == nil {
		t.Fatal("expected error when map List fails")
	}
}

func TestLoad_OpenCoreReturnsNotCompiled(t *testing.T) {
	if _, err := ebpf.Load("dwx-mesh0", logr.Discard()); !errors.Is(err, ebpf.ErrNotCompiled) {
		t.Errorf("open-core Load() = %v, want ErrNotCompiled", err)
	}
}
