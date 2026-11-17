package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// serve runs the HTTP server on ln until ctx is cancelled, then shuts it down.
func serve(ctx context.Context, ln net.Listener, handler http.Handler) error {
	// Every request context derives from connCtx, so cancelling it ends open
	// event streams, which would otherwise hold a graceful shutdown for its
	// full timeout.
	connCtx, cancelConns := context.WithCancel(context.Background())
	defer cancelConns()
	httpSrv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		MaxHeaderBytes:    64 << 10, // ample for any MCP client; the default is 1 MB
		IdleTimeout:       2 * time.Minute,
		BaseContext:       func(net.Listener) context.Context { return connCtx },
		// No ReadTimeout or WriteTimeout: MCP requests and responses can be
		// long-lived event streams. /auth sets its own body-read deadline.
	}
	errc := make(chan error, 1)
	go func() { errc <- httpSrv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	return shutdown(httpSrv, cancelConns, errc)
}

// shutdown stops the server gracefully: in-flight requests get a second to
// finish, then the remaining connections (typically idle MCP event streams)
// are cancelled, and anything still open after five seconds is closed.
func shutdown(srv *http.Server, cancelConns context.CancelFunc, serveErr <-chan error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Shutdown(ctx) }()
	var err error
	select {
	case err = <-done:
	case <-time.After(time.Second):
		cancelConns()
		err = <-done
	}
	if err != nil {
		_ = srv.Close()
	}
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// probeHealth GETs /healthz on a server listening at addr. A wildcard or empty
// host (":8808", "0.0.0.0:8808") is probed through loopback.
func probeHealth(ctx context.Context, addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("-addr: %w", err)
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(host, port)+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("unhealthy: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unhealthy: /healthz returned %d", resp.StatusCode)
	}
	return nil
}
