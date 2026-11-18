// Package transport provides an http.RoundTripper that retries transient
// failures with exponential backoff and jitter, and bounds each attempt.
package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ErrAttemptTimeout is returned (wrapped) when a server took longer than
// Retry.Timeout to send response headers, or stopped sending the body for that
// long. Waiting for headers is retried like any other transient failure. A
// stalled body is not: by then the response belongs to the caller, so the
// error comes from its Read, and retrying is up to the caller.
var ErrAttemptTimeout = errors.New("transport: no response from the server in time")

// maxRetryAfter bounds a Retry-After value before it is converted to a
// duration, so an absurd header cannot overflow.
const maxRetryAfter = 24 * time.Hour

// Retry is an http.RoundTripper that retries requests that fail transiently: a
// timeout, a dropped or refused connection, HTTP 429, or a 5xx status other
// than 501 and 505. Permanent failures (an unknown host, a certificate error,
// an unsupported scheme) are returned at once.
//
// Retrying resends the request, so use it only for idempotent requests.
// Requests with a body are only retried when req.GetBody is set
// (http.NewRequestWithContext sets it for the common in-memory body types).
type Retry struct {
	// Base is the underlying transport. http.DefaultTransport if nil.
	Base http.RoundTripper
	// MaxRetries is the number of retries after the first attempt.
	MaxRetries int
	// MinBackoff is the delay before the first retry. Doubles on each retry.
	MinBackoff time.Duration
	// MaxBackoff caps the computed delay. When the server asks, through
	// Retry-After, for a longer wait than this, the response is returned
	// instead of retrying early.
	MaxBackoff time.Duration
	// Timeout bounds each attempt: how long to wait for the response headers,
	// and then how long a read of the body may go without receiving data (see
	// ErrAttemptTimeout for which of these is retried). It does not limit the
	// total time, so a large download on a working connection is never cut
	// off; bound that with the request's context. Zero disables it.
	Timeout time.Duration

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
		resp, err := r.attempt(base, req)
		if attempt >= r.MaxRetries || !retryable(resp, err) {
			return resp, err
		}
		if req.Body != nil && req.GetBody == nil {
			return resp, err // body cannot be replayed
		}
		delay, ok := r.backoff(attempt, resp)
		if !ok {
			return resp, err // the server asked for a longer wait than allowed
		}
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

// attempt sends req once, enforcing r.Timeout on the headers and on gaps in
// the body.
func (r *Retry) attempt(base http.RoundTripper, req *http.Request) (*http.Response, error) {
	if r.Timeout <= 0 {
		return base.RoundTrip(req)
	}
	ctx, cancel := context.WithCancelCause(req.Context())
	timer := time.AfterFunc(r.Timeout, func() { cancel(ErrAttemptTimeout) })
	resp, err := base.RoundTrip(req.WithContext(ctx))
	if err != nil {
		timer.Stop()
		if errors.Is(context.Cause(ctx), ErrAttemptTimeout) {
			err = fmt.Errorf("%w (waited %v for headers): %w", ErrAttemptTimeout, r.Timeout, err)
		}
		cancel(nil)
		return nil, err
	}
	// From here on the timer measures gaps between reads of the body.
	timer.Reset(r.Timeout)
	resp.Body = &watchedBody{rc: resp.Body, ctx: ctx, cancel: cancel, timer: timer, d: r.Timeout}
	return resp, nil
}

// watchedBody cancels the request when a read gets no data for d, and
// releases the request's context when closed.
type watchedBody struct {
	rc     io.ReadCloser
	ctx    context.Context
	cancel context.CancelCauseFunc
	timer  *time.Timer
	d      time.Duration
}

func (b *watchedBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && errors.Is(context.Cause(b.ctx), ErrAttemptTimeout) {
		err = fmt.Errorf("%w (no data for %v): %w", ErrAttemptTimeout, b.d, err)
	} else if n > 0 {
		b.timer.Reset(b.d)
	}
	return n, err
}

func (b *watchedBody) Close() error {
	b.timer.Stop()
	err := b.rc.Close()
	b.cancel(nil)
	return err
}

func retryable(resp *http.Response, err error) bool {
	if err != nil {
		return retryableErr(err)
	}
	c := resp.StatusCode
	return c == http.StatusTooManyRequests ||
		(c >= 500 && c != http.StatusNotImplemented && c != http.StatusHTTPVersionNotSupported)
}

// retryableErr reports whether a transport error is worth another attempt.
func retryableErr(err error) bool {
	if errors.Is(err, ErrAttemptTimeout) {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false // the caller gave up
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return dnsErr.IsTimeout || dnsErr.IsTemporary // not "no such host"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.EPIPE)
}

// backoff returns the delay before the next attempt. ok is false when the
// server's Retry-After asks for longer than MaxBackoff.
func (r *Retry) backoff(attempt int, resp *http.Response) (d time.Duration, ok bool) {
	maxD := r.MaxBackoff
	if maxD <= 0 {
		maxD = 30 * time.Second
	}
	if resp != nil {
		if d, ok := retryAfter(resp); ok {
			return d, d <= maxD
		}
	}
	minD := r.MinBackoff
	if minD <= 0 {
		minD = 200 * time.Millisecond
	}
	// minD doubled per attempt, capped at maxD; compared before shifting so a
	// large attempt count cannot overflow.
	d = maxD
	if attempt < 63 && minD <= maxD>>attempt {
		d = minD << attempt
	}
	// Equal jitter: a random delay in [d/2, d].
	half := d / 2
	return half + time.Duration(rand.Int64N(int64(half)+1)), true
}

// retryAfter parses a Retry-After header (seconds or an HTTP date), capped at
// maxRetryAfter.
func retryAfter(resp *http.Response) (time.Duration, bool) {
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil && secs >= 0 {
		if secs > int64(maxRetryAfter/time.Second) {
			return maxRetryAfter, true
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		return min(max(time.Until(t), 0), maxRetryAfter), true
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

// ErrInsecureURL is returned for a plain-http base URL that is not on the
// loopback interface: requests carry credentials, which must not travel
// unencrypted.
var ErrInsecureURL = errors.New("base URL must use https (plain http is allowed only for localhost)")

// CheckBaseURL accepts an https URL, or an http URL whose host is loopback
// (localhost, 127.0.0.0/8, ::1), as used by test servers.
func CheckBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid base URL: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if strings.EqualFold(host, "localhost") {
			return nil
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return nil
		}
		return fmt.Errorf("%w: %s", ErrInsecureURL, u.Redacted())
	}
	return fmt.Errorf("invalid base URL %q: scheme must be https", u.Redacted())
}
