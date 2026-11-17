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
	"cmp"
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
	"strconv"
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

// config is everything run needs, gathered from flags and the environment.
type config struct {
	addr         string
	token        string
	allowedHosts []string
	keyStore     string
	auditLog     string
	usersFile    string // multi-user mode: where registered users are kept
	adminToken   string // multi-user mode: protects /admin
	masterKey    string // multi-user mode: encrypts stored Garmin keys
	garminURL    string
	logLevel     string
	envKey       string // initial API key from GARMIN_PERSONAL_API_KEY[_FILE]
	disableAuth  bool   // serve no /auth endpoint or key page
	healthcheck  bool
	showVersion  bool
}

// run starts the server and blocks until ctx is cancelled or serving fails.
// ready, when non-nil, is called with the bound address once it is listening.
func run(ctx context.Context, args []string, getenv func(string) string, stderr io.Writer, ready func(addr string)) error {
	cfg, err := parseConfig(args, getenv, stderr)
	switch {
	case errors.Is(err, flag.ErrHelp):
		return nil
	case err != nil:
		return err
	case cfg.showVersion:
		fmt.Fprintln(stderr, "garmin-mcp", version)
		return nil
	case cfg.healthcheck:
		return probeHealth(ctx, cfg.addr)
	}
	if err := cfg.validate(); err != nil {
		return err
	}

	log, err := newLogger(cfg.logLevel, stderr)
	if err != nil {
		return err
	}
	audit, closeAudit, err := newAuditLogger(cfg.auditLog, log)
	if err != nil {
		return err
	}
	defer closeAudit()
	if cfg.usersFile != "" {
		return runMulti(ctx, cfg, log, audit, ready)
	}
	creds, srv, err := newServer(cfg, log, audit)
	if err != nil {
		return err
	}
	supplied := loadInitialKey(ctx, creds, cfg.envKey, !cfg.disableAuth, log)
	if cfg.disableAuth && !supplied {
		// With no endpoint to provide a key, a server that starts without one can
		// never work, so stop and say how to fix it.
		return errors.New("-disable-auth-endpoint needs a key from the start: set GARMIN_PERSONAL_API_KEY (or GARMIN_PERSONAL_API_KEY_FILE), or point -key-store at a file that already holds one")
	}

	ln, err := net.Listen("tcp", cfg.addr)
	if err != nil {
		return err
	}
	bound := ln.Addr().String()
	log.Info("listening", "addr", bound, "mcp", "/mcp", "auth_endpoint", !cfg.disableAuth, "audit_log", cmp.Or(cfg.auditLog, "(in this log, audit=true)"), "token_required", cfg.token != "", "key_loaded", creds.Status().Authenticated)
	if !creds.Status().Authenticated && !cfg.disableAuth {
		log.Info("no API key yet: open the address in a browser, or POST {\"api_key\": \"...\"} to /auth")
	}
	if ready != nil {
		ready(bound)
	}
	return serve(ctx, ln, srv.Handler())
}

