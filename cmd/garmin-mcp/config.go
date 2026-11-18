package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// minTokenLen is the shortest bearer token accepted. A short token can be
// guessed, and nothing throttles attempts.
const minTokenLen = 16

// maxAuditMaxMB bounds -audit-log-max-mb (1 TiB), so converting it to bytes
// cannot overflow.
const maxAuditMaxMB = 1 << 20

// config is everything run needs, gathered from flags and the environment.
type config struct {
	addr         string
	token        string
	allowedHosts []string
	keyStore     string
	auditLog     string
	auditMaxMB   int64 // rotate the audit file past this size; 0 = never
	garminURL    string
	logLevel     string
	envKey       string // API key from GARMIN_PERSONAL_API_KEY[_FILE]
	setKey       bool   // read a key from stdin, check it and save it to keyStore
	keyStored    bool   // report whether keyStore holds a key
	healthcheck  bool
	showVersion  bool
}

// parseConfig reads flags and the environment. It returns flag.ErrHelp when
// help was requested. Secrets may come from the environment or, safer in
// containers, from a file named by NAME_FILE (the Docker secrets convention).
func parseConfig(args []string, getenv func(string) string, stderr io.Writer) (config, error) {
	token, err := secret(getenv, "GARMIN_MCP_TOKEN")
	if err != nil {
		return config{}, err
	}
	envKey, err := secret(getenv, "GARMIN_PERSONAL_API_KEY")
	if err != nil {
		return config{}, err
	}

	fs := flag.NewFlagSet("garmin-mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfg := config{envKey: envKey, token: token}
	var allowedHosts string
	fs.StringVar(&cfg.addr, "addr", "127.0.0.1:8808", "address to listen on; a non-loopback address requires a token (GARMIN_MCP_TOKEN)")
	fs.StringVar(&allowedHosts, "allowed-host", getenv("GARMIN_MCP_ALLOWED_HOSTS"), "comma-separated public hostnames this server may be reached as, e.g. garmin.example.com; set it behind a reverse proxy. On Fly.io, <FLY_APP_NAME>.fly.dev is added automatically (env GARMIN_MCP_ALLOWED_HOSTS)")
	fs.BoolVar(&cfg.setKey, "set-key", false, "read a Garmin API key from standard input, check it with Garmin, save it to -key-store and exit")
	fs.BoolVar(&cfg.keyStored, "key-stored", false, "exit 0 if -key-store holds a key, 1 if not; the key is not checked with Garmin")
	fs.BoolVar(&cfg.healthcheck, "healthcheck", false, "probe a running server at -addr and exit 0 if it is healthy (for container health checks)")
	fs.StringVar(&cfg.keyStore, "key-store", getenv("GARMIN_MCP_KEY_STORE"), "file holding the API key, mode 0600, written by -set-key; GARMIN_PERSONAL_API_KEY takes precedence (env GARMIN_MCP_KEY_STORE)")
	fs.StringVar(&cfg.auditLog, "audit-log", getenv("GARMIN_MCP_AUDIT_LOG"), "append the audit trail (tool calls and refused requests) to this file as JSON lines, mode 0600; without it the records appear in the normal log tagged audit=true (env GARMIN_MCP_AUDIT_LOG)")
	fs.Int64Var(&cfg.auditMaxMB, "audit-log-max-mb", 10, "when the audit file grows past this many megabytes it is renamed to <file>.1 (replacing an older one) and a new file is started; 0 disables the limit")
	fs.StringVar(&cfg.garminURL, "garmin-url", getenv("GARMIN_PERSONAL_API_BASE_URL"), "Garmin API base URL; for testing")
	fs.StringVar(&cfg.logLevel, "log-level", "info", "debug, info, warn or error")
	fs.BoolVar(&cfg.showVersion, "version", false, "print the version and exit")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: garmin-mcp [flags]\n\nThe Garmin Personal API key comes from GARMIN_PERSONAL_API_KEY (or\nGARMIN_PERSONAL_API_KEY_FILE), or from the -key-store file, where\n  garmin-mcp -set-key -key-store FILE\nsaves a key read from standard input.\n\nClients must send Authorization: Bearer <token> on /mcp when\nGARMIN_MCP_TOKEN (or GARMIN_MCP_TOKEN_FILE) is set; at least %d characters.\nIt is required on a non-loopback -addr.\n\nFlags:\n", minTokenLen)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	modes := 0
	for _, on := range []bool{cfg.showVersion, cfg.healthcheck, cfg.setKey, cfg.keyStored} {
		if on {
			modes++
		}
	}
	if modes > 1 {
		return config{}, errors.New("use only one of -version, -healthcheck, -set-key and -key-stored")
	}
	if cfg.token != "" && len(cfg.token) < minTokenLen {
		return config{}, fmt.Errorf("the token must be at least %d characters long (generate one with: openssl rand -hex 32)", minTokenLen)
	}
	if cfg.auditMaxMB < 0 || cfg.auditMaxMB > maxAuditMaxMB {
		return config{}, fmt.Errorf("-audit-log-max-mb must be between 0 and %d", maxAuditMaxMB)
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
// by NAME_FILE. Setting both is an error, so the source is never ambiguous. An
// empty file is an error too: a failed secret mount must not silently turn
// into "no token", which would leave the server open.
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
		fromFile := strings.TrimSpace(string(b))
		if fromFile == "" {
			return "", fmt.Errorf("%s_FILE names an empty file (%s)", name, f)
		}
		return fromFile, nil
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
