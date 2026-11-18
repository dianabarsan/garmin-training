package garminconnect_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	garminconnect "github.com/dianabarsan/garmin-training/garmin-connect"
)

type failingToken struct{ err error }

func (f failingToken) AccessToken(context.Context) (string, error) { return "", f.err }

func TestUserSettingsStillWorks(t *testing.T) {
	t.Parallel()
	c, _ := serve(t, `{"a":1}`)
	if got, err := c.UserSettings(context.Background()); err != nil || string(got) != `{"a":1}` {
		t.Errorf("got %s err %v", got, err)
	}
}

func TestNoContentMeansNoData(t *testing.T) {
	t.Parallel()
	c := noContentClient(t)
	ctx := context.Background()

	if h, err := c.HRV(ctx, d2); err != nil || h.Summary != nil || len(h.Readings) != 0 {
		t.Errorf("HRV: %+v err %v", h, err)
	}
	if got, err := c.HRVRange(ctx, d1, d2); err != nil || len(got) != 0 {
		t.Errorf("HRVRange: %v err %v", got, err)
	}
	if s, err := c.Sleep(ctx, d2); err != nil || s.ID != 0 {
		t.Errorf("Sleep: %+v err %v", s, err)
	}
	if got, err := c.SleepRange(ctx, d1, d2); err != nil || len(got) != 0 {
		t.Errorf("SleepRange: %v err %v", got, err)
	}
	if h, err := c.HeartRate(ctx, d2); err != nil || len(h.Samples) != 0 {
		t.Errorf("HeartRate: %+v err %v", h, err)
	}
	if got, err := c.BodyBattery(ctx, d1, d2); err != nil || len(got) != 0 {
		t.Errorf("BodyBattery: %v err %v", got, err)
	}
	if got, err := c.DailySummaries(ctx, d1, d2); err != nil || len(got) != 0 {
		t.Errorf("DailySummaries: %v err %v", got, err)
	}
	if got, err := c.Steps(ctx, d1, d2); err != nil || len(got) != 0 {
		t.Errorf("Steps: %v err %v", got, err)
	}
	if got, err := c.VO2Max(ctx, d1, d2); err != nil || len(got) != 0 {
		t.Errorf("VO2Max: %v err %v", got, err)
	}
	if got, err := c.Activities(ctx, garminconnect.ActivitiesOptions{}); err != nil || len(got) != 0 {
		t.Errorf("Activities: %v err %v", got, err)
	}
	if s, err := c.ActivitySeries(ctx, 1); err != nil || len(s.Points) != 0 {
		t.Errorf("ActivitySeries: %+v err %v", s, err)
	}
}

func TestNoContent_DoesNotHideRealEmptyBodies(t *testing.T) {
	t.Parallel()
	// A 200 with no body is still a protocol problem, only 204 means "no data".
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	if _, err := c.UserSettings(context.Background()); err == nil {
		t.Error("200 with empty body must still be an error for decoded endpoints")
	}
}

// --- HRV range --------------------------------------------------------------

