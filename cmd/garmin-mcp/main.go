// Command garmin-mcp serves your Garmin Connect data to AI agents over MCP
// (streamable HTTP).
//
//	garmin-mcp -set-key -key-store ~/.config/garmin-mcp/garmin.key   # reads the key from stdin
//	garmin-mcp -key-store ~/.config/garmin-mcp/garmin.key            # listens on 127.0.0.1:8808
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
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/dianabarsan/garmin-training/cmd/garmin-mcp/internal/mcpserver"
	garminconnect "github.com/dianabarsan/garmin-training/garmin-connect"
	"github.com/dianabarsan/garmin-training/garmin-connect/auth"
)

// version is set at build time: -ldflags "-X main.version=v1.2.3".
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Getenv, os.Stdin, os.Stdout, os.Stderr, nil); err != nil {
		fmt.Fprintln(os.Stderr, "garmin-mcp:", err)
		os.Exit(1)
	}
}

// keyCheckTimeout bounds checking a key with Garmin.
const keyCheckTimeout = 20 * time.Second

// keyPage is where Garmin Personal API keys are created.
const keyPage = "https://connect.garmin.com/app/settings/personalApiToken"

// run starts the server and blocks until ctx is cancelled or serving fails.
// ready, when non-nil, is called with the bound address once it is listening.
func run(ctx context.Context, args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer, ready func(addr string)) error {
	cfg, err := parseConfig(args, getenv, stderr)
	switch {
	case errors.Is(err, flag.ErrHelp):
		return nil
	case err != nil:
		return err
	case cfg.showVersion:
		fmt.Fprintln(stdout, "garmin-mcp", version)
		return nil
	case cfg.healthcheck:
		return probeHealth(ctx, cfg.addr)
	case cfg.setKey:
		return setKey(ctx, cfg, stdin, stdout)
	case cfg.keyStored:
		return keyStored(cfg)
	}

	log, err := newLogger(cfg.logLevel, stderr)
	if err != nil {
		return err
	}
	audit, closeAudit, err := newAuditLogger(cfg.auditLog, cfg.auditMaxMB<<20, log)
	if err != nil {
		return err
	}
	defer closeAudit()
	key, from, err := startKey(cfg, log)
	if err != nil {
		return err
	}
	tokens := newTokenSource(key, cfg.garminURL)
	srv, err := newServer(cfg, tokens, log, audit)
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", cfg.addr)
	if err != nil {
		return err
	}
	bound := ln.Addr().String()
	log.Info("listening", "addr", bound, "mcp", "/mcp", "key_from", from, "audit_log", cmp.Or(cfg.auditLog, "(in this log, audit=true)"), "token_required", cfg.token != "")
	// The key is checked once Garmin answers, without holding up the start: a
	// server whose network is not up yet still serves, and retries the key on
	// the first request.
	// The check is cancelled when run returns, so a serve failure does not
	// wait for a slow Garmin.
	checkCtx, cancelCheck := context.WithCancel(ctx)
	checked := make(chan struct{})
	go func() { defer close(checked); checkKey(checkCtx, tokens, log) }()
	defer func() { cancelCheck(); <-checked }()
	if ready != nil {
		ready(bound)
	}
	return serve(ctx, ln, srv.Handler())
}

func newTokenSource(key, garminURL string) *auth.TokenSource {
	var opts []auth.Option
	if garminURL != "" {
		opts = append(opts, auth.WithBaseURL(garminURL))
	}
	return auth.NewTokenSource(key, opts...)
}

// newServer wires the Garmin client and the MCP server.
func newServer(cfg config, tokens *auth.TokenSource, log, audit *slog.Logger) (*mcpserver.Server, error) {
	clientOpts := []garminconnect.Option{garminconnect.WithTokenSource(tokens), garminconnect.WithUserAgent("garmin-mcp/" + version)}
	if cfg.garminURL != "" {
		clientOpts = append(clientOpts, garminconnect.WithBaseURL(cfg.garminURL))
	}
	return mcpserver.New(mcpserver.Config{
		API:          garminconnect.New(clientOpts...),
		Token:        cfg.token,
		Addr:         cfg.addr,
		AllowedHosts: cfg.allowedHosts,
		Logger:       log,
		Audit:        audit,
		Version:      version,
	})
}

// errNoKey is returned when the server has no API key to start with.
var errNoKey = errors.New("no Garmin API key: set GARMIN_PERSONAL_API_KEY (or GARMIN_PERSONAL_API_KEY_FILE), or save one in the key store first with: garmin-mcp -set-key -key-store FILE")

