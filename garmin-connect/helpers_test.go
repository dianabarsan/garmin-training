package garminconnect_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	garminconnect "github.com/dianabarsan/garmin-training/garmin-connect"
)

var (
	d1 = time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	d2 = time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
)

// serve returns a client whose server replies with body to every request and
// records the last request.
func serve(t *testing.T, body string) (*garminconnect.Client, *http.Request) {
	t.Helper()
	last := new(http.Request)
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*last = *r.Clone(context.Background())
		_, _ = w.Write([]byte(body))
	}))
	return c, last
}

func noContentClient(t *testing.T) *garminconnect.Client {
	t.Helper()
	return newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
}

// windowRecorder answers every request with one entry per window, recording
// the window each request asked for.
type windowRecorder struct {
	windows [][2]string
}

func (r *windowRecorder) handler(startKey, endKey string, body func(start string) string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var s, e string
		if startKey != "" {
			s, e = req.URL.Query().Get(startKey), req.URL.Query().Get(endKey)
		} else { // dates embedded in the path: .../daily/{start}/{end}
			parts := strings.Split(strings.TrimRight(req.URL.Path, "/"), "/")
			s, e = parts[len(parts)-2], parts[len(parts)-1]
		}
		r.windows = append(r.windows, [2]string{s, e})
		_, _ = w.Write([]byte(body(s)))
	})
}

func (r *windowRecorder) summary() string {
	parts := make([]string, len(r.windows))
	for i, w := range r.windows {
		parts[i] = w[0] + ".." + w[1]
	}
	return strings.Join(parts, " ")
}

func day(s string) time.Time {
	t, _ := time.Parse(time.DateOnly, s)
	return t
}

type staticToken string

func (s staticToken) AccessToken(context.Context) (string, error) { return string(s), nil }

func newTestClient(t *testing.T, h http.Handler, opts ...garminconnect.Option) *garminconnect.Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	base := []garminconnect.Option{
		garminconnect.WithBaseURL(srv.URL + "/"), // trailing slash must be tolerated
		garminconnect.WithTokenSource(staticToken("tok")),
		garminconnect.WithRetry(2, time.Millisecond, 5*time.Millisecond),
	}
	return garminconnect.New(append(base, opts...)...)
}