// parseConfig reads flags and the environment. It returns flag.ErrHelp when
// help was requested. Secrets may come from the environment or, safer in
// containers, from a file named by NAME_FILE (the Docker secrets convention).
func parseConfig(args []string, getenv func(string) string, stderr io.Writer) (config, error) {
	tokenDefault, err := secret(getenv, "GARMIN_MCP_TOKEN")
	if err != nil {
		return config{}, err
	}
	envKey, err := secret(getenv, "GARMIN_PERSONAL_API_KEY")
	if err != nil {
		return config{}, err
	}
	disableDefault, err := boolEnv(getenv, "GARMIN_MCP_DISABLE_AUTH_ENDPOINT")
	if err != nil {
		return config{}, err
	}
	adminDefault, err := secret(getenv, "GARMIN_MCP_ADMIN_TOKEN")
	if err != nil {
		return config{}, err
	}
	masterKey, err := secret(getenv, "GARMIN_MCP_MASTER_KEY")
	if err != nil {
		return config{}, err
	}

	fs := flag.NewFlagSet("garmin-mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfg := config{envKey: envKey, masterKey: masterKey}
	var allowedHosts string
	fs.StringVar(&cfg.addr, "addr", "127.0.0.1:8808", "address to listen on; a non-loopback address requires -token")
	fs.StringVar(&cfg.token, "token", tokenDefault, "require this bearer token on /mcp and /auth (env GARMIN_MCP_TOKEN or GARMIN_MCP_TOKEN_FILE)")
	fs.StringVar(&allowedHosts, "allowed-host", getenv("GARMIN_MCP_ALLOWED_HOSTS"), "comma-separated public hostnames this server may be reached as, e.g. garmin.example.com; set it behind a reverse proxy (env GARMIN_MCP_ALLOWED_HOSTS)")
	fs.BoolVar(&cfg.disableAuth, "disable-auth-endpoint", disableDefault, "do not serve /auth or the key page (both answer 404), so the key can only come from GARMIN_PERSONAL_API_KEY[_FILE] or -key-store; use it when the server is reachable from outside (env GARMIN_MCP_DISABLE_AUTH_ENDPOINT)")
	fs.BoolVar(&cfg.healthcheck, "healthcheck", false, "probe a running server at -addr and exit 0 if it is healthy (for container health checks)")
	fs.StringVar(&cfg.keyStore, "key-store", getenv("GARMIN_MCP_KEY_STORE"), "file in which to keep the API key between restarts, mode 0600 (env GARMIN_MCP_KEY_STORE); the key stays in memory only when unset")
	fs.StringVar(&cfg.auditLog, "audit-log", getenv("GARMIN_MCP_AUDIT_LOG"), "append the audit trail (tool calls, refused requests, key changes) to this file as JSON lines, mode 0600; without it the records appear in the normal log tagged audit=true (env GARMIN_MCP_AUDIT_LOG)")
	fs.StringVar(&cfg.usersFile, "users-file", getenv("GARMIN_MCP_USERS_FILE"), "run in multi-user mode, keeping the registered users in this file (mode 0600); needs -admin-token and GARMIN_MCP_MASTER_KEY, and replaces -token, -key-store and /auth (env GARMIN_MCP_USERS_FILE)")
	fs.StringVar(&cfg.adminToken, "admin-token", adminDefault, "multi-user mode: bearer token, at least 16 characters, that protects /admin/users, where users are registered (env GARMIN_MCP_ADMIN_TOKEN or GARMIN_MCP_ADMIN_TOKEN_FILE)")
	fs.StringVar(&cfg.garminURL, "garmin-url", getenv("GARMIN_PERSONAL_API_BASE_URL"), "Garmin API base URL; for testing")
	fs.StringVar(&cfg.logLevel, "log-level", "info", "debug, info, warn or error")
	fs.BoolVar(&cfg.showVersion, "version", false, "print the version and exit")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: garmin-mcp [flags]\n\nThe Garmin Personal API key is read from GARMIN_PERSONAL_API_KEY at startup, or\nprovided later at http://<addr>/ or by POSTing {\"api_key\": \"...\"} to /auth\n(unless -disable-auth-endpoint is set).\n\nWith -users-file the server serves several people instead: each is registered\nthrough /admin/users with their own Garmin key and receives their own token.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	cfg.allowedHosts = splitList(allowedHosts)
	return cfg, nil
}

// validate rejects flag combinations that would silently do the wrong thing.
func (c config) validate() error {
	if c.usersFile == "" {
		switch {
		case c.adminToken != "":
			return errors.New("-admin-token only applies in multi-user mode (-users-file)")
		case c.masterKey != "":
			return errors.New("GARMIN_MCP_MASTER_KEY only applies in multi-user mode (-users-file)")
		}
		return nil
	}
	var clash []string
	if c.token != "" {
		clash = append(clash, "-token (use -admin-token and each user's own token)")
	}
	if c.keyStore != "" {
		clash = append(clash, "-key-store")
	}
	if c.disableAuth {
		clash = append(clash, "-disable-auth-endpoint")
	}
	if c.envKey != "" {
		clash = append(clash, "GARMIN_PERSONAL_API_KEY")
	}
	if len(clash) > 0 {
		return fmt.Errorf("-users-file (multi-user mode) cannot be combined with %s: in this mode every user's Garmin key is registered through /admin/users", strings.Join(clash, ", "))
	}
	if c.adminToken == "" {
		return errors.New("multi-user mode needs an admin token: set -admin-token, GARMIN_MCP_ADMIN_TOKEN or GARMIN_MCP_ADMIN_TOKEN_FILE")
	}
	return nil
}

// runMulti serves many users, each with their own Garmin key and token.
func runMulti(ctx context.Context, cfg config, log, audit *slog.Logger, ready func(addr string)) error {
	key, err := mcpserver.ParseMasterKey(cfg.masterKey)
	if err != nil {
		return fmt.Errorf("GARMIN_MCP_MASTER_KEY (or GARMIN_MCP_MASTER_KEY_FILE): %w", err)
	}
	store, err := mcpserver.OpenUserStore(cfg.usersFile, key)
	if err != nil {
		return err
	}
	srv, err := mcpserver.NewMulti(mcpserver.MultiConfig{
		Users:        store,
		AdminToken:   cfg.adminToken,
		AuthBaseURL:  cfg.garminURL,
		Addr:         cfg.addr,
		AllowedHosts: cfg.allowedHosts,
		Logger:       log,
		Audit:        audit,
		Version:      version,
		NewClient: func(ts garmin.TokenSource) *garmin.Client {
			opts := []garmin.Option{garmin.WithTokenSource(ts), garmin.WithUserAgent("garmin-mcp/" + version)}
			if cfg.garminURL != "" {
				opts = append(opts, garmin.WithBaseURL(cfg.garminURL))
			}
			return garmin.New(opts...)
		},
	})
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", cfg.addr)
	if err != nil {
		return err
	}
	bound := ln.Addr().String()
	log.Info("listening", "addr", bound, "mode", "multi-user", "users", srv.UserCount(), "mcp", "/mcp", "admin", "/admin/users",
		"audit_log", cmp.Or(cfg.auditLog, "(in this log, audit=true)"))
	if ready != nil {
		ready(bound)
	}
	return serve(ctx, ln, srv.Handler())
}

// newLogger builds the text logger for the given level name.
func newLogger(level string, w io.Writer) (*slog.Logger, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("invalid -log-level %q", level)
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: lvl})), nil
}

