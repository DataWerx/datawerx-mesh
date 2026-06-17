package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestResponderHandleAnswersProbe drives the responder's handler directly and
// confirms a dialed probe round-trips: the body it returns classifies as a
// healthy probe from this cluster.
func TestResponderHandleAnswersProbe(t *testing.T) {
	r := &Responder{ClusterID: "east"}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, ResponderPath, nil)

	r.handle(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q, want application/json", ct)
	}
	got := Classify("east", rec.Code, rec.Body.Bytes(), time.Millisecond, 1, nil)
	if !got.OK {
		t.Errorf("the responder body should classify as healthy, got reason %q", got.Reason)
	}
}

// TestResponderHandleRejectsOtherPaths makes sure the responder is not a
// general-purpose endpoint.
func TestResponderHandleRejectsOtherPaths(t *testing.T) {
	r := &Responder{ClusterID: "east"}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	r.handle(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for a non-probe path", rec.Code)
	}
}

// TestResponderServesAndProberDials wires the real httpProbe against a live
// Responder over loopback — the closest the hermetic suite gets to the e2e
// path. It proves the server, the wire body, and the dialer agree.
func TestResponderServesAndProberDials(t *testing.T) {
	r := &Responder{Addr: "127.0.0.1:0", ClusterID: "east"}

	// Start the responder on an ephemeral port via httptest so the test owns the
	// lifecycle; reuse the handler the Runnable installs.
	mux := http.NewServeMux()
	mux.HandleFunc(ResponderPath, r.handle)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	addr := srv.Listener.Addr().String()
	probeFn := httpProbe(2 * time.Second)
	res := probeFn(context.Background(), Target{ClusterID: "east", Address: addr})
	if !res.OK {
		t.Fatalf("live dial of the responder failed: %q", res.Reason)
	}

	// Dialing it as the wrong cluster must surface the misroute.
	mis := probeFn(context.Background(), Target{ClusterID: "west", Address: addr})
	if mis.OK || !strings.Contains(mis.Reason, "misrouted") {
		t.Errorf("dialing as the wrong cluster should report a misroute, got OK=%v reason=%q", mis.OK, mis.Reason)
	}
}

// TestParseProbeBodyWrongMarker confirms a well-formed JSON body that lacks the
// DataWerx marker is rejected, so an unrelated JSON service cannot masquerade as
// a responder.
func TestParseProbeBodyWrongMarker(t *testing.T) {
	if _, ok := parseProbeBody([]byte(`{"marker":"something-else","clusterID":"east"}`)); ok {
		t.Error("a body without the DataWerx marker must not parse as a probe envelope")
	}
}

// TestResponderStartLifecycle starts the real Runnable on an ephemeral port,
// dials it, and confirms a cancelled context shuts it down cleanly.
func TestResponderStartLifecycle(t *testing.T) {
	r := &Responder{Addr: "127.0.0.1:18099", ClusterID: "east"}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()

	probeFn := httpProbe(2 * time.Second)
	var res Result
	deadline := time.After(2 * time.Second)
	for {
		res = probeFn(context.Background(), Target{ClusterID: "east", Address: "127.0.0.1:18099"})
		if res.OK {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("responder never answered: %q", res.Reason)
		case <-time.After(10 * time.Millisecond):
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

func TestNeedLeaderElection(t *testing.T) {
	if (&Responder{}).NeedLeaderElection() {
		t.Error("responder must run on every node, not only the leader")
	}
	if (&Prober{}).NeedLeaderElection() {
		t.Error("prober must run on every node, not only the leader")
	}
}

// TestHTTPProbeDialFailure confirms an unreachable address classifies as a
// failure rather than panicking.
func TestHTTPProbeDialFailure(t *testing.T) {
	probeFn := httpProbe(200 * time.Millisecond)
	// Port 1 on loopback refuses connections.
	res := probeFn(context.Background(), Target{ClusterID: "dead", Address: "127.0.0.1:1"})
	if res.OK {
		t.Fatal("a refused dial must not classify as healthy")
	}
	if !strings.Contains(res.Reason, "dial failed") {
		t.Errorf("reason %q should report the dial failure", res.Reason)
	}
}
