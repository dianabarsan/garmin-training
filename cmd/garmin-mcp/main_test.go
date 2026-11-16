package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	tokenPath = "/personalapitoken-service/personal-api/personal-api-key/token"
	goodKey   = "cmd-test-key-ABCDEF123456"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func fakeGarmin(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var exchanges atomic.Int32
	enc := base64.RawURLEncoding.EncodeToString
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case tokenPath:
			exchanges.Add(1)
			if r.Header.Get("Authorization") != "ApiKey "+goodKey {
				http.Error(w, "no", http.StatusUnauthorized)
				return
			}
			jwt := enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(fmt.Sprintf(`{"exp":%d}`, time.Now().Add(24*time.Hour).Unix()))) + ".s"
			_, _ = w.Write([]byte(jwt))
		case "/usersummary-service/stats/steps/daily/" + time.Now().AddDate(0, 0, -6).Format(time.DateOnly) + "/" + time.Now().Format(time.DateOnly):
			_, _ = w.Write([]byte(`[{"calendarDate":"2026-11-16","totalSteps":4321,"stepGoal":8000,"totalDistance":3000}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &exchanges
}

// start runs the server on a random port and returns its base URL, its log
// output, and a stop function that waits for a clean shutdown.
func start(t *testing.T, env map[string]string, args ...string) (base string, logs *syncBuf, stop func() error) {
	t.Helper()
	logs = &syncBuf{}
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	done := make(chan error, 1)
	all := append([]string{"-addr", "127.0.0.1:0"}, args...)
	go func() {
		done <- run(ctx, all, func(k string) string { return env[k] }, logs, func(a string) { ready <- a })
	}()
	select {
	case addr := <-ready:
		base = "http://" + addr
	case err := <-done:
		cancel()
		t.Fatalf("run exited early: %v\n%s", err, logs.String())
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("server did not start")
	}
	var once sync.Once
	var result error
	stop = func() error {
		once.Do(func() {
			cancel()
			select {
			case result = <-done:
			case <-time.After(10 * time.Second):
				result = fmt.Errorf("server did not shut down")
			}
		})
		return result
	}
	t.Cleanup(func() { _ = stop() })
	return base, logs, stop
}

func get(t *testing.T, url string, hdr map[string]string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func post(t *testing.T, url, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestRun_EndToEnd_ProvideKeyThenUseTools(t *testing.T) {
	garminSrv, _ := fakeGarmin(t)
	base, logs, stop := start(t, nil, "-garmin-url", garminSrv.URL)

	if code, body := get(t, base+"/healthz", nil); code != 200 || !strings.Contains(body, "ok") {
		t.Errorf("healthz: %d %q", code, body)
	}
	if code, body := get(t, base+"/auth", nil); code != 200 || !strings.Contains(body, `"authenticated":false`) {
		t.Errorf("status before: %d %s", code, body)
	}
	if code, _ := post(t, base+"/auth", `{"api_key":"wrong"}`); code != 401 {
		t.Errorf("wrong key: %d", code)
	}
	if code, body := post(t, base+"/auth", `{"api_key":"`+goodKey+`"}`); code != 200 || !strings.Contains(body, `"authenticated":true`) {
		t.Fatalf("good key: %d %s", code, body)
	}

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).
		Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: base + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_steps"})
	if err != nil || res.IsError {
		t.Fatalf("get_steps: err=%v res=%+v", err, res)
	}
	b, _ := json.Marshal(res.StructuredContent)
	if !strings.Contains(string(b), `"steps":4321`) {
		t.Errorf("tool output = %s", b)
	}

	if err := stop(); err != nil {
		t.Errorf("shutdown: %v", err)
	}
	if strings.Contains(logs.String(), goodKey) || strings.Contains(logs.String(), "wrong") && strings.Contains(logs.String(), "api_key") {
		t.Errorf("secrets in the log:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "listening") {
		t.Errorf("log lacks the startup line:\n%s", logs.String())
	}
}

func TestRun_KeyFromEnvironment(t *testing.T) {
	garminSrv, _ := fakeGarmin(t)
	base, logs, _ := start(t, map[string]string{"GARMIN_PERSONAL_API_KEY": goodKey}, "-garmin-url", garminSrv.URL)
	if _, body := get(t, base+"/auth", nil); !strings.Contains(body, `"authenticated":true`) {
		t.Errorf("a valid key in the environment should authenticate at start: %s\n%s", body, logs.String())
	}
	if strings.Contains(logs.String(), goodKey) {
		t.Errorf("key in log:\n%s", logs.String())
	}
}

func TestRun_BadEnvironmentKeyIsNotFatal(t *testing.T) {
	garminSrv, _ := fakeGarmin(t)
	const bad = "definitely-not-valid-SECRET"
	base, logs, _ := start(t, map[string]string{"GARMIN_PERSONAL_API_KEY": bad}, "-garmin-url", garminSrv.URL)
	if _, body := get(t, base+"/auth", nil); !strings.Contains(body, `"authenticated":false`) {
		t.Errorf("status: %s", body)
	}
	if !strings.Contains(logs.String(), "not accepted") {
		t.Errorf("a rejected key should be reported:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), bad) || strings.Contains(logs.String(), "SECRET") {
		t.Errorf("the rejected key leaked into the log:\n%s", logs.String())
	}
	if code, _ := post(t, base+"/auth", `{"api_key":"`+goodKey+`"}`); code != 200 {
		t.Errorf("the server must still accept a key at /auth: %d", code)
	}
}

func TestRun_KeyStoreSurvivesARestart(t *testing.T) {
	garminSrv, exchanges := fakeGarmin(t)
	store := filepath.Join(t.TempDir(), "state", "garmin.key")

	base1, _, stop1 := start(t, nil, "-garmin-url", garminSrv.URL, "-key-store", store)
	if code, _ := post(t, base1+"/auth", `{"api_key":"`+goodKey+`"}`); code != 200 {
		t.Fatalf("auth: %d", code)
	}
	if err := stop1(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(store)
	if err != nil {
		t.Fatalf("key was not persisted: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("store mode = %v, want 0600", info.Mode().Perm())
	}

	before := exchanges.Load()
	base2, logs2, _ := start(t, nil, "-garmin-url", garminSrv.URL, "-key-store", store)
	if _, body := get(t, base2+"/auth", nil); !strings.Contains(body, `"authenticated":true`) || !strings.Contains(body, `"persisted":true`) {
		t.Errorf("after restart: %s\n%s", body, logs2.String())
	}
	if exchanges.Load() != before {
		t.Error("loading the stored key must not call Garmin")
	}

	// Removing the key at /auth also removes it from disk.
	req, _ := http.NewRequest("DELETE", base2+"/auth", nil)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 200 {
		t.Fatalf("DELETE: %v %v", resp, err)
	}
	if _, err := os.Stat(store); !os.IsNotExist(err) {
		t.Errorf("key file still on disk after DELETE: %v", err)
	}
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

func TestRun_ShutsDownCleanlyWithAConnectedClient(t *testing.T) {
	garminSrv, _ := fakeGarmin(t)
	base, _, stop := start(t, map[string]string{"GARMIN_PERSONAL_API_KEY": goodKey}, "-garmin-url", garminSrv.URL)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).
		Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: base + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	start := time.Now()
	if err := stop(); err != nil {
		t.Errorf("shutdown with an open session: %v", err)
	}
	if time.Since(start) > 8*time.Second {
		t.Error("shutdown took too long")
	}
}

func writeSecret(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(p, []byte(content), 0o400); err != nil {
		t.Fatal(err)
	}
	return p
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
			if code, _ := get(t, base+"/healthz", map[string]string{"Host": host}); code != want {
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

func TestRun_Healthcheck(t *testing.T) {
	garminSrv, _ := fakeGarmin(t)
	base, _, stop := start(t, map[string]string{"GARMIN_MCP_TOKEN": "tok"}, "-garmin-url", garminSrv.URL)
	hostPort := strings.TrimPrefix(base, "http://")
	noEnv := func(string) string { return "" }

	if err := run(context.Background(), []string{"-healthcheck", "-addr", hostPort}, noEnv, io.Discard, nil); err != nil {
		t.Errorf("a healthy server (even with a token set) must pass: %v", err)
	}
	// A wildcard address, as used inside a container, is probed via loopback.
	port := hostPort[strings.LastIndex(hostPort, ":")+1:]
	for _, wildcard := range []string{":" + port, "0.0.0.0:" + port} {
		if err := run(context.Background(), []string{"-healthcheck", "-addr", wildcard}, noEnv, io.Discard, nil); err != nil {
			t.Errorf("-addr %s: %v", wildcard, err)
		}
	}
	if err := run(context.Background(), []string{"-healthcheck", "-addr", "not-an-address"}, noEnv, io.Discard, nil); err == nil {
		t.Error("a malformed address must fail")
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{"-healthcheck", "-addr", hostPort}, noEnv, io.Discard, nil); err == nil {
		t.Error("a stopped server must be reported unhealthy")
	}
}
