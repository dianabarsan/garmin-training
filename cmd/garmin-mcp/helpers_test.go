package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	tokenPath = "/personalapitoken-service/personal-api/personal-api-key/token"
	goodKey   = "cmd-test-key-ABCDEF123456"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func fakeGarmin(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var exchanges atomic.Int32
	enc := base64.RawURLEncoding.EncodeToString
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case tokenPath:
			exchanges.Add(1)
			if r.Header.Get("Authorization") != "ApiKey "+goodKey {
				http.Error(w, "no", http.StatusUnauthorized)
				return
			}
			jwt := enc([]byte(`{"alg":"none"}`)) + "." + enc(fmt.Appendf(nil, `{"exp":%d}`, time.Now().Add(24*time.Hour).Unix())) + ".s"
			_, _ = w.Write([]byte(jwt))
		case "/usersummary-service/stats/steps/daily/" + time.Now().AddDate(0, 0, -6).Format(time.DateOnly) + "/" + time.Now().Format(time.DateOnly):
			_, _ = w.Write([]byte(`[{"calendarDate":"2026-11-16","totalSteps":4321,"stepGoal":8000,"totalDistance":3000}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &exchanges
}

// start runs the server on a random port and returns its base URL, its log
// output, and a stop function that waits for a clean shutdown.
func start(t *testing.T, env map[string]string, args ...string) (base string, logs *syncBuf, stop func() error) {
	t.Helper()
	logs = &syncBuf{}
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	done := make(chan error, 1)
	all := append([]string{"-addr", "127.0.0.1:0"}, args...)
	go func() {
		done <- run(ctx, all, func(k string) string { return env[k] }, io.Discard, logs, func(a string) { ready <- a })
	}()
	select {
	case addr := <-ready:
		base = "http://" + addr
	case err := <-done:
		cancel()
		t.Fatalf("run exited early: %v\n%s", err, logs.String())
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("server did not start")
	}
	var once sync.Once
	var result error
	stop = func() error {
		once.Do(func() {
			cancel()
			select {
			case result = <-done:
			case <-time.After(10 * time.Second):
				result = fmt.Errorf("server did not shut down")
			}
		})
		return result
	}
	t.Cleanup(func() { _ = stop() })
	return base, logs, stop
}

func get(t *testing.T, url string, hdr map[string]string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func post(t *testing.T, url, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}
