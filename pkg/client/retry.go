package client

import (
	"context"
	"math/rand"
	"net/http"
	"time"
)

// RetryPolicy bounds how transient control-plane failures are retried. A
// "transient" failure is a transport error or a retryable HTTP status (429 or
// 5xx); 4xx other than 429 is treated as terminal - the request is malformed or
// the credential is bad — retrying won't help.
type RetryPolicy struct {
	// MaxAttempts is the total number of attempts, including the first. Values
	// < 1 are treated as 1, no retry.
	MaxAttempts int
	// BaseDelay is the first backoff step; each subsequent step doubles it.
	BaseDelay time.Duration
	// MaxDelay caps any single backoff step before jitter is applied.
	MaxDelay time.Duration
}

// DefaultRetryPolicy is a conservative policy suitable for a control-plane poll
// loop.  It yields a handful of attempts with capped exponential backoff.
var DefaultRetryPolicy = RetryPolicy{
	MaxAttempts: 4,
	BaseDelay:   200 * time.Millisecond,
	MaxDelay:    5 * time.Second,
}

// backoff returns the capped exponential delay for a zero-based attempt index,
// before jitter is applied: attempt 0 → BaseDelay, 1 → 2·BaseDelay, ... clamped
// to MaxDelay. It is a pure function so the schedule is unit-testable.
func (p RetryPolicy) backoff(attempt int) time.Duration {
	if p.BaseDelay <= 0 {
		return 0
	}
	d := p.BaseDelay
	for i := 0; i < attempt; i++ {
		d *= 2
		if p.MaxDelay > 0 && d >= p.MaxDelay {
			return p.MaxDelay
		}
		if d <= 0 { // overflow guard
			return p.MaxDelay
		}
	}
	if p.MaxDelay > 0 && d > p.MaxDelay {
		return p.MaxDelay
	}
	return d
}

// attempts normalizes MaxAttempts to a sane lower bound of 1.
func (p RetryPolicy) attempts() int {
	if p.MaxAttempts < 1 {
		return 1
	}
	return p.MaxAttempts
}

// isRetryableStatus reports whether an HTTP status warrants a retry. Rate
// limiting (429) and server-side failures (5xx) are transient; everything else
// is terminal for the purposes of the retry loop.
func isRetryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || (code >= 500 && code <= 599)
}

// fullJitter returns a random duration in [0, d], the "full jitter" strategy
// that avoids synchronized retry storms across a fleet of agents. It tolerates
// d <= 0 by returning 0.
func fullJitter(d time.Duration, rng *rand.Rand) time.Duration {
	if d <= 0 {
		return 0
	}
	if rng == nil {
		return d
	}
	return time.Duration(rng.Int63n(int64(d) + 1))
}

// sleepCtx waits for d or until ctx is cancelled, whichever comes first,
// returning ctx.Err() if it was cancelled. A non-positive d returns immediately
// (still honoring an already-cancelled context).
func sleepCtx(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
