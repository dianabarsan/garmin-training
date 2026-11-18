package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func noSleep(context.Context, time.Duration) error { return nil }

func TestRetry_StatusCodes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		statuses  []int
		wantCalls int32
		wantFinal int
	}{
		{"success first try", []int{200}, 1, 200},
		{"retries 503 then ok", []int{503, 503, 200}, 3, 200},
		{"retries 429 then ok", []int{429, 200}, 2, 200},
		{"does not retry 400", []int{400}, 1, 400},
		{"does not retry 404", []int{404}, 1, 404},
		{"does not retry 501", []int{501}, 1, 501},
		{"does not retry 505", []int{505}, 1, 505},
		{"retries 502 and 504", []int{502, 504, 200}, 3, 200},
		{"gives up after max retries", []int{500, 500, 500, 500}, 3, 500}, // MaxRetries=2
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				i := int(calls.Add(1)) - 1
				w.WriteHeader(tc.statuses[min(i, len(tc.statuses)-1)])
			}))
			t.Cleanup(srv.Close)

			rt := &Retry{MaxRetries: 2, MinBackoff: time.Millisecond, sleep: noSleep}
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
			resp, err := rt.RoundTrip(req)
			if err != nil {
				t.Fatalf("RoundTrip: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.wantFinal {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.wantFinal)
			}
			if got := calls.Load(); got != tc.wantCalls {
				t.Errorf("calls = %d, want %d", got, tc.wantCalls)
			}
		})
	}
}

func TestRetry_ReplaysBody(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(srv.Close)

	rt := &Retry{MaxRetries: 1, MinBackoff: time.Millisecond, sleep: noSleep}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, strings.NewReader("payload"))
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(bodies) != 2 || bodies[0] != "payload" || bodies[1] != "payload" {
		t.Errorf("bodies = %q, want payload twice", bodies)
	}
}

func TestRetry_NoRetryWhenBodyNotReplayable(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	rt := &Retry{MaxRetries: 3, sleep: noSleep}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, io.NopCloser(strings.NewReader("x")))
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1", calls.Load())
	}
}

func TestRetry_HonorsRetryAfter(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
		}
	}))
	t.Cleanup(srv.Close)

	var slept time.Duration
	rt := &Retry{
		MaxRetries: 1,
		MaxBackoff: time.Minute,
		sleep: func(_ context.Context, d time.Duration) error {
			slept = d
			return nil
		},
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if slept != 7*time.Second {
		t.Errorf("slept %v, want 7s", slept)
	}
}

// A server that asks for a longer wait than MaxBackoff gets its response
// returned, rather than a retry that comes too early and is refused again.
func TestRetry_RetryAfterLongerThanMaxBackoffIsNotRetried(t *testing.T) {
	t.Parallel()
	rt := &Retry{MaxBackoff: time.Second}
	resp := &http.Response{Header: http.Header{"Retry-After": []string{"3600"}}}
	if _, ok := rt.backoff(0, resp); ok {
		t.Error("backoff should refuse a Retry-After beyond MaxBackoff")
	}

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)
	rt = &Retry{MaxRetries: 3, MaxBackoff: time.Second, sleep: noSleep}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	got, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	got.Body.Close()
	if got.StatusCode != http.StatusTooManyRequests || calls.Load() != 1 {
		t.Errorf("status %d after %d calls, want 429 after 1", got.StatusCode, calls.Load())
	}
}

func TestRetryAfter_HugeValuesDoNotOverflow(t *testing.T) {
	t.Parallel()
	for _, v := range []string{"10000000000", "9223372036854775807", "99999999999999999999"} {
		resp := &http.Response{Header: http.Header{"Retry-After": []string{v}}}
		d, ok := retryAfter(resp)
		if ok && (d <= 0 || d > maxRetryAfter) {
			t.Errorf("Retry-After %s: %v", v, d)
		}
		rt := &Retry{MaxBackoff: time.Second}
		if d, ok := rt.backoff(0, resp); ok && d < 0 {
			t.Errorf("Retry-After %s: negative backoff %v", v, d)
		}
	}
}

func TestRetryAfter_Parsing(t *testing.T) {
	t.Parallel()
	future := time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)
	tests := []struct {
		name   string
		header string
		wantOK bool
	}{
		{"empty", "", false},
		{"seconds", "5", true},
		{"negative", "-1", false},
		{"http date", future, true},
		{"garbage", "soon", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resp := &http.Response{Header: http.Header{}}
			if tc.header != "" {
				resp.Header.Set("Retry-After", tc.header)
			}
			if _, ok := retryAfter(resp); ok != tc.wantOK {
				t.Errorf("ok = %v, want %v", ok, tc.wantOK)
			}
		})
	}
}

