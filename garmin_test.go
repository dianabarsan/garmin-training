package garmin_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	garmin "github.com/dianabarsan/garmin-training"
)

type staticToken string

func (s staticToken) AccessToken(context.Context) (string, error) { return string(s), nil }

type failingToken struct{ err error }

func (f failingToken) AccessToken(context.Context) (string, error) { return "", f.err }

func newTestClient(t *testing.T, h http.Handler, opts ...garmin.Option) *garmin.Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	base := []garmin.Option{
		garmin.WithBaseURL(srv.URL + "/"), // trailing slash must be tolerated
		garmin.WithTokenSource(staticToken("tok")),
		garmin.WithRetry(2, time.Millisecond, 5*time.Millisecond),
	}
	return garmin.New(append(base, opts...)...)
}

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
	}), garmin.WithUserAgent("custom-ua"))

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
		{http.StatusUnauthorized, garmin.ErrUnauthorized},
		{http.StatusForbidden, garmin.ErrForbidden},
		{http.StatusNotFound, garmin.ErrNotFound},
		{http.StatusTooManyRequests, garmin.ErrRateLimited},
		{http.StatusBadRequest, nil},
	}
	for _, tc := range tests {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			t.Parallel()
			c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "nope", tc.status)
			}))
			_, err := c.UserSettings(context.Background())

			var apiErr *garmin.APIError
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
			if tc.sentinel == nil && (errors.Is(err, garmin.ErrUnauthorized) || errors.Is(err, garmin.ErrNotFound)) {
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
	}), garmin.WithRetry(0, 0, 0))
	if _, err := c.UserSettings(context.Background()); err == nil {
		t.Fatal("expected error")
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1", calls.Load())
	}
}

func TestClient_NoTokenSource(t *testing.T) {
	t.Parallel()
	c := garmin.New(garmin.WithBaseURL("http://127.0.0.1:0"))
	if _, err := c.UserSettings(context.Background()); !errors.Is(err, garmin.ErrNoTokenSource) {
		t.Errorf("err = %v, want ErrNoTokenSource", err)
	}
}

func TestClient_TokenSourceError(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	c := garmin.New(garmin.WithTokenSource(failingToken{boom}))
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
	_ = garmin.New(garmin.WithHTTPClient(hc), garmin.WithTokenSource(staticToken("t")))
	if hc.Transport != nil {
		t.Error("caller's http.Client.Transport was modified")
	}
}
