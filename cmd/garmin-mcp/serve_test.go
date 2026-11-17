package main

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"
)

// A response that never ends on its own (an idle MCP event stream) must not
// hold up shutdown: serve cancels it after the grace period.
func TestServe_ShutdownEndsOpenStreams(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		http.NewResponseController(w).Flush()
		close(entered)
		<-r.Context().Done() // a stream that only ends when cancelled
	})
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- serve(ctx, ln, handler) }()

	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		resp, err := http.Get("http://" + ln.Addr().String() + "/stream")
		if err == nil {
			_, _ = resp.Body.Read(make([]byte, 1)) // blocks until the stream ends
			resp.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream did not start")
	}

	start := time.Now()
	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("serve: %v", err)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("shutdown did not end the open stream")
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("shutdown took %v; open streams should be cancelled after about a second", took)
	}
	select {
	case <-clientDone:
	case <-time.After(2 * time.Second):
		t.Error("the client's stream was not closed")
	}
}
