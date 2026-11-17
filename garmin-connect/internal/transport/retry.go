// Package transport provides an http.RoundTripper that retries transient
// failures with exponential backoff and jitter.
package transport

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

// Retry is an http.RoundTripper that retries requests that fail with a
// network error, HTTP 429, or HTTP 5xx. Requests with a body are only retried
// when req.GetBody is set (http.NewRequestWithContext sets it for the common
// in-memory body types).
type Retry struct {
	// Base is the underlying transport. http.DefaultTransport if nil.
	Base http.RoundTripper
	// MaxRetries is the number of retries after the first attempt.
	MaxRetries int
	// MinBackoff is the delay before the first retry. Doubles on each retry.
	MinBackoff time.Duration
	// MaxBackoff caps the computed delay (and any Retry-After value).
	MaxBackoff time.Duration

	// sleep is replaceable in tests.
	sleep func(ctx context.Context, d time.Duration) error
}

// RoundTrip implements http.RoundTripper.
func (r *Retry) RoundTrip(req *http.Request) (*http.Response, error) {
	base := r.Base
	if base == nil {
		base = http.DefaultTransport
	}
	sleep := r.sleep
	if sleep == nil {
		sleep = sleepCtx
	}

	for attempt := 0; ; attempt++ {
		resp, err := base.RoundTrip(req)
		if attempt >= r.MaxRetries || !retryable(resp, err) {
			return resp, err
		}
		if req.Body != nil && req.GetBody == nil {
			return resp, err // body cannot be replayed
		}

		delay := r.backoff(attempt, resp)
		if resp != nil {
			// Drain so the connection can be reused, then discard.
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
			_ = resp.Body.Close()
		}
		if err := sleep(req.Context(), delay); err != nil {
			return nil, err
		}

		if req.GetBody != nil {
			body, gerr := req.GetBody()
			if gerr != nil {
				return nil, gerr
			}
			req = req.Clone(req.Context())
			req.Body = body
		}
	}
}

func retryable(resp *http.Response, err error) bool {
	if err != nil {
		return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
	}
	return resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
}

func (r *Retry) backoff(attempt int, resp *http.Response) time.Duration {
	maxD := r.MaxBackoff
	if maxD <= 0 {
		maxD = 30 * time.Second
	}
	if resp != nil {
		if d, ok := retryAfter(resp); ok {
			return min(d, maxD)
		}
	}
	minD := r.MinBackoff
	if minD <= 0 {
		minD = 200 * time.Millisecond
	}
	d := minD << attempt
	if d <= 0 || d > maxD { // overflow or cap
		d = maxD
	}
	// Full jitter in [d/2, d].
	half := d / 2
	return half + time.Duration(rand.Int64N(int64(half)+1))
}

func retryAfter(resp *http.Response) (time.Duration, bool) {
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(time.Until(t), 0), true
	}
	return 0, false
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
