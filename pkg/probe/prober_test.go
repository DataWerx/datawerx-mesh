package probe

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestProberCycleRecordsResults runs a single cycle against a fake dialer and a
// fixed peer list, asserting the prober plans the right targets, records every
// outcome, and projects them into the slo.Liveness signal — all without a
// network.
func TestProberCycleRecordsResults(t *testing.T) {
	peers := []Peer{
		{ClusterID: "up", Connected: true, ProbeAddress: "10.0.0.1:9998"},
		{ClusterID: "broken", Connected: true, ProbeAddress: "10.0.0.2:9998"},
		{ClusterID: "down", Connected: false, ProbeAddress: "10.0.0.3:9998"},
	}

	var dialed []string
	fakeProbe := func(_ context.Context, t Target) Result {
		dialed = append(dialed, t.ClusterID)
		if t.ClusterID == "broken" {
			return Result{ClusterID: t.ClusterID, OK: false, Reason: "refused", ObservedAtUnix: 1000}
		}
		return Result{ClusterID: t.ClusterID, OK: true, RTTMillis: 3, ObservedAtUnix: 1000}
	}

	p := &Prober{
		Peers: func(context.Context) ([]Peer, error) { return peers, nil },
		Probe: fakeProbe,
		Now:   func() int64 { return 1000 },
	}
	p.cycle(context.Background())

	// "down" is never dialed; "broken" and "up" are, in sorted order.
	if len(dialed) != 2 || dialed[0] != "broken" || dialed[1] != "up" {
		t.Fatalf("dialed = %v, want [broken up]", dialed)
	}
	if r, ok := p.Observations().Latest("up"); !ok || !r.OK {
		t.Errorf("up should be recorded healthy, got %#v ok=%v", r, ok)
	}
	if r, ok := p.Observations().Latest("broken"); !ok || r.OK {
		t.Errorf("broken should be recorded failed, got %#v ok=%v", r, ok)
	}

	live := p.Observations().Liveness(1000)
	if live["up"].HandshakeAge != 0 {
		t.Errorf("up liveness age = %d, want 0 (just probed)", live["up"].HandshakeAge)
	}
	if live["broken"].HandshakeAge >= 0 {
		t.Errorf("broken liveness age = %d, want negative (not live)", live["broken"].HandshakeAge)
	}
}

// TestProberCyclePublishesResults confirms a cycle hands every dialed result to
// the Publisher so they can be written back to the mesh.
func TestProberCyclePublishesResults(t *testing.T) {
	var published []Result
	p := &Prober{
		Peers: func(context.Context) ([]Peer, error) {
			return []Peer{
				{ClusterID: "a", Connected: true, ProbeAddress: "10.0.0.1:9998"},
				{ClusterID: "b", Connected: true, ProbeAddress: "10.0.0.2:9998"},
			}, nil
		},
		Probe: func(_ context.Context, t Target) Result {
			return Result{ClusterID: t.ClusterID, OK: t.ClusterID == "a", ObservedAtUnix: 1000}
		},
		Publish: func(_ context.Context, results []Result) error {
			published = append(published, results...)
			return nil
		},
		Now: func() int64 { return 1000 },
	}
	p.cycle(context.Background())

	if len(published) != 2 {
		t.Fatalf("expected both results published, got %d", len(published))
	}
	if published[0].ClusterID != "a" || !published[0].OK || published[1].ClusterID != "b" || published[1].OK {
		t.Errorf("published results not as dialed: %#v", published)
	}
}

// TestProberCyclePublishErrorIsNonFatal confirms a Publisher error does not stop
// the cycle from recording its observations.
func TestProberCyclePublishErrorIsNonFatal(t *testing.T) {
	p := &Prober{
		Peers: func(context.Context) ([]Peer, error) {
			return []Peer{{ClusterID: "a", Connected: true, ProbeAddress: "x:1"}}, nil
		},
		Probe: func(_ context.Context, t Target) Result {
			return Result{ClusterID: t.ClusterID, OK: true, ObservedAtUnix: 1}
		},
		Publish: func(context.Context, []Result) error { return context.DeadlineExceeded },
		Now:     func() int64 { return 1 },
	}
	p.cycle(context.Background())
	if r, ok := p.Observations().Latest("a"); !ok || !r.OK {
		t.Error("a publish failure must not drop the in-memory observation")
	}
}

// TestProberCycleListerError confirms a failed peer list skips the cycle without
// recording anything or panicking.
func TestProberCycleListerError(t *testing.T) {
	called := false
	p := &Prober{
		Peers: func(context.Context) ([]Peer, error) { return nil, context.DeadlineExceeded },
		Probe: func(context.Context, Target) Result { called = true; return Result{} },
		Now:   func() int64 { return 1 },
	}
	p.cycle(context.Background())
	if called {
		t.Error("no target should be dialed when the peer list fails")
	}
}

// TestProberStartDefaults runs Start with no Probe, Now, or Interval set so the
// default dialer, clock, and cadence are installed, against a peer list with no
// probeable targets, then cancels — covering the default-assignment path with no
// real dial.
func TestProberStartDefaults(t *testing.T) {
	p := &Prober{
		Peers: func(context.Context) ([]Peer, error) { return []Peer{{ClusterID: "down"}}, nil },
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Start(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Start returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after cancellation")
	}
}

// TestProberStartProbesImmediatelyAndStops confirms the loop probes once up
// front (so the first signal is available without an interval wait) and exits
// cleanly on context cancellation.
func TestProberStartProbesImmediatelyAndStops(t *testing.T) {
	var mu sync.Mutex
	cycles := 0
	p := &Prober{
		Interval: time.Hour, // long, so only the immediate probe runs
		Peers: func(context.Context) ([]Peer, error) {
			return []Peer{{ClusterID: "a", Connected: true, ProbeAddress: "x:1"}}, nil
		},
		Probe: func(context.Context, Target) Result {
			mu.Lock()
			cycles++
			mu.Unlock()
			return Result{ClusterID: "a", OK: true}
		},
		Now: func() int64 { return 1 },
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Start(ctx) }()

	// Give the immediate cycle a moment, then cancel.
	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		n := cycles
		mu.Unlock()
		if n >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("prober did not run its immediate cycle")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Start returned %v, want nil on cancel", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after context cancellation")
	}
}
