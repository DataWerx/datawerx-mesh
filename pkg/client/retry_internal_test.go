package client

import (
	"context"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetryPolicy_Backoff(t *testing.T) {
	p := RetryPolicy{MaxAttempts: 6, BaseDelay: 100 * time.Millisecond, MaxDelay: 1 * time.Second}
	want := []time.Duration{
		100 * time.Millisecond,
		200 * time.Millisecond,
		400 * time.Millisecond,
		800 * time.Millisecond,
		1 * time.Second, // capped
		1 * time.Second, // capped
	}
	for i, w := range want {
		if got := p.backoff(i); got != w {
			t.Errorf("backoff(%d) = %v, want %v", i, got, w)
		}
	}

	// A zero base delay disables backoff entirely.
	if got := (RetryPolicy{BaseDelay: 0}).backoff(3); got != 0 {
		t.Errorf("zero-base backoff = %v, want 0", got)
	}
}

func TestRetryPolicy_Attempts(t *testing.T) {
	if got := (RetryPolicy{MaxAttempts: 0}).attempts(); got != 1 {
		t.Errorf("attempts(0) = %d, want 1", got)
	}
	if got := (RetryPolicy{MaxAttempts: 5}).attempts(); got != 5 {
		t.Errorf("attempts(5) = %d, want 5", got)
	}
}

func TestIsRetryableStatus(t *testing.T) {
	retryable := []int{429, 500, 502, 503, 504}
	terminal := []int{200, 301, 400, 401, 403, 404, 422}
	for _, c := range retryable {
		if !isRetryableStatus(c) {
			t.Errorf("status %d should be retryable", c)
		}
	}
	for _, c := range terminal {
		if isRetryableStatus(c) {
			t.Errorf("status %d should be terminal", c)
		}
	}
}

func TestFullJitter_Bounds(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	d := 100 * time.Millisecond
	for i := 0; i < 1000; i++ {
		j := fullJitter(d, rng)
		if j < 0 || j > d {
			t.Fatalf("jitter %v out of [0,%v]", j, d)
		}
	}
	// nil rng yields the full duration (jitter disabled).
	if got := fullJitter(d, nil); got != d {
		t.Errorf("nil-rng jitter = %v, want %v", got, d)
	}
	if got := fullJitter(0, rng); got != 0 {
		t.Errorf("zero-duration jitter = %v, want 0", got)
	}
}

// noSleep is an injectable sleeper that records call count and never waits.
func noSleep(calls *int32) func(context.Context, time.Duration) error {
	return func(ctx context.Context, _ time.Duration) error {
		atomic.AddInt32(calls, 1)
		return ctx.Err()
	}
}

func TestAuthorizedGet_RetriesTransientThenSucceeds(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable) // 503 twice
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var sleeps int32
	c := NewEnterpriseControlPlaneClient(srv.URL,
		WithTokenLoader(func() string { return "tok" }),
		WithRetryPolicy(RetryPolicy{MaxAttempts: 5, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}),
		withoutJitter(),
		withSleeper(noSleep(&sleeps)),
	)
	c.token = "tok"

	resp, err := c.authorizedGet(context.Background(), "/x")
	if err != nil {
		t.Fatalf("authorizedGet: %v", err)
	}
	defer drainAndClose(resp)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if hits != 3 {
		t.Errorf("server hits = %d, want 3 (2 failures + 1 success)", hits)
	}
	if sleeps != 2 {
		t.Errorf("backoff sleeps = %d, want 2", sleeps)
	}
}

func TestAuthorizedGet_ExhaustsRetries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway) // always 502
	}))
	defer srv.Close()

	var sleeps int32
	c := NewEnterpriseControlPlaneClient(srv.URL,
		WithTokenLoader(func() string { return "tok" }),
		WithRetryPolicy(RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}),
		withSleeper(noSleep(&sleeps)),
	)
	c.token = "tok"

	if _, err := c.authorizedGet(context.Background(), "/x"); err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if sleeps != 2 { // 3 attempts → 2 inter-attempt sleeps
		t.Errorf("backoff sleeps = %d, want 2", sleeps)
	}
}

func TestAuthorizedGet_TokenRefreshOn401(t *testing.T) {
	const goodToken = "fresh-token"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer "+goodToken {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	// The loader always returns the current rotated token; only the cached
	// c.token is stale, modeling an OIDC re-issue the agent hasn't picked up yet.
	// The first attempt uses the stale cached token (401), the refresh re-reads
	// the loader and the retry succeeds.
	c := NewEnterpriseControlPlaneClient(srv.URL,
		WithTokenLoader(func() string { return goodToken }),
		withSleeper(func(context.Context, time.Duration) error { return nil }),
	)
	c.token = "stale-token"

	resp, err := c.authorizedGet(context.Background(), "/api/v1/topology")
	if err != nil {
		t.Fatalf("authorizedGet: %v", err)
	}
	defer drainAndClose(resp)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 after refresh", resp.StatusCode)
	}
	if c.token != goodToken {
		t.Errorf("token not refreshed: %q", c.token)
	}
}

func TestAuthorizedGet_RefreshOnlyOnce(t *testing.T) {
	// Persistent 403 must not loop forever: refresh fires once, then the 403 is
	// returned verbatim for the caller to interpret.
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	c := NewEnterpriseControlPlaneClient(srv.URL,
		WithTokenLoader(func() string { return "tok" }),
		WithRetryPolicy(RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond}),
		withSleeper(func(context.Context, time.Duration) error { return nil }),
	)
	c.token = "tok"

	resp, err := c.authorizedGet(context.Background(), "/x")
	if err != nil {
		t.Fatalf("authorizedGet: %v", err)
	}
	defer drainAndClose(resp)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
	// One initial 403 + one refreshed retry = 2 hits. The 403 is terminal after
	// the single refresh, so the backoff budget is not consumed.
	if hits != 2 {
		t.Errorf("server hits = %d, want 2 (initial + one refresh)", hits)
	}
}

func TestFetchTopologyWithRevision(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/whoami":
			w.WriteHeader(http.StatusOK)
		case "/api/v1/topology":
			_, _ = w.Write([]byte(`{"revision":"rev-42","peers":[{"clusterID":"c1","publicKey":"k1","endpoint":"e1"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := NewEnterpriseControlPlaneClient(srv.URL, WithTokenLoader(func() string { return "tok" }))
	peers, rev, err := c.FetchTopologyWithRevision(context.Background())
	if err != nil {
		t.Fatalf("FetchTopologyWithRevision: %v", err)
	}
	if rev != "rev-42" {
		t.Errorf("revision = %q, want rev-42", rev)
	}
	if len(peers) != 1 || peers[0].ClusterID != "c1" {
		t.Errorf("peers = %#v", peers)
	}
}
