package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// config is everything run needs, gathered from flags and the environment.
type config struct {
	addr         string
	token        string
	allowedHosts []string
	keyStore     string
	auditLog     string
	garminURL    string
	logLevel     string
	envKey       string // initial API key from GARMIN_PERSONAL_API_KEY[_FILE]
	disableAuth  bool   // serve no /auth endpoint or key page
	healthcheck  bool
	showVersion  bool
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

	fs := flag.NewFlagSet("garmin-mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfg := config{envKey: envKey}
	var allowedHosts string
	fs.StringVar(&cfg.addr, "addr", "127.0.0.1:8808", "address to listen on; a non-loopback address requires -token")
	fs.StringVar(&cfg.token, "token", tokenDefault, "require this bearer token on /mcp and /auth (env GARMIN_MCP_TOKEN or GARMIN_MCP_TOKEN_FILE)")
	fs.StringVar(&allowedHosts, "allowed-host", getenv("GARMIN_MCP_ALLOWED_HOSTS"), "comma-separated public hostnames this server may be reached as, e.g. garmin.example.com; set it behind a reverse proxy (env GARMIN_MCP_ALLOWED_HOSTS)")
	fs.BoolVar(&cfg.disableAuth, "disable-auth-endpoint", disableDefault, "do not serve /auth or the key page (both answer 404), so the key can only come from GARMIN_PERSONAL_API_KEY[_FILE] or -key-store; use it when the server is reachable from outside (env GARMIN_MCP_DISABLE_AUTH_ENDPOINT)")
	fs.BoolVar(&cfg.healthcheck, "healthcheck", false, "probe a running server at -addr and exit 0 if it is healthy (for container health checks)")
	fs.StringVar(&cfg.keyStore, "key-store", getenv("GARMIN_MCP_KEY_STORE"), "file in which to keep the API key between restarts, mode 0600 (env GARMIN_MCP_KEY_STORE); the key stays in memory only when unset")
	fs.StringVar(&cfg.auditLog, "audit-log", getenv("GARMIN_MCP_AUDIT_LOG"), "append the audit trail (tool calls, refused requests, key changes) to this file as JSON lines, mode 0600; without it the records appear in the normal log tagged audit=true (env GARMIN_MCP_AUDIT_LOG)")
	fs.StringVar(&cfg.garminURL, "garmin-url", getenv("GARMIN_PERSONAL_API_BASE_URL"), "Garmin API base URL; for testing")
	fs.StringVar(&cfg.logLevel, "log-level", "info", "debug, info, warn or error")
	fs.BoolVar(&cfg.showVersion, "version", false, "print the version and exit")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: garmin-mcp [flags]\n\nThe Garmin Personal API key is read from GARMIN_PERSONAL_API_KEY at startup, or\nprovided later at http://<addr>/ or by POSTing {\"api_key\": \"...\"} to /auth\n(unless -disable-auth-endpoint is set).\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	cfg.allowedHosts = splitList(allowedHosts)
	// Fly.io tells every machine its app's name and serves the app at
	// <name>.fly.dev, so that hostname can be allowed without being typed twice.
	if app := strings.TrimSpace(getenv("FLY_APP_NAME")); app != "" {
		cfg.allowedHosts = append(cfg.allowedHosts, app+".fly.dev")
	}
	return cfg, nil
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
