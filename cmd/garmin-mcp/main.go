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
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/dianabarsan/garmin-training/cmd/garmin-mcp/internal/mcpserver"
	garminconnect "github.com/dianabarsan/garmin-training/garmin-connect"
	"github.com/dianabarsan/garmin-training/garmin-connect/auth"
)

// version is set at build time: -ldflags "-X main.version=v1.2.3".
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr, nil); err != nil {
		fmt.Fprintln(os.Stderr, "garmin-mcp:", err)
		os.Exit(1)
	}
}

// run starts the server and blocks until ctx is cancelled or serving fails.
// ready, when non-nil, is called with the bound address once it is listening.
func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer, ready func(addr string)) error {
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
		restrictFile(path, "the key store", log)
		credOpts = append(credOpts, mcpserver.WithKeyStore(mcpserver.NewFileKeyStore(path)))
	}
	creds := mcpserver.NewCredentials(credOpts...)

	clientOpts := []garminconnect.Option{garminconnect.WithTokenSource(creds), garminconnect.WithUserAgent("garmin-mcp/" + version)}
	if cfg.garminURL != "" {
		clientOpts = append(clientOpts, garminconnect.WithBaseURL(cfg.garminURL))
	}
	srv, err := mcpserver.New(mcpserver.Config{
		API:                 garminconnect.New(clientOpts...),
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
// when that endpoint is enabled, otherwise by restarting with a new key). A key
// that could not be checked (Garmin unreachable, or the key store failing) is
// used anyway and checked on the first request, so a network blip at startup
// does not leave the server without its key.
func loadInitialKey(ctx context.Context, creds *mcpserver.Credentials, envKey string, authEndpoint bool, log *slog.Logger) (supplied bool) {
	if envKey != "" {
		vctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		err := creds.Set(vctx, envKey)
		switch {
		case err == nil:
			log.Info("API key from the environment accepted")
		case errors.Is(err, auth.ErrInvalidKey):
			remedy := "provide a key at /auth"
			if !authEndpoint {
				remedy = "restart with a new key"
			}
			log.Warn("the API key from GARMIN_PERSONAL_API_KEY was not accepted; "+remedy, "reason", reasonOf(err))
		default:
			creds.UseUnverified(envKey)
			log.Warn("could not check the API key from GARMIN_PERSONAL_API_KEY; using it anyway, it is checked again on the first request", "reason", reasonOf(err))
		}
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
	case errors.Is(err, mcpserver.ErrKeyNotSaved):
		return "could not save it to the key store"
	case errors.Is(err, context.DeadlineExceeded):
		return "Garmin did not answer in time"
	default:
		return "could not reach Garmin"
	}
}
