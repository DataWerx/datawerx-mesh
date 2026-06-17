package probe_test

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/datawerx/datawerx/pkg/probe"
	"github.com/datawerx/datawerx/pkg/reach"
	"github.com/datawerx/datawerx/pkg/slo"
)

func TestNextProbeStatus(t *testing.T) {
	const refresh int64 = 60
	tests := []struct {
		name        string
		cur         probe.ProbeStatus
		result      probe.Result
		wantNext    probe.ProbeStatus
		wantPersist bool
	}{
		{
			name:        "first probe, success, is always persisted",
			cur:         probe.ProbeStatus{},
			result:      probe.Result{OK: true, ObservedAtUnix: 1000},
			wantNext:    probe.ProbeStatus{LastAttemptUnix: 1000, LastSuccessUnix: 1000},
			wantPersist: true,
		},
		{
			name:        "first probe, failure, is persisted",
			cur:         probe.ProbeStatus{},
			result:      probe.Result{OK: false, ObservedAtUnix: 1000},
			wantNext:    probe.ProbeStatus{LastAttemptUnix: 1000},
			wantPersist: true,
		},
		{
			name:        "healthy to failing flips state, persisted immediately",
			cur:         probe.ProbeStatus{LastAttemptUnix: 1000, LastSuccessUnix: 1000},
			result:      probe.Result{OK: false, ObservedAtUnix: 1010},
			wantNext:    probe.ProbeStatus{LastAttemptUnix: 1010, LastSuccessUnix: 1000},
			wantPersist: true,
		},
		{
			name:        "failing to healthy flips state, persisted immediately",
			cur:         probe.ProbeStatus{LastAttemptUnix: 1000, LastSuccessUnix: 500},
			result:      probe.Result{OK: true, ObservedAtUnix: 1010},
			wantNext:    probe.ProbeStatus{LastAttemptUnix: 1010, LastSuccessUnix: 1010},
			wantPersist: true,
		},
		{
			name:        "steady healthy within refresh window is not persisted",
			cur:         probe.ProbeStatus{LastAttemptUnix: 1000, LastSuccessUnix: 1000},
			result:      probe.Result{OK: true, ObservedAtUnix: 1030},
			wantNext:    probe.ProbeStatus{LastAttemptUnix: 1030, LastSuccessUnix: 1030},
			wantPersist: false,
		},
		{
			name:        "steady healthy past refresh window is persisted to keep age fresh",
			cur:         probe.ProbeStatus{LastAttemptUnix: 1000, LastSuccessUnix: 1000},
			result:      probe.Result{OK: true, ObservedAtUnix: 1065},
			wantNext:    probe.ProbeStatus{LastAttemptUnix: 1065, LastSuccessUnix: 1065},
			wantPersist: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			next, persist := probe.NextProbeStatus(tt.cur, tt.result, refresh)
			if next != tt.wantNext {
				t.Errorf("next = %#v, want %#v", next, tt.wantNext)
			}
			if persist != tt.wantPersist {
				t.Errorf("persist = %v, want %v", persist, tt.wantPersist)
			}
		})
	}
}