func TestUserSettings(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/userprofile-service/userprofile/settings" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("User-Agent"); got != "custom-ua" {
			t.Errorf("User-Agent = %q", got)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept = %q", got)
		}
		_, _ = w.Write([]byte(`{"userData":{"measurementSystem":"metric"}}`))
	}), garminconnect.WithUserAgent("custom-ua"))

	got, err := c.UserSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"userData":{"measurementSystem":"metric"}}`; string(got) != want {
		t.Errorf("settings = %s, want %s", got, want)
	}
}

func TestClient_ErrorMapping(t *testing.T) {
	t.Parallel()
	tests := []struct {
		status   int
		sentinel error
	}{
		{http.StatusUnauthorized, garminconnect.ErrUnauthorized},
		{http.StatusForbidden, garminconnect.ErrForbidden},
		{http.StatusNotFound, garminconnect.ErrNotFound},
		{http.StatusTooManyRequests, garminconnect.ErrRateLimited},
		{http.StatusBadRequest, nil},
	}
	for _, tc := range tests {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			t.Parallel()
			c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "nope", tc.status)
			}))
			_, err := c.UserSettings(context.Background())

			var apiErr *garminconnect.APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("err = %v, want *APIError", err)
			}
			if apiErr.StatusCode != tc.status {
				t.Errorf("StatusCode = %d, want %d", apiErr.StatusCode, tc.status)
			}
			if !strings.Contains(apiErr.Body, "nope") || !strings.Contains(err.Error(), "nope") {
				t.Errorf("body not surfaced: %v", err)
			}
			if tc.sentinel != nil && !errors.Is(err, tc.sentinel) {
				t.Errorf("errors.Is(%v, %v) = false", err, tc.sentinel)
			}
			if tc.sentinel == nil && (errors.Is(err, garminconnect.ErrUnauthorized) || errors.Is(err, garminconnect.ErrNotFound)) {
				t.Errorf("unexpected sentinel match for %v", err)
			}
		})
	}
}

func TestClient_RetriesTransientErrors(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"userId":"ok"}`))
	}))
	got, err := c.UserSettings(context.Background())
	if err != nil || string(got) != `{"userId":"ok"}` {
		t.Fatalf("got=%s err=%v", got, err)
	}
	if calls.Load() != 3 {
		t.Errorf("calls = %d, want 3", calls.Load())
	}
}

func TestClient_RetryDisabled(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}), garminconnect.WithRetry(0, 0, 0))
	if _, err := c.UserSettings(context.Background()); err == nil {
		t.Fatal("expected error")
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1", calls.Load())
	}
}

func TestClient_NoTokenSource(t *testing.T) {
	t.Parallel()
	c := garminconnect.New(garminconnect.WithBaseURL("http://127.0.0.1:0"))
	if _, err := c.UserSettings(context.Background()); !errors.Is(err, garminconnect.ErrNoTokenSource) {
		t.Errorf("err = %v, want ErrNoTokenSource", err)
	}
}

func TestClient_TokenSourceError(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	c := garminconnect.New(garminconnect.WithTokenSource(failingToken{boom}))
	if _, err := c.UserSettings(context.Background()); !errors.Is(err, boom) {
		t.Errorf("err = %v, want wrapped boom", err)
	}
}

func TestClient_BadJSON(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"malformed": `{not json`,
		"empty":     ``,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			if _, err := c.UserSettings(context.Background()); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestClient_ContextCancelled(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.UserSettings(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestClient_DoesNotMutateCallerHTTPClient(t *testing.T) {
	t.Parallel()
	hc := &http.Client{Timeout: time.Second}
	_ = garminconnect.New(garminconnect.WithHTTPClient(hc), garminconnect.WithTokenSource(staticToken("t")))
	if hc.Transport != nil {
		t.Error("caller's http.Client.Transport was modified")
	}
}

func TestClient_ResponseSizeIsLimited(t *testing.T) {
	t.Parallel()
	big := `{"pad":"` + strings.Repeat("x", 5000) + `"}`
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(big))
	}), garminconnect.WithMaxResponseBytes(1000))
	if _, err := c.UserSettings(context.Background()); !errors.Is(err, garminconnect.ErrResponseTooLarge) {
		t.Errorf("err = %v, want ErrResponseTooLarge", err)
	}
	// The same response within the limit is fine.
	c = newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(big))
	}), garminconnect.WithMaxResponseBytes(10000))
	if _, err := c.UserSettings(context.Background()); err != nil {
		t.Errorf("within the limit: %v", err)
	}
}

func TestClient_TrailingDataIsAnError(t *testing.T) {
	t.Parallel()
	c, _ := serve(t, `{"a":1} {"b":2}`)
	if _, err := c.UserSettings(context.Background()); err == nil {
		t.Error("a body with two JSON values must be rejected")
	}
}

// rotatingToken hands out tok-1 until it is invalidated, then tok-2.
type rotatingToken struct {
	mu          sync.Mutex
	invalidated []string
}

func (r *rotatingToken) AccessToken(context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.invalidated) == 0 {
		return "tok-1", nil
	}
	return "tok-2", nil
}

