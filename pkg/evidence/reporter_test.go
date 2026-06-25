package evidence

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr"

	"github.com/DataWerx/datawerx-mesh/pkg/verify"
)

// fakeSink records the payloads pushed to it and can be made to fail.
type fakeSink struct {
	mu       sync.Mutex
	payloads [][]byte
	err      error
}

func (f *fakeSink) PushEvidence(_ context.Context, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.payloads = append(f.payloads, append([]byte(nil), payload...))
	return nil
}

func (f *fakeSink) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.payloads)
}

// snapWith builds a snapshot carrying a peer, so the assembled evidence is
// non-trivial and changes when the peer changes.
func snapWith(phase, msg string) verify.Snapshot {
	return verify.BuildSnapshot(verify.SnapshotInputs{
		Now:   1000,
		Peers: []verify.PeerSnapshot{{ClusterID: "a", Phase: phase, Message: msg}},
	})
}

func newReporter(snap SnapshotFunc, sink Sink) *Reporter {
	return &Reporter{Snapshot: snap, Sink: sink, Log: logr.Discard()}
}

func TestReportOncePushesAssembledEvidence(t *testing.T) {
	sink := &fakeSink{}
	r := newReporter(func(context.Context) (verify.Snapshot, error) {
		return snapWith("Error", "overlap"), nil
	}, sink)

	r.reportOnce(context.Background())

	if sink.count() != 1 {
		t.Fatalf("expected one push, got %d", sink.count())
	}
	// The payload is the grounded evidence JSON — it must carry the diagnosis the
	// engines derive from the snapshot.
	payload := string(sink.payloads[0])
	if !strings.Contains(payload, "diagnosis") || !strings.Contains(payload, "peer[a]") {
		t.Errorf("payload is not the grounded evidence: %s", payload)
	}
}

func TestReportOnceSkipsUnchangedEvidence(t *testing.T) {
	sink := &fakeSink{}
	r := newReporter(func(context.Context) (verify.Snapshot, error) {
		return snapWith("Connected", ""), nil
	}, sink)

	r.reportOnce(context.Background()) // first push
	r.reportOnce(context.Background()) // identical evidence -> skipped
	if sink.count() != 1 {
		t.Fatalf("unchanged evidence should not push again; got %d pushes", sink.count())
	}

	// A change re-pushes.
	r.Snapshot = func(context.Context) (verify.Snapshot, error) { return snapWith("Error", "boom"), nil }
	r.reportOnce(context.Background())
	if sink.count() != 2 {
		t.Fatalf("changed evidence should push; got %d pushes", sink.count())
	}
}

func TestReportOnceSnapshotErrorIsSwallowed(t *testing.T) {
	sink := &fakeSink{}
	r := newReporter(func(context.Context) (verify.Snapshot, error) {
		return verify.Snapshot{}, errors.New("api blip")
	}, sink)
	r.reportOnce(context.Background()) // must not panic or push
	if sink.count() != 0 {
		t.Errorf("a gather failure must not push, got %d", sink.count())
	}
}

func TestReportOncePushErrorDoesNotMarkSent(t *testing.T) {
	sink := &fakeSink{err: errors.New("503")}
	r := newReporter(func(context.Context) (verify.Snapshot, error) {
		return snapWith("Connected", ""), nil
	}, sink)
	r.reportOnce(context.Background()) // push fails

	// A failed push must NOT update lastHash, so the next attempt retries the same
	// evidence rather than wrongly short-circuiting it as "already sent".
	sink.err = nil
	r.reportOnce(context.Background())
	if sink.count() != 1 {
		t.Fatalf("expected the retry to push after the earlier failure, got %d", sink.count())
	}
}

func TestStartPrimesThenStopsOnCancel(t *testing.T) {
	sink := &fakeSink{}
	r := newReporter(func(context.Context) (verify.Snapshot, error) {
		return snapWith("Connected", ""), nil
	}, sink)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before Start so the loop primes once then returns immediately
	if err := r.Start(ctx); err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	if sink.count() != 1 {
		t.Errorf("Start should prime exactly one report before exiting, got %d", sink.count())
	}
	if r.Interval <= 0 {
		t.Error("Start should default the interval")
	}
}
