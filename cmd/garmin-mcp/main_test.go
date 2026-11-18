package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dianabarsan/garmin-training/cmd/garmin-mcp/internal/mcpserver"
	"github.com/dianabarsan/garmin-training/garmin-connect/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

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

func TestRun_Healthcheck(t *testing.T) {
	garminSrv, _ := fakeGarmin(t)
	base, _, stop := start(t, map[string]string{"GARMIN_MCP_TOKEN": "tok-0123456789abcdef"}, "-garmin-url", garminSrv.URL)
	hostPort := strings.TrimPrefix(base, "http://")
	noEnv := func(string) string { return "" }

	if err := run(context.Background(), []string{"-healthcheck", "-addr", hostPort}, noEnv, io.Discard, io.Discard, nil); err != nil {
		t.Errorf("a healthy server (even with a token set) must pass: %v", err)
	}
	// A wildcard address, as used inside a container, is probed via loopback.
	port := hostPort[strings.LastIndex(hostPort, ":")+1:]
	for _, wildcard := range []string{":" + port, "0.0.0.0:" + port} {
		if err := run(context.Background(), []string{"-healthcheck", "-addr", wildcard}, noEnv, io.Discard, io.Discard, nil); err != nil {
			t.Errorf("-addr %s: %v", wildcard, err)
		}
	}
	if err := run(context.Background(), []string{"-healthcheck", "-addr", "not-an-address"}, noEnv, io.Discard, io.Discard, nil); err == nil {
		t.Error("a malformed address must fail")
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{"-healthcheck", "-addr", hostPort}, noEnv, io.Discard, io.Discard, nil); err == nil {
		t.Error("a stopped server must be reported unhealthy")
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

func TestRun_DisableAuthEndpoint(t *testing.T) {
	garminSrv, _ := fakeGarmin(t)
	for name, tc := range map[string]struct {
		env  map[string]string
		args []string
	}{
		"flag":        {map[string]string{"GARMIN_PERSONAL_API_KEY": goodKey}, []string{"-disable-auth-endpoint"}},
		"environment": {map[string]string{"GARMIN_PERSONAL_API_KEY": goodKey, "GARMIN_MCP_DISABLE_AUTH_ENDPOINT": "true"}, nil},
	} {
		base, logs, _ := start(t, tc.env, append([]string{"-garmin-url", garminSrv.URL}, tc.args...)...)
		for _, path := range []string{"/auth", "/"} {
			if code, _ := get(t, base+path, nil); code != 404 {
				t.Errorf("%s: GET %s -> %d, want 404", name, path, code)
			}
		}
		if code, _ := post(t, base+"/auth", `{"api_key":"`+goodKey+`"}`); code != 404 {
			t.Errorf("%s: POST /auth -> %d, want 404", name, code)
		}
		if code, _ := get(t, base+"/healthz", nil); code != 200 {
			t.Errorf("%s: healthz -> %d", name, code)
		}
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).
			Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: base + "/mcp"}, nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_steps"})
		cs.Close()
		if err != nil || res.IsError {
			t.Errorf("%s: the key from the environment must work: err=%v res=%+v", name, err, res)
		}
		if strings.Contains(logs.String(), "POST {") || strings.Contains(logs.String(), "browser") {
			t.Errorf("%s: the log still tells you to use the removed endpoint:\n%s", name, logs.String())
		}
		if !strings.Contains(logs.String(), "auth_endpoint=false") {
			t.Errorf("%s: the startup line should say the endpoint is off:\n%s", name, logs.String())
		}
	}
}

func TestRun_DisableAuthEndpointNeedsAKeyFromTheStart(t *testing.T) {
	noEnv := func(string) string { return "" }
	err := run(context.Background(), []string{"-addr", "127.0.0.1:0", "-disable-auth-endpoint"}, noEnv, io.Discard, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "GARMIN_PERSONAL_API_KEY") {
		t.Errorf("no key at all: err = %v, want advice on how to supply one", err)
	}
	// A key store that does not exist yet is the same situation.
	err = run(context.Background(), []string{"-addr", "127.0.0.1:0", "-disable-auth-endpoint", "-key-store", filepath.Join(t.TempDir(), "absent")}, noEnv, io.Discard, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "-key-store") {
		t.Errorf("empty key store: err = %v", err)
	}
	// An unparsable environment value is reported, not ignored.
	bad := func(k string) string {
		if k == "GARMIN_MCP_DISABLE_AUTH_ENDPOINT" {
			return "maybe"
		}
		return ""
	}
	if err := run(context.Background(), []string{"-addr", "127.0.0.1:0"}, bad, io.Discard, io.Discard, nil); err == nil || !strings.Contains(err.Error(), "true or false") {
		t.Errorf("bad env value: err = %v", err)
	}
}

