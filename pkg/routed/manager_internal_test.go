package routed

import (
	"errors"
	"testing"

	"github.com/go-logr/logr"
)

// fakeOps is an in-memory routeOps recording the live route set and op counts.
type fakeOps struct {
	live     map[Route]struct{}
	replaces int
	deletes  int
	failNext bool
}

func newFakeOps() *fakeOps { return &fakeOps{live: map[Route]struct{}{}} }

func (f *fakeOps) Replace(r Route) error {
	if f.failNext {
		f.failNext = false
		return errors.New("boom")
	}
	f.live[r] = struct{}{}
	f.replaces++
	return nil
}

func (f *fakeOps) Delete(r Route) error {
	delete(f.live, r)
	f.deletes++
	return nil
}

func newTestManager(ops routeOps) *Manager {
	return &Manager{ops: ops, peerRoutes: map[string][]Route{}, log: logr.Discard()}
}

func TestManager_ConfigureInstallsAndTracks(t *testing.T) {
	ops := newFakeOps()
	m := newTestManager(ops)

	if err := m.ConfigurePeer("cluster-b", "100.64.0.5", []string{"10.96.0.0/16", "10.40.0.0/16"}); err != nil {
		t.Fatalf("ConfigurePeer: %v", err)
	}
	if len(ops.live) != 2 {
		t.Fatalf("expected 2 live routes, got %d", len(ops.live))
	}
	if _, ok := ops.live[Route{Dest: "10.96.0.0/16", Via: "100.64.0.5"}]; !ok {
		t.Error("expected route for 10.96.0.0/16 via overlay next-hop")
	}
}

func TestManager_ConfigureConvergesOnChange(t *testing.T) {
	ops := newFakeOps()
	m := newTestManager(ops)

	_ = m.ConfigurePeer("b", "100.64.0.5", []string{"10.96.0.0/16", "10.40.0.0/16"})
	// Drop one CIDR and add another: the removed one must be withdrawn.
	if err := m.ConfigurePeer("b", "100.64.0.5", []string{"10.96.0.0/16", "10.50.0.0/16"}); err != nil {
		t.Fatalf("reconfigure: %v", err)
	}
	if _, ok := ops.live[Route{Dest: "10.40.0.0/16", Via: "100.64.0.5"}]; ok {
		t.Error("stale route 10.40.0.0/16 should have been withdrawn")
	}
	if _, ok := ops.live[Route{Dest: "10.50.0.0/16", Via: "100.64.0.5"}]; !ok {
		t.Error("new route 10.50.0.0/16 should have been installed")
	}
}

func TestManager_NextHopChangeRewithdrawsOld(t *testing.T) {
	ops := newFakeOps()
	m := newTestManager(ops)

	_ = m.ConfigurePeer("b", "100.64.0.5", []string{"10.96.0.0/16"})
	// Overlay address of the remote gateway changed.
	_ = m.ConfigurePeer("b", "100.64.0.9", []string{"10.96.0.0/16"})

	if _, ok := ops.live[Route{Dest: "10.96.0.0/16", Via: "100.64.0.5"}]; ok {
		t.Error("route via the old next-hop should have been withdrawn")
	}
	if _, ok := ops.live[Route{Dest: "10.96.0.0/16", Via: "100.64.0.9"}]; !ok {
		t.Error("route via the new next-hop should be present")
	}
}

func TestManager_RemovePeerWithdrawsAll(t *testing.T) {
	ops := newFakeOps()
	m := newTestManager(ops)

	_ = m.ConfigurePeer("b", "100.64.0.5", []string{"10.96.0.0/16", "10.40.0.0/16"})
	if err := m.RemovePeer("b"); err != nil {
		t.Fatalf("RemovePeer: %v", err)
	}
	if len(ops.live) != 0 {
		t.Errorf("expected all routes withdrawn, %d remain", len(ops.live))
	}
	// Idempotent: removing again is a no-op.
	if err := m.RemovePeer("b"); err != nil {
		t.Errorf("second RemovePeer should be a no-op: %v", err)
	}
}

func TestManager_ConfigureRollsBackOnError(t *testing.T) {
	ops := newFakeOps()
	ops.failNext = true // first Replace fails
	m := newTestManager(ops)

	if err := m.ConfigurePeer("b", "100.64.0.5", []string{"10.96.0.0/16", "10.40.0.0/16"}); err == nil {
		t.Fatal("expected ConfigurePeer to fail when a route install fails")
	}
	if len(ops.live) != 0 {
		t.Errorf("expected rollback to leave no routes, got %d", len(ops.live))
	}
}

func TestManager_PeerHandshakeZero(t *testing.T) {
	m := newTestManager(newFakeOps())
	hs, err := m.PeerHandshake("b")
	if err != nil || hs != 0 {
		t.Errorf("PeerHandshake = (%d, %v), want (0, nil)", hs, err)
	}
}