func (r *rotatingToken) Invalidate(tok string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.invalidated = append(r.invalidated, tok)
}

func TestClient_RevokedTokenIsReplacedOnce(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	accept := "Bearer tok-2"
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != accept {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	})
	ts := &rotatingToken{}
	c := newTestClient(t, h, garminconnect.WithTokenSource(ts))
	if _, err := c.UserSettings(context.Background()); err != nil {
		t.Fatalf("a revoked token should be replaced: %v", err)
	}
	if calls.Load() != 2 || len(ts.invalidated) != 1 || ts.invalidated[0] != "tok-1" {
		t.Errorf("calls=%d invalidated=%v", calls.Load(), ts.invalidated)
	}

	// Refused again, and the source keeps handing out the same token: the 401
	// is returned without resending a request that would fail the same way.
	calls.Store(0)
	accept = "never"
	if _, err := c.UserSettings(context.Background()); !errors.Is(err, garminconnect.ErrUnauthorized) || calls.Load() != 1 {
		t.Errorf("err=%v calls=%d, want ErrUnauthorized after 1", err, calls.Load())
	}

	// A token source that cannot invalidate gets the 401 straight away.
	var plain atomic.Int32
	c = newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		plain.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	if _, err := c.UserSettings(context.Background()); !errors.Is(err, garminconnect.ErrUnauthorized) || plain.Load() != 1 {
		t.Errorf("err=%v calls=%d, want ErrUnauthorized after 1", err, plain.Load())
	}
}

// Requests refused together with the same revoked token invalidate it once
// and all retry with its replacement.
func TestClient_ConcurrentRefusalsShareOneReplacement(t *testing.T) {
	t.Parallel()
	const n = 8
	var refused sync.WaitGroup
	refused.Add(n)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer tok-1" {
			refused.Done()
			refused.Wait() // every request holds tok-1 before any is refused
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	})
	ts := &rotatingToken{}
	c := newTestClient(t, h, garminconnect.WithTokenSource(ts))
	errs := make(chan error, n)
	for range n {
		go func() {
			_, err := c.UserSettings(context.Background())
			errs <- err
		}()
	}
	for range n {
		if err := <-errs; err != nil {
			t.Errorf("a request refused with the revoked token failed: %v", err)
		}
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if len(ts.invalidated) != 1 {
		t.Errorf("invalidated %v, want tok-1 once", ts.invalidated)
	}
}

func TestClient_RefusesPlainHTTPToARemoteHost(t *testing.T) {
	t.Parallel()
	c := garminconnect.New(garminconnect.WithBaseURL("http://example.com"), garminconnect.WithTokenSource(staticToken("tok")))
	if _, err := c.UserSettings(context.Background()); !errors.Is(err, garminconnect.ErrInsecureBaseURL) {
		t.Errorf("err = %v, want ErrInsecureBaseURL", err)
	}
}

func TestClient_MoreSentinels(t *testing.T) {
	t.Parallel()
	for code, want := range map[int]error{
		400: garminconnect.ErrBadRequest,
		500: garminconnect.ErrServer,
		503: garminconnect.ErrServer,
		501: garminconnect.ErrServer,
	} {
		c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
		}), garminconnect.WithRetry(0, 0, 0))
		_, err := c.UserSettings(context.Background())
		var apiErr *garminconnect.APIError
		if !errors.Is(err, want) || !errors.As(err, &apiErr) || apiErr.StatusCode != code {
			t.Errorf("%d: err = %v, want %v", code, err, want)
		}
	}
}