// The documented way to test through a tunnel: store the key once with /auth,
// then restart with the endpoint removed and let the key store supply it.
func TestRun_StoreTheKeyThenRestartWithoutTheEndpoint(t *testing.T) {
	garminSrv, _ := fakeGarmin(t)
	store := filepath.Join(t.TempDir(), "garmin.key")

	base1, _, stop1 := start(t, nil, "-garmin-url", garminSrv.URL, "-key-store", store)
	if code, _ := post(t, base1+"/auth", `{"api_key":"`+goodKey+`"}`); code != 200 {
		t.Fatalf("storing the key: %d", code)
	}
	if err := stop1(); err != nil {
		t.Fatal(err)
	}

	base2, logs2, _ := start(t, nil, "-garmin-url", garminSrv.URL, "-key-store", store, "-disable-auth-endpoint")
	if code, _ := get(t, base2+"/auth", nil); code != 404 {
		t.Errorf("/auth after restart: %d, want 404", code)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).
		Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: base2 + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	if res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_steps"}); err != nil || res.IsError {
		t.Errorf("the stored key should work without the endpoint: err=%v res=%+v\n%s", err, res, logs2.String())
	}
}

func TestRun_RejectedKeyWithoutTheEndpointSaysToRestart(t *testing.T) {
	garminSrv, _ := fakeGarmin(t)
	base, logs, _ := start(t, map[string]string{"GARMIN_PERSONAL_API_KEY": "not-the-key-SECRET"}, "-garmin-url", garminSrv.URL, "-disable-auth-endpoint")
	if code, _ := get(t, base+"/healthz", nil); code != 200 {
		t.Errorf("a rejected key is not fatal: %d", code)
	}
	if !strings.Contains(logs.String(), "restart with a new key") || strings.Contains(logs.String(), "provide a key at /auth") {
		t.Errorf("the warning must say to restart, not to use /auth:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), "SECRET") {
		t.Errorf("the rejected key leaked into the log:\n%s", logs.String())
	}
}

// A key from the environment that cannot be checked because Garmin is
// unreachable is used anyway, not discarded: otherwise a network blip at
// startup would leave a server without /auth unusable until restarted.
func TestRun_UnreachableGarminKeepsTheEnvironmentKey(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	unreachable := closed.URL
	closed.Close()

	base, logs, _ := start(t, map[string]string{"GARMIN_PERSONAL_API_KEY": goodKey}, "-garmin-url", unreachable)
	if _, body := get(t, base+"/auth", nil); !strings.Contains(body, `"authenticated":true`) {
		t.Errorf("the unchecked key should be in use: %s\n%s", body, logs.String())
	}
	if !strings.Contains(logs.String(), "using it anyway") || !strings.Contains(logs.String(), "could not reach Garmin") {
		t.Errorf("the log should say the key could not be checked:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), goodKey) {
		t.Errorf("key in log:\n%s", logs.String())
	}

	// Without the endpoint the server must still start, with the key.
	_, logs2, _ := start(t, map[string]string{"GARMIN_PERSONAL_API_KEY": goodKey}, "-garmin-url", unreachable, "-disable-auth-endpoint")
	if !strings.Contains(logs2.String(), "key_loaded=true") {
		t.Errorf("with -disable-auth-endpoint the key should be loaded:\n%s", logs2.String())
	}
}

func TestReasonOf(t *testing.T) {
	for err, want := range map[error]string{
		nil:                      "",
		auth.ErrInvalidKey:       "rejected by Garmin",
		auth.ErrMalformedToken:   "unexpected answer from Garmin",
		mcpserver.ErrKeyNotSaved: "could not save it to the key store",
		context.DeadlineExceeded: "Garmin did not answer in time",
		errors.New("dial tcp /secret/path: refused"): "could not reach Garmin",
	} {
		if got := reasonOf(err); got != want {
			t.Errorf("reasonOf(%v) = %q, want %q", err, got, want)
		}
	}
}

func TestRun_RefusesToExposeItselfWithoutAToken(t *testing.T) {
	var logs syncBuf
	err := run(context.Background(), []string{"-addr", "0.0.0.0:0"}, func(string) string { return "" }, io.Discard, &logs, nil)
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("err = %v, want a refusal that mentions the token", err)
	}
	// With a token it is allowed (and then stopped immediately).
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"-addr", "0.0.0.0:0", "-token", "flag-token-0123456789"}, func(string) string { return "" }, io.Discard, &logs, func(a string) { ready <- a })
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
	if err := run(context.Background(), []string{"-version"}, noEnv, &out, io.Discard, nil); err != nil || !strings.Contains(out.String(), "garmin-mcp") {
		t.Errorf("-version: %v %q", err, out.String())
	}
	out = syncBuf{}
	if err := run(context.Background(), []string{"-h"}, noEnv, io.Discard, &out, nil); err != nil || !strings.Contains(out.String(), "GARMIN_PERSONAL_API_KEY") {
		t.Errorf("-h: %v %q", err, out.String())
	}
	if err := run(context.Background(), []string{"-nope"}, noEnv, io.Discard, io.Discard, nil); err == nil {
		t.Error("an unknown flag must be an error")
	}
	if err := run(context.Background(), []string{"-log-level", "loud"}, noEnv, io.Discard, io.Discard, nil); err == nil || !strings.Contains(err.Error(), "log-level") {
		t.Errorf("bad log level: %v", err)
	}
	// An address that is already in use is reported, not swallowed.
	l := httptest.NewServer(http.NotFoundHandler())
	defer l.Close()
	if err := run(context.Background(), []string{"-addr", strings.TrimPrefix(l.URL, "http://")}, noEnv, io.Discard, io.Discard, nil); err == nil {
		t.Error("listening on a busy port must fail")
	}
}