// startKey returns the API key the server starts with, and where it came
// from: the environment (GARMIN_PERSONAL_API_KEY[_FILE]) wins over the key
// store. A server without a key could never answer, so that is an error.
func startKey(cfg config, log *slog.Logger) (key, from string, err error) {
	if cfg.envKey != "" {
		return cfg.envKey, "environment", nil
	}
	if cfg.keyStore == "" {
		return "", "", errNoKey
	}
	restrictFile(cfg.keyStore, "the key store", log)
	key, ok, err := keyFile(cfg.keyStore).load()
	if err != nil {
		return "", "", fmt.Errorf("-key-store: %w", err)
	}
	if !ok {
		return "", "", fmt.Errorf("-key-store %s holds no key: save one with -set-key, or set GARMIN_PERSONAL_API_KEY", cfg.keyStore)
	}
	return key, "key store", nil
}

// checkKey exchanges the key with Garmin once and logs the outcome, so a
// rejected key shows up in the log at startup rather than on the first tool
// call. A failure is not fatal: the client tries again on every request.
func checkKey(ctx context.Context, tokens *auth.TokenSource, log *slog.Logger) {
	checkCtx, cancel := context.WithTimeout(ctx, keyCheckTimeout)
	defer cancel()
	_, err := tokens.AccessToken(checkCtx)
	switch {
	case ctx.Err() != nil:
		// Shutting down: nothing to report.
	case err == nil:
		log.Info("Garmin accepted the API key")
	case errors.Is(err, auth.ErrInvalidKey):
		log.Warn("Garmin rejected the API key: create a new one at " + keyPage + ", give it to the server (-set-key, or GARMIN_PERSONAL_API_KEY) and restart it")
	default:
		log.Warn("could not check the API key with Garmin; it is tried again on the first request", "reason", reasonOf(err))
	}
}

// maxKeyInput bounds what -set-key reads; an API key is a couple of hundred
// bytes.
const maxKeyInput = 4 << 10

// setKey implements -set-key: it reads a key from stdin, checks it with
// Garmin and saves it to the key store. A key Garmin rejects is not saved.
func setKey(ctx context.Context, cfg config, stdin io.Reader, stdout io.Writer) error {
	if cfg.keyStore == "" {
		return errors.New("-set-key needs -key-store, the file to save the key in")
	}
	if stdin == nil {
		return errors.New("-set-key reads the key from standard input")
	}
	b, err := io.ReadAll(io.LimitReader(stdin, maxKeyInput+1))
	if err != nil {
		return fmt.Errorf("reading the key: %w", err)
	}
	if len(b) > maxKeyInput {
		return fmt.Errorf("the input is longer than %d bytes, too long to be an API key", maxKeyInput)
	}
	key := strings.TrimSpace(string(b))
	if key == "" {
		return errors.New("-set-key reads the key from standard input, and it was empty")
	}
	if strings.ContainsFunc(key, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return errors.New("the key contains spaces, line breaks or control characters; give the key alone, on one line")
	}
	ctx, cancel := context.WithTimeout(ctx, keyCheckTimeout)
	defer cancel()
	if _, err := newTokenSource(key, cfg.garminURL).AccessToken(ctx); err != nil {
		if errors.Is(err, auth.ErrInvalidKey) {
			return errors.New("the key was rejected by Garmin; create a new one at " + keyPage)
		}
		return fmt.Errorf("could not check the key with Garmin (%s); it was not saved", reasonOf(err))
	}
	if err := keyFile(cfg.keyStore).save(key); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "Garmin accepted the key; saved it to", cfg.keyStore)
	return nil
}

// keyStored implements -key-stored: it succeeds when the key store holds a
// key. It neither prints the key nor contacts Garmin.
func keyStored(cfg config) error {
	if cfg.keyStore == "" {
		return errors.New("-key-stored needs -key-store")
	}
	_, ok, err := keyFile(cfg.keyStore).load()
	switch {
	case err != nil:
		return err
	case !ok:
		return fmt.Errorf("no key is stored in %s", cfg.keyStore)
	}
	return nil
}

// reasonOf gives a log-safe reason for a failed key check. It maps known
// errors to fixed text instead of quoting the error, which could carry
// details such as file paths.
func reasonOf(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, auth.ErrInvalidKey):
		return "rejected by Garmin"
	case errors.Is(err, auth.ErrMalformedToken):
		return "unexpected answer from Garmin"
	case errors.Is(err, context.DeadlineExceeded):
		return "Garmin did not answer in time"
	case errors.As(err, new(*net.OpError)), errors.As(err, new(*net.DNSError)):
		return "could not reach Garmin"
	default:
		return "the check failed"
	}
}