func TestPlanTargets(t *testing.T) {
	tests := []struct {
		name  string
		peers []probe.Peer
		want  []probe.Target
	}{
		{
			name: "only connected, conflict-free peers with an address are probed",
			peers: []probe.Peer{
				{ClusterID: "b", Connected: true, ProbeAddress: "10.0.0.2:9998"},
				{ClusterID: "a", Connected: true, ProbeAddress: "10.0.0.1:9998"},
				{ClusterID: "down", Connected: false, ProbeAddress: "10.0.0.3:9998"},
				{ClusterID: "conflict", Connected: true, Conflict: true, ProbeAddress: "10.0.0.4:9998"},
				{ClusterID: "noaddr", Connected: true},
			},
			want: []probe.Target{
				{ClusterID: "a", Address: "10.0.0.1:9998"},
				{ClusterID: "b", Address: "10.0.0.2:9998"},
			},
		},
		{
			name:  "no probeable peers yields an empty plan",
			peers: []probe.Peer{{ClusterID: "down", Connected: false}},
			want:  []probe.Target{},
		},
		{
			name:  "nil input yields an empty plan",
			peers: nil,
			want:  []probe.Target{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := probe.PlanTargets(tt.peers)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("PlanTargets() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestClassify(t *testing.T) {
	const now int64 = 1000
	okBody := probe.ProbeBody("east")

	tests := []struct {
		name       string
		cluster    string
		status     int
		body       []byte
		rtt        time.Duration
		dialErr    error
		wantOK     bool
		wantRTT    int64
		reasonHint string
	}{
		{
			name: "healthy probe from the expected cluster", cluster: "east",
			status: 200, body: okBody, rtt: 12 * time.Millisecond,
			wantOK: true, wantRTT: 12,
		},
		{
			name: "dial error is a failure", cluster: "east",
			dialErr: errors.New("connection refused"), reasonHint: "dial failed",
		},
		{
			name: "non-200 is a failure", cluster: "east",
			status: 503, body: okBody, reasonHint: "HTTP 503",
		},
		{
			name: "unrecognized body is a failure", cluster: "east",
			status: 200, body: []byte("OK"), reasonHint: "envelope",
		},
		{
			name: "wrong cluster answering is a misroute failure", cluster: "east",
			status: 200, body: probe.ProbeBody("west"), reasonHint: "misrouted",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := probe.Classify(tt.cluster, tt.status, tt.body, tt.rtt, now, tt.dialErr)
			if r.OK != tt.wantOK {
				t.Fatalf("OK = %v, want %v (reason %q)", r.OK, tt.wantOK, r.Reason)
			}
			if r.ClusterID != tt.cluster {
				t.Errorf("ClusterID = %q, want %q", r.ClusterID, tt.cluster)
			}
			if r.ObservedAtUnix != now {
				t.Errorf("ObservedAtUnix = %d, want %d", r.ObservedAtUnix, now)
			}
			if tt.wantOK {
				if r.RTTMillis != tt.wantRTT {
					t.Errorf("RTTMillis = %d, want %d", r.RTTMillis, tt.wantRTT)
				}
				if r.Reason != "" {
					t.Errorf("success should carry no reason, got %q", r.Reason)
				}
				return
			}
			if r.RTTMillis != 0 {
				t.Errorf("failed probe should report no RTT, got %d", r.RTTMillis)
			}
			if tt.reasonHint != "" && !contains(r.Reason, tt.reasonHint) {
				t.Errorf("reason %q should mention %q", r.Reason, tt.reasonHint)
			}
		})
	}
}

func TestProbeBodyRoundTrip(t *testing.T) {
	body := probe.ProbeBody("payments-cluster")
	r := probe.Classify("payments-cluster", 200, body, time.Millisecond, 1, nil)
	if !r.OK {
		t.Fatalf("a body built by ProbeBody must classify as healthy, got reason %q", r.Reason)
	}
}

func TestObservationsLiveness(t *testing.T) {
	var obs probe.Observations
	obs.Record(probe.Result{ClusterID: "fresh", OK: true, ObservedAtUnix: 950})
	obs.Record(probe.Result{ClusterID: "stale", OK: true, ObservedAtUnix: 100})
	obs.Record(probe.Result{ClusterID: "failed", OK: false, ObservedAtUnix: 990})

	const now int64 = 1000
	const staleAfter int64 = 300
	live := obs.Liveness(now)

	if got := live["fresh"].HandshakeAge; got != 50 {
		t.Errorf("fresh age = %d, want 50", got)
	}
	if got := live["stale"].HandshakeAge; got != 900 {
		t.Errorf("stale age = %d, want 900", got)
	}
	if got := live["failed"].HandshakeAge; got >= 0 {
		t.Errorf("failed probe age = %d, want negative (not live)", got)
	}

	// The whole point: this feeds slo.Assess unchanged. A fresh probe is live,
	// a stale or failed one is not.
	if !liveAt(live["fresh"], staleAfter) {
		t.Error("a fresh successful probe must read as live")
	}
	if liveAt(live["stale"], staleAfter) {
		t.Error("a stale probe must not read as live")
	}
	if liveAt(live["failed"], staleAfter) {
		t.Error("a failed probe must not read as live")
	}
}

// liveAt mirrors slo.Liveness.live for the assertions above without reaching
// into slo internals; it keeps the test honest about the contract the prober
// feeds.
func liveAt(l slo.Liveness, staleAfter int64) bool {
	return l.HandshakeAge >= 0 && l.HandshakeAge <= staleAfter
}

func TestObservationsClockSkew(t *testing.T) {
	var obs probe.Observations
	// A probe observed slightly in the future relative to now must not produce a
	// negative age that would read as "not live".
	obs.Record(probe.Result{ClusterID: "skewed", OK: true, ObservedAtUnix: 1005})
	if got := obs.Liveness(1000)["skewed"].HandshakeAge; got != 0 {
		t.Errorf("clock-skewed age = %d, want clamped to 0", got)
	}
}

func TestObservationsLatest(t *testing.T) {
	var obs probe.Observations
	if _, ok := obs.Latest("x"); ok {
		t.Error("an unrecorded cluster should report no latest result")
	}
	obs.Record(probe.Result{ClusterID: "x", OK: true, RTTMillis: 7})
	obs.Record(probe.Result{ClusterID: "x", OK: false, Reason: "later"})
	got, ok := obs.Latest("x")
	if !ok || got.OK || got.Reason != "later" {
		t.Errorf("Latest should return the most recent result, got %#v ok=%v", got, ok)
	}
}

// TestProberSignalFeedsSLO is the design claim made executable: the prober's
// Observations project into the exact slo.Liveness map slo.Assess consumes, so
// a peer that reach says *should* be reachable but the prober cannot dial comes
// out Impaired — the "should reach, can't" verdict — with no change to the slo
// engine.
func TestProberSignalFeedsSLO(t *testing.T) {
	matrix := reach.Matrix{
		Reachabilities: []reach.Reachability{
			{Cluster: "healthy", Status: reach.StatusReachable},
			{Cluster: "impaired", Status: reach.StatusReachable},
		},
	}

	var obs probe.Observations
	obs.Record(probe.Result{ClusterID: "healthy", OK: true, ObservedAtUnix: 990})
	obs.Record(probe.Result{ClusterID: "impaired", OK: false, ObservedAtUnix: 990})

	report := slo.Assess(matrix, obs.Liveness(1000), 300)
	got := map[string]slo.Verdict{}
	for _, s := range report.Signals {
		got[s.Cluster] = s.Verdict
	}
	if got["healthy"] != slo.VerdictHealthy {
		t.Errorf("a peer the prober reaches should be Healthy, got %q", got["healthy"])
	}
	if got["impaired"] != slo.VerdictImpaired {
		t.Errorf("a reachable peer the prober cannot dial should be Impaired, got %q", got["impaired"])
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
