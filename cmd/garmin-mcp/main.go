// Command garmin-mcp serves your Garmin Connect data to AI agents over MCP
// (streamable HTTP), and provides an endpoint for giving it your Garmin
// Personal API key.
//
//	garmin-mcp                       # listens on 127.0.0.1:8808
//	open http://127.0.0.1:8808/      # paste your API key
//	curl -d '{"api_key":"..."}' -H 'Content-Type: application/json' http://127.0.0.1:8808/auth
//
// MCP clients connect to http://127.0.0.1:8808/mcp.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	garmin "github.com/dianabarsan/garmin-training"
	"github.com/dianabarsan/garmin-training/internal/mcpserver"
)

// version is set at build time: -ldflags "-X main.version=v1.2.3".
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Getenv, os.Stderr, nil); err != nil {
		fmt.Fprintln(os.Stderr, "garmin-mcp:", err)
		os.Exit(1)
	}
}

// run starts the server and blocks until ctx is cancelled or serving fails.
// ready, when non-nil, is called with the bound address once it is listening.
func run(ctx context.Context, args []string, getenv func(string) string, stderr io.Writer, ready func(addr string)) error {
	// Secrets may come from the environment or, safer in containers, from a file
	// named by NAME_FILE (the Docker secrets convention).
	tokenDefault, err := secret(getenv, "GARMIN_MCP_TOKEN")
	if err != nil {
		return err
	}
	envKey, err := secret(getenv, "GARMIN_PERSONAL_API_KEY")
	if err != nil {
		return err
	}

	fs := flag.NewFlagSet("garmin-mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", "127.0.0.1:8808", "address to listen on; a non-loopback address requires -token")
	token := fs.String("token", tokenDefault, "require this bearer token on /mcp and /auth (env GARMIN_MCP_TOKEN or GARMIN_MCP_TOKEN_FILE)")
	allowedHosts := fs.String("allowed-host", getenv("GARMIN_MCP_ALLOWED_HOSTS"), "comma-separated public hostnames this server may be reached as, e.g. garmin.example.com; set it behind a reverse proxy (env GARMIN_MCP_ALLOWED_HOSTS)")
	healthcheck := fs.Bool("healthcheck", false, "probe a running server at -addr and exit 0 if it is healthy (for container health checks)")
	keyStore := fs.String("key-store", getenv("GARMIN_MCP_KEY_STORE"), "file in which to keep the API key between restarts, mode 0600 (env GARMIN_MCP_KEY_STORE); the key stays in memory only when unset")
	baseURL := fs.String("garmin-url", getenv("GARMIN_PERSONAL_API_BASE_URL"), "Garmin API base URL; for testing")
	level := fs.String("log-level", "info", "debug, info, warn or error")
	showVersion := fs.Bool("version", false, "print the version and exit")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: garmin-mcp [flags]\n\nThe Garmin Personal API key is read from GARMIN_PERSONAL_API_KEY at startup, or\nprovided later at http://<addr>/ or by POSTing {\"api_key\": \"...\"} to /auth.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *showVersion {
		fmt.Fprintln(stderr, "garmin-mcp", version)
		return nil
	}
	if *healthcheck {
		return probeHealth(ctx, *addr)
	}

	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(*level)); err != nil {
		return fmt.Errorf("invalid -log-level %q", *level)
	}
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: lvl}))

	credOpts := []mcpserver.CredentialsOption{}
	if *baseURL != "" {
		credOpts = append(credOpts, mcpserver.WithAuthBaseURL(*baseURL))
	}
	if *keyStore != "" {
		path, err := filepath.Abs(*keyStore)
		if err != nil {
			return fmt.Errorf("-key-store: %w", err)
		}
		credOpts = append(credOpts, mcpserver.WithKeyStore(mcpserver.NewFileKeyStore(path)))
	}
	creds := mcpserver.NewCredentials(credOpts...)

	clientOpts := []garmin.Option{garmin.WithTokenSource(creds), garmin.WithUserAgent("garmin-mcp/" + version)}
	if *baseURL != "" {
		clientOpts = append(clientOpts, garmin.WithBaseURL(*baseURL))
	}
	srv, err := mcpserver.New(mcpserver.Config{
		API:          garmin.New(clientOpts...),
		Credentials:  creds,
		Token:        *token,
		Addr:         *addr,
		AllowedHosts: splitList(*allowedHosts),
		Logger:       log,
		Version:      version,
	})
	if err != nil {
		return err
	}

	// Initial key: an explicit environment variable wins, otherwise the store.
	switch {
	case envKey != "":
		vctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := creds.Set(vctx, envKey)
		cancel()
		if err != nil {
			// Not fatal: the key can still be provided at /auth.
			log.Warn("the API key from GARMIN_PERSONAL_API_KEY was not accepted; provide a key at /auth", "reason", reasonOf(err))
		} else {
			log.Info("API key from the environment accepted")
		}
	default:
		if ok, err := creds.LoadStored(); err != nil {
			log.Warn("could not read the stored key", "err", err)
		} else if ok {
			log.Info("API key loaded from the key store")
		}
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	// Every request context derives from connCtx, so cancelling it ends open
	// event streams, which would otherwise hold a graceful shutdown for its
	// full timeout.
	connCtx, cancelConns := context.WithCancel(context.Background())
	defer cancelConns()
	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		BaseContext:       func(net.Listener) context.Context { return connCtx },
		// No WriteTimeout: MCP responses can be long-lived event streams.
	}
	bound := ln.Addr().String()
	log.Info("listening", "addr", bound, "mcp", "/mcp", "auth", "/auth", "token_required", *token != "", "key_loaded", creds.Status().Authenticated)
	if !creds.Status().Authenticated {
		log.Info("no API key yet: open the address in a browser, or POST {\"api_key\": \"...\"} to /auth")
	}
	if ready != nil {
		ready(bound)
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

// reasonOf gives a log-safe reason for a rejected key.
func reasonOf(err error) string {
	if err == nil {
		return ""
	}
	return strings.SplitN(err.Error(), ":", 2)[0]
}

// secret returns the value of NAME, or the trimmed contents of the file named
// by NAME_FILE. Setting both is an error, so the source is never ambiguous.
func secret(getenv func(string) string, name string) (string, error) {
	v, f := strings.TrimSpace(getenv(name)), strings.TrimSpace(getenv(name+"_FILE"))
	switch {
	case v != "" && f != "":
		return "", fmt.Errorf("set only one of %s and %s_FILE", name, name)
	case f != "":
		b, err := os.ReadFile(f)
		if err != nil {
			return "", fmt.Errorf("reading %s_FILE: %w", name, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return v, nil
}

// splitList splits a comma-separated list, dropping blanks.
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
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
