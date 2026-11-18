package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSecret(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(p, []byte(content), 0o400); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRun_TokenFromEnvironmentProtectsEndpoints(t *testing.T) {
	garminSrv, _ := fakeGarmin(t)
	base, _, _ := start(t, map[string]string{"GARMIN_MCP_TOKEN": "env-token-0123456789"}, "-garmin-url", garminSrv.URL)
	if code, _ := get(t, base+"/mcp", nil); code != 401 {
		t.Errorf("without token: %d", code)
	}
	if code, _ := get(t, base+"/mcp", map[string]string{"Authorization": "Bearer env-token-0123456789"}); code == 401 {
		t.Errorf("with token: %d", code)
	}
}

func TestRun_SecretsFromFiles(t *testing.T) {
	garminSrv, _ := fakeGarmin(t)
	env := map[string]string{
		"GARMIN_MCP_TOKEN_FILE":        writeSecret(t, "file-token-0123456789\n"), // a trailing newline is normal in secret files
		"GARMIN_PERSONAL_API_KEY_FILE": writeSecret(t, goodKey+"\n"),
	}
	base, logs, _ := start(t, env, "-garmin-url", garminSrv.URL)
	if code, _ := get(t, base+"/mcp", nil); code != 401 {
		t.Errorf("the token from a file must be enforced: %d", code)
	}
	if code, _ := get(t, base+"/mcp", map[string]string{"Authorization": "Bearer file-token-0123456789"}); code == 401 {
		t.Errorf("the token from a file was refused: %d", code)
	}
	waitForLog(t, logs, "Garmin accepted the API key") // the key from its file
	if strings.Contains(logs.String(), goodKey) || strings.Contains(logs.String(), "file-token") {
		t.Errorf("a secret leaked into the log:\n%s", logs.String())
	}
}

func TestRun_SecretSourceErrors(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"token set twice":    {"GARMIN_MCP_TOKEN": "a", "GARMIN_MCP_TOKEN_FILE": writeSecret(t, "b")},
		"key set twice":      {"GARMIN_PERSONAL_API_KEY": "a", "GARMIN_PERSONAL_API_KEY_FILE": writeSecret(t, "b")},
		"token file missing": {"GARMIN_MCP_TOKEN_FILE": filepath.Join(t.TempDir(), "absent")},
		"key file missing":   {"GARMIN_PERSONAL_API_KEY_FILE": filepath.Join(t.TempDir(), "absent")},
		// An empty secret file (a failed mount) must not mean "no token".
		"token file empty": {"GARMIN_MCP_TOKEN_FILE": writeSecret(t, " \n")},
		"key file empty":   {"GARMIN_PERSONAL_API_KEY_FILE": writeSecret(t, "")},
		"token too short":  {"GARMIN_MCP_TOKEN": "short"},
	} {
		err := run(context.Background(), []string{"-addr", "127.0.0.1:0"}, func(k string) string { return env[k] }, nil, io.Discard, io.Discard, nil)
		if err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestRun_AllowedHostsForReverseProxies(t *testing.T) {
	garminSrv, _ := fakeGarmin(t)
	for name, tc := range map[string]struct {
		env  map[string]string
		args []string
	}{
		"flag":        {nil, []string{"-allowed-host", "garmin.example.com, other.example.org"}},
		"environment": {map[string]string{"GARMIN_MCP_ALLOWED_HOSTS": "garmin.example.com,other.example.org"}, nil},
	} {
		base, _, _ := start(t, tc.env, append([]string{"-garmin-url", garminSrv.URL}, tc.args...)...)
		for host, want := range map[string]int{
			"garmin.example.com":              404, // allowed: "/" serves nothing
			"GARMIN.example.com:443":          404, // case and port are ignored
			"other.example.org":               404,
			"localhost":                       404, // local administration and health checks
			"evil.example":                    403,
			"garmin.example.com.evil.example": 403,
		} {
			if code, _ := get(t, base+"/", map[string]string{"Host": host}); code != want {
				t.Errorf("%s: Host %q -> %d, want %d", name, host, code, want)
			}
		}
	}
}

func TestRun_AllowedHostsRejectsURLs(t *testing.T) {
	err := run(context.Background(), []string{"-addr", "127.0.0.1:0", "-allowed-host", "https://garmin.example.com"},
		keyEnv, nil, io.Discard, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "hostname") {
		t.Errorf("err = %v, want a complaint about the URL form", err)
	}
}

// Fly.io names the app in FLY_APP_NAME and serves it at <name>.fly.dev, so that
// hostname is allowed without being configured twice.
func TestRun_FlyAppNameAllowsItsHostname(t *testing.T) {
	garminSrv, _ := fakeGarmin(t)
	probe := func(env map[string]string, host string) int {
		base, _, stop := start(t, env, "-garmin-url", garminSrv.URL)
		defer stop()
		code, _ := get(t, base+"/", map[string]string{"Host": host})
		return code // 404 when allowed: "/" serves nothing
	}
	fly := map[string]string{"FLY_APP_NAME": "my-garmin"}
	if code := probe(fly, "my-garmin.fly.dev"); code != 404 {
		t.Errorf("the app's own hostname: %d, want 404", code)
	}
	if code := probe(fly, "other-app.fly.dev"); code != 403 {
		t.Errorf("another app's hostname: %d, want 403", code)
	}
	if code := probe(fly, "localhost"); code != 404 {
		t.Errorf("localhost stays allowed: %d", code)
	}
	if code := probe(nil, "my-garmin.fly.dev"); code != 403 {
		t.Errorf("without FLY_APP_NAME that hostname is not special: %d, want 403", code)
	}
	both := map[string]string{"FLY_APP_NAME": "my-garmin", "GARMIN_MCP_ALLOWED_HOSTS": "garmin.example.com"}
	if probe(both, "garmin.example.com") != 404 || probe(both, "my-garmin.fly.dev") != 404 {
		t.Error("a configured hostname and the Fly one should both be allowed")
	}
}

func TestParseConfig_HelpNeverShowsTheToken(t *testing.T) {
	const secretToken = "HELP-LEAK-CHECK-0123456789"
	for name, env := range map[string]map[string]string{
		"environment": {"GARMIN_MCP_TOKEN": secretToken},
		"file":        {"GARMIN_MCP_TOKEN_FILE": writeSecret(t, secretToken)},
	} {
		// -h, and a mistyped flag, both print the usage text with the flag defaults.
		for _, args := range [][]string{{"-h"}, {"-no-such-flag"}} {
			var out strings.Builder
			_, _ = parseConfig(args, func(k string) string { return env[k] }, &out)
			if strings.Contains(out.String(), secretToken) {
				t.Errorf("%s %v: the usage text shows the token:\n%s", name, args, out.String())
			}
			if !strings.Contains(out.String(), "GARMIN_MCP_TOKEN") {
				t.Errorf("%s %v: the usage text should explain the token:\n%s", name, args, out.String())
			}
		}
	}
}

func TestParseConfig_TokenSources(t *testing.T) {
	env := func(k string) string { return map[string]string{"GARMIN_MCP_TOKEN": "env-token-0123456789"}[k] }
	cfg, err := parseConfig(nil, env, io.Discard)
	if err != nil || cfg.token != "env-token-0123456789" {
		t.Errorf("from the environment: token=%q err=%v", cfg.token, err)
	}
	short := func(k string) string { return map[string]string{"GARMIN_MCP_TOKEN": "short"}[k] }
	if _, err := parseConfig(nil, short, io.Discard); err == nil {
		t.Error("a short token must be refused")
	}
	if _, err := parseConfig([]string{"-token", "flag-token-0123456789"}, noEnv, io.Discard); err == nil {
		t.Error("there is no -token flag: a token on the command line would be visible in the process list")
	}
	if _, err := parseConfig([]string{"-audit-log-max-mb", "9223372036854775807"}, noEnv, io.Discard); err == nil {
		t.Error("an audit size limit that overflows when converted to bytes must be refused")
	}
	if _, err := parseConfig([]string{"-audit-log-max-mb", "-1"}, noEnv, io.Discard); err == nil {
		t.Error("a negative audit size limit must be refused")
	}
}

func TestParseConfig_ModesAreExclusive(t *testing.T) {
	for _, args := range [][]string{{"-set-key", "-key-stored"}, {"-healthcheck", "-set-key"}, {"-version", "-key-stored"}} {
		if _, err := parseConfig(args, noEnv, io.Discard); err == nil || !strings.Contains(err.Error(), "only one of") {
			t.Errorf("%v: err = %v", args, err)
		}
	}
}
