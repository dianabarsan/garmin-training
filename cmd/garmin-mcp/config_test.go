package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	base, _, _ := start(t, map[string]string{"GARMIN_MCP_TOKEN": "env-token"}, "-garmin-url", garminSrv.URL)
	if code, _ := get(t, base+"/auth", nil); code != 401 {
		t.Errorf("without token: %d", code)
	}
	if code, _ := get(t, base+"/auth", map[string]string{"Authorization": "Bearer env-token"}); code != 200 {
		t.Errorf("with token: %d", code)
	}
}

func TestRun_RefusesToExposeItselfWithoutAToken(t *testing.T) {
	var logs syncBuf
	err := run(context.Background(), []string{"-addr", "0.0.0.0:0"}, func(string) string { return "" }, &logs, nil)
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("err = %v, want a refusal that mentions the token", err)
	}
	// With a token it is allowed (and then stopped immediately).
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"-addr", "0.0.0.0:0", "-token", "t"}, func(string) string { return "" }, &logs, func(a string) { ready <- a })
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("exited: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("did not start")
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("shutdown: %v", err)
	}
}

func TestRun_FlagsAndVersion(t *testing.T) {
	noEnv := func(string) string { return "" }
	var out syncBuf
	if err := run(context.Background(), []string{"-version"}, noEnv, &out, nil); err != nil || !strings.Contains(out.String(), "garmin-mcp") {
		t.Errorf("-version: %v %q", err, out.String())
	}
	out = syncBuf{}
	if err := run(context.Background(), []string{"-h"}, noEnv, &out, nil); err != nil || !strings.Contains(out.String(), "GARMIN_PERSONAL_API_KEY") {
		t.Errorf("-h: %v %q", err, out.String())
	}
	if err := run(context.Background(), []string{"-nope"}, noEnv, io.Discard, nil); err == nil {
		t.Error("an unknown flag must be an error")
	}
	if err := run(context.Background(), []string{"-log-level", "loud"}, noEnv, io.Discard, nil); err == nil || !strings.Contains(err.Error(), "log-level") {
		t.Errorf("bad log level: %v", err)
	}
	// An address that is already in use is reported, not swallowed.
	l := httptest.NewServer(http.NotFoundHandler())
	defer l.Close()
	if err := run(context.Background(), []string{"-addr", strings.TrimPrefix(l.URL, "http://")}, noEnv, io.Discard, nil); err == nil {
		t.Error("listening on a busy port must fail")
	}
}

func TestRun_SecretsFromFiles(t *testing.T) {
	garminSrv, _ := fakeGarmin(t)
	env := map[string]string{
		"GARMIN_MCP_TOKEN_FILE":        writeSecret(t, "file-token\n"), // a trailing newline is normal in secret files
		"GARMIN_PERSONAL_API_KEY_FILE": writeSecret(t, goodKey+"\n"),
	}
	base, logs, _ := start(t, env, "-garmin-url", garminSrv.URL)
	if code, _ := get(t, base+"/auth", nil); code != 401 {
		t.Errorf("the token from a file must be enforced: %d", code)
	}
	code, body := get(t, base+"/auth", map[string]string{"Authorization": "Bearer file-token"})
	if code != 200 || !strings.Contains(body, `"authenticated":true`) {
		t.Errorf("token and key from files: %d %s\n%s", code, body, logs.String())
	}
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
	} {
		err := run(context.Background(), []string{"-addr", "127.0.0.1:0"}, func(k string) string { return env[k] }, io.Discard, nil)
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
			"garmin.example.com":              200,
			"GARMIN.example.com:443":          200, // case and port are ignored
			"other.example.org":               200,
			"localhost":                       200, // local administration and health checks
			"evil.example":                    403,
			"garmin.example.com.evil.example": 403,
		} {
			if code, _ := get(t, base+"/auth", map[string]string{"Host": host}); code != want {
				t.Errorf("%s: Host %q -> %d, want %d", name, host, code, want)
			}
		}
	}
}

func TestRun_AllowedHostsRejectsURLs(t *testing.T) {
	err := run(context.Background(), []string{"-addr", "127.0.0.1:0", "-allowed-host", "https://garmin.example.com"},
		func(string) string { return "" }, io.Discard, nil)
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
		code, _ := get(t, base+"/auth", map[string]string{"Host": host})
		return code
	}
	fly := map[string]string{"FLY_APP_NAME": "my-garmin"}
	if code := probe(fly, "my-garmin.fly.dev"); code != 200 {
		t.Errorf("the app's own hostname: %d, want 200", code)
	}
	if code := probe(fly, "other-app.fly.dev"); code != 403 {
		t.Errorf("another app's hostname: %d, want 403", code)
	}
	if code := probe(fly, "localhost"); code != 200 {
		t.Errorf("localhost stays allowed: %d", code)
	}
	if code := probe(nil, "my-garmin.fly.dev"); code != 403 {
		t.Errorf("without FLY_APP_NAME that hostname is not special: %d, want 403", code)
	}
	both := map[string]string{"FLY_APP_NAME": "my-garmin", "GARMIN_MCP_ALLOWED_HOSTS": "garmin.example.com"}
	if probe(both, "garmin.example.com") != 200 || probe(both, "my-garmin.fly.dev") != 200 {
		t.Error("a configured hostname and the Fly one should both be allowed")
	}
}
