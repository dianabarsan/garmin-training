package transport

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

func TestRetry_RetryAfterCappedByMaxBackoff(t *testing.T) {
	t.Parallel()
	rt := &Retry{MaxBackoff: time.Second}
	resp := &http.Response{Header: http.Header{"Retry-After": []string{"3600"}}}
	if got := rt.backoff(0, resp); got != time.Second {
		t.Errorf("backoff = %v, want 1s", got)
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
		d := rt.backoff(attempt, nil)
		if d <= 0 || d > time.Second {
			t.Fatalf("attempt %d: backoff %v out of (0, 1s]", attempt, d)
		}
	}
	if d := rt.backoff(0, nil); d < 50*time.Millisecond || d > 100*time.Millisecond {
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
			return nil, errors.New("connection reset")
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