func TestRetry_BackoffGrowsAndIsCapped(t *testing.T) {
	t.Parallel()
	rt := &Retry{MinBackoff: 100 * time.Millisecond, MaxBackoff: time.Second}
	for attempt := range 70 { // includes shift overflow territory
		d, _ := rt.backoff(attempt, nil)
		if d <= 0 || d > time.Second {
			t.Fatalf("attempt %d: backoff %v out of (0, 1s]", attempt, d)
		}
		if attempt >= 4 && d < 500*time.Millisecond { // capped: equal jitter keeps at least half
			t.Fatalf("attempt %d: backoff %v below the capped range [500ms, 1s]", attempt, d)
		}
	}
	if d, _ := rt.backoff(0, nil); d < 50*time.Millisecond || d > 100*time.Millisecond {
		t.Errorf("first backoff %v not in [50ms, 100ms]", d)
	}
}

type errTransport struct{ err error }

func (e errTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, e.err }

func TestRetry_NetworkErrors(t *testing.T) {
	t.Parallel()
	t.Run("retried then returned", func(t *testing.T) {
		t.Parallel()
		var calls int
		base := roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return nil, &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}
		})
		rt := &Retry{Base: base, MaxRetries: 2, sleep: noSleep}
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.invalid", nil)
		if _, err := rt.RoundTrip(req); err == nil {
			t.Fatal("expected error")
		}
		if calls != 3 {
			t.Errorf("calls = %d, want 3", calls)
		}
	})
	t.Run("permanent errors not retried", func(t *testing.T) {
		t.Parallel()
		for name, perm := range map[string]error{
			"unknown host":       &net.OpError{Op: "dial", Err: &net.DNSError{Err: "no such host", Name: "x.invalid", IsNotFound: true}},
			"certificate":        &tls.CertificateVerificationError{Err: errors.New("x509: unknown authority")},
			"unsupported scheme": errors.New(`unsupported protocol scheme "ftp"`),
		} {
			var calls int
			base := roundTripFunc(func(*http.Request) (*http.Response, error) { calls++; return nil, perm })
			rt := &Retry{Base: base, MaxRetries: 3, sleep: noSleep}
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.invalid", nil)
			if _, err := rt.RoundTrip(req); err == nil || calls != 1 {
				t.Errorf("%s: err=%v after %d calls, want an error after 1", name, err, calls)
			}
		}
	})
	t.Run("context errors not retried", func(t *testing.T) {
		t.Parallel()
		rt := &Retry{Base: errTransport{context.Canceled}, MaxRetries: 5, sleep: noSleep}
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.invalid", nil)
		if _, err := rt.RoundTrip(req); !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	})
}

func TestRetry_CancelledDuringSleep(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	// The first attempt must run with a live context, so cancel from inside the
	// sleep hook and let the real sleepCtx observe the cancellation.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rt := &Retry{MaxRetries: 3, MinBackoff: time.Hour, MaxBackoff: time.Hour}
	rt.sleep = func(ctx context.Context, d time.Duration) error {
		cancel()
		return sleepCtx(ctx, d)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if _, err := rt.RoundTrip(req); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRetry_TimeoutWaitingForHeadersIsRetried(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			select { // the first attempt hangs before answering
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(srv.Close)
	rt := &Retry{MaxRetries: 1, Timeout: 100 * time.Millisecond, sleep: noSleep}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "ok" || calls.Load() != 2 {
		t.Errorf("body %q after %d calls, want ok after 2", b, calls.Load())
	}
}

func TestRetry_TimeoutWithoutRetriesReportsAttemptTimeout(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)
	rt := &Retry{Timeout: 100 * time.Millisecond}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	if _, err := rt.RoundTrip(req); !errors.Is(err, ErrAttemptTimeout) {
		t.Errorf("err = %v, want ErrAttemptTimeout", err)
	}
}

// A body that stalls is cut off after Timeout without data, but a slow body
// that keeps delivering data is read to the end however long it takes.
func TestRetry_TimeoutAppliesToGapsInTheBody(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flush := http.NewResponseController(w).Flush
		gap := 40 * time.Millisecond
		if r.URL.Path == "/stall" {
			gap = 2 * time.Second
		}
		for range 6 {
			_, _ = io.WriteString(w, "chunk")
			_ = flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(gap):
			}
		}
	}))
	t.Cleanup(srv.Close)
	rt := &Retry{Timeout: 150 * time.Millisecond}

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/slow", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(resp.Body) // about 240 ms in total, longer than Timeout
	resp.Body.Close()
	if err != nil || len(b) != 30 {
		t.Errorf("slow body: %d bytes, err %v; want all 30", len(b), err)
	}

	req, _ = http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/stall", nil)
	resp, err = rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !errors.Is(err, ErrAttemptTimeout) {
		t.Errorf("stalled body: err = %v, want ErrAttemptTimeout", err)
	}
}
