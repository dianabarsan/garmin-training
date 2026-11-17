package garminconnect_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
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