// newAuditLogger returns the logger for the audit trail and a function that
// closes it. With a path, records are appended to that file as JSON lines (the
// file is created mode 0600, its directory 0700). Without one they go to the
// normal log tagged audit=true.
func newAuditLogger(path string, ops *slog.Logger) (*slog.Logger, func(), error) {
	if path == "" {
		return ops.With("audit", true), func() {}, nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, nil, fmt.Errorf("-audit-log: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return nil, nil, fmt.Errorf("-audit-log: %w", err)
	}
	f, err := os.OpenFile(abs, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("-audit-log: %w", err)
	}
	logger := slog.New(slog.NewJSONHandler(f, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return logger, func() { _ = f.Close() }, nil
}

// newServer wires the credentials, the Garmin client and the MCP server.
func newServer(cfg config, log, audit *slog.Logger) (*mcpserver.Credentials, *mcpserver.Server, error) {
	credOpts := []mcpserver.CredentialsOption{}
	if cfg.garminURL != "" {
		credOpts = append(credOpts, mcpserver.WithAuthBaseURL(cfg.garminURL))
	}
	if cfg.keyStore != "" {
		path, err := filepath.Abs(cfg.keyStore)
		if err != nil {
			return nil, nil, fmt.Errorf("-key-store: %w", err)
		}
		credOpts = append(credOpts, mcpserver.WithKeyStore(mcpserver.NewFileKeyStore(path)))
	}
	creds := mcpserver.NewCredentials(credOpts...)

	clientOpts := []garmin.Option{garmin.WithTokenSource(creds), garmin.WithUserAgent("garmin-mcp/" + version)}
	if cfg.garminURL != "" {
		clientOpts = append(clientOpts, garmin.WithBaseURL(cfg.garminURL))
	}
	srv, err := mcpserver.New(mcpserver.Config{
		API:                 garmin.New(clientOpts...),
		Credentials:         creds,
		Token:               cfg.token,
		Addr:                cfg.addr,
		AllowedHosts:        cfg.allowedHosts,
		DisableAuthEndpoint: cfg.disableAuth,
		Logger:              log,
		Audit:               audit,
		Version:             version,
	})
	if err != nil {
		return nil, nil, err
	}
	return creds, srv, nil
}

// loadInitialKey activates the starting API key: one given explicitly (the
// environment or a secret file) wins, otherwise the key store's. It reports
// whether a key was supplied at all. A key Garmin rejects still counts as
// supplied: it is reported but not fatal, because it can be replaced (at /auth
// when that endpoint is enabled, otherwise by restarting with a new key).
func loadInitialKey(ctx context.Context, creds *mcpserver.Credentials, envKey string, authEndpoint bool, log *slog.Logger) (supplied bool) {
	if envKey != "" {
		vctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		if err := creds.Set(vctx, envKey); err != nil {
			remedy := "provide a key at /auth"
			if !authEndpoint {
				remedy = "restart with a new key"
			}
			log.Warn("the API key from GARMIN_PERSONAL_API_KEY was not accepted; "+remedy, "reason", reasonOf(err))
			return true
		}
		log.Info("API key from the environment accepted")
		return true
	}
	ok, err := creds.LoadStored()
	if err != nil {
		log.Warn("could not read the stored key", "err", err)
		return false
	}
	if ok {
		log.Info("API key loaded from the key store")
	}
	return ok
}

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
		IdleTimeout:       2 * time.Minute,
		BaseContext:       func(net.Listener) context.Context { return connCtx },
		// No WriteTimeout: MCP responses can be long-lived event streams.
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

// boolEnv reads an optional boolean environment variable; unset means false.
func boolEnv(getenv func(string) string, name string) (bool, error) {
	v := strings.TrimSpace(getenv(name))
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s must be true or false, got %q", name, v)
	}
	return b, nil
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