func TestClient_AttemptTimeout(t *testing.T) {
	t.Parallel()
	hang := func() http.Handler {
		var calls atomic.Int32
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 {
				select {
				case <-r.Context().Done():
				case <-time.After(5 * time.Second):
				}
				return
			}
			_, _ = w.Write([]byte(`{}`))
		})
	}
	c := newTestClient(t, hang(), garminconnect.WithAttemptTimeout(100*time.Millisecond))
	if _, err := c.UserSettings(context.Background()); err != nil {
		t.Errorf("a hung attempt should be retried: %v", err)
	}
	c = newTestClient(t, hang(), garminconnect.WithAttemptTimeout(100*time.Millisecond), garminconnect.WithRetry(0, 0, 0))
	if _, err := c.UserSettings(context.Background()); !errors.Is(err, garminconnect.ErrAttemptTimeout) {
		t.Errorf("without retries: err = %v, want ErrAttemptTimeout", err)
	}
}

func TestNew_IgnoresNilOptions(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}), nil)
	if _, err := c.UserSettings(context.Background()); err != nil {
		t.Error(err)
	}
	if _, err := (&garminconnect.Client{}).UserSettings(context.Background()); !errors.Is(err, garminconnect.ErrNoTokenSource) {
		t.Errorf("zero Client: err = %v, want ErrNoTokenSource", err)
	}
}

// stallingHandler sends the start of a body and then stops on its first call,
// and serves body in full afterwards.
func stallingHandler(body string, calls *atomic.Int32) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			_, _ = w.Write([]byte(body[:len(body)/2]))
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
			return
		}
		_, _ = w.Write([]byte(body))
	})
}

func TestClient_StalledBodyIsRequestedAgain(t *testing.T) {
	t.Parallel()
	const body = `[{"calendarDate":"2026-11-01","totalSteps":5,"stepGoal":7000}]`
	var calls atomic.Int32
	c := newTestClient(t, stallingHandler(body, &calls), garminconnect.WithAttemptTimeout(100*time.Millisecond))
	days, err := c.Steps(context.Background(), day("2026-11-01"), day("2026-11-01"))
	if err != nil || len(days) != 1 || days[0].TotalSteps != 5 || days[0].StepGoal != 7000 {
		t.Fatalf("days=%+v err=%v", days, err)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2", calls.Load())
	}

	calls.Store(0)
	c = newTestClient(t, stallingHandler(body, &calls), garminconnect.WithAttemptTimeout(100*time.Millisecond), garminconnect.WithRetry(0, 0, 0))
	if _, err := c.Steps(context.Background(), day("2026-11-01"), day("2026-11-01")); !errors.Is(err, garminconnect.ErrAttemptTimeout) {
		t.Errorf("without retries: err = %v, want ErrAttemptTimeout", err)
	}
}

func TestClient_StalledDownloadIsNotRequestedAgain(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	c := newTestClient(t, stallingHandler("0123456789abcdef", &calls), garminconnect.WithAttemptTimeout(100*time.Millisecond))
	var buf bytes.Buffer
	err := c.DownloadActivity(context.Background(), 1, &buf)
	if !errors.Is(err, garminconnect.ErrAttemptTimeout) || calls.Load() != 1 {
		t.Errorf("err = %v, calls = %d; want ErrAttemptTimeout after one call", err, calls.Load())
	}
}

// keepingToken can be invalidated but always hands out the same token.
type keepingToken struct{ invalidated atomic.Int32 }

func (k *keepingToken) AccessToken(context.Context) (string, error) { return "tok", nil }
func (k *keepingToken) Invalidate(string)                           { k.invalidated.Add(1) }

func TestClient_401IsNotResentWithTheSameToken(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	ts := &keepingToken{}
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "nope", http.StatusUnauthorized)
	}), garminconnect.WithTokenSource(ts))
	_, err := c.UserSettings(context.Background())
	var apiErr *garminconnect.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnauthorized || !strings.Contains(apiErr.Body, "nope") {
		t.Errorf("err = %v, want Garmin's own 401", err)
	}
	if calls.Load() != 1 || ts.invalidated.Load() != 1 {
		t.Errorf("calls=%d invalidated=%d, want 1 and 1", calls.Load(), ts.invalidated.Load())
	}
	// A second 401 within the minute does not invalidate again.
	_, _ = c.UserSettings(context.Background())
	if ts.invalidated.Load() != 1 {
		t.Errorf("invalidated %d times, want once per minute", ts.invalidated.Load())
	}
}
