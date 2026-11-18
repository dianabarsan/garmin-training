package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dianabarsan/garmin-training/garmin-connect/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// setKeyCmd runs -set-key with key on standard input.
func setKeyCmd(t *testing.T, garminURL, store, key string) (string, error) {
	t.Helper()
	var out syncBuf
	err := run(context.Background(), []string{"-set-key", "-key-store", store, "-garmin-url", garminURL}, noEnv, strings.NewReader(key+"\n"), &out, io.Discard, nil)
	return out.String(), err
}

// getSteps calls the get_steps tool over HTTP.
func getSteps(t *testing.T, base string) error {
	t.Helper()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).
		Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: base + "/mcp"}, nil)
	if err != nil {
		return err
	}
	defer cs.Close()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_steps"})
	if err != nil {
		return err
	}
	b, _ := json.Marshal(res.StructuredContent)
	if res.IsError || !strings.Contains(string(b), `"steps":4321`) {
		var text string
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				text += tc.Text
			}
		}
		return fmt.Errorf("get_steps: %s %s", text, b)
	}
	return nil
}

// waitForLog waits until the log contains want, which the startup key check
// writes shortly after the server starts listening.
func waitForLog(t *testing.T, logs *syncBuf, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(logs.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("the log never said %q:\n%s", want, logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRun_EndToEnd_SetKeyThenUseTools(t *testing.T) {
	garminSrv, _ := fakeGarmin(t)
	store := filepath.Join(t.TempDir(), "state", "garmin.key")

	if _, err := setKeyCmd(t, garminSrv.URL, store, "wrong-key-SECRET"); err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Errorf("a rejected key: err = %v", err)
	}
	if _, err := os.Stat(store); !os.IsNotExist(err) {
		t.Errorf("a rejected key must not be saved: %v", err)
	}
	out, err := setKeyCmd(t, garminSrv.URL, store, goodKey)
	if err != nil || !strings.Contains(out, "saved") || strings.Contains(out, goodKey) {
		t.Fatalf("a good key: %q %v", out, err)
	}
	if info, err := os.Stat(store); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v, want mode 0600", info, err)
	}

	base, logs, stop := start(t, nil, "-garmin-url", garminSrv.URL, "-key-store", store)
	if code, body := get(t, base+"/healthz", nil); code != 200 || !strings.Contains(body, "ok") {
		t.Errorf("healthz: %d %q", code, body)
	}
	if err := getSteps(t, base); err != nil {
		t.Fatal(err)
	}
	waitForLog(t, logs, "Garmin accepted the API key")
	if err := stop(); err != nil {
		t.Errorf("shutdown: %v", err)
	}
	if strings.Contains(logs.String(), goodKey) {
		t.Errorf("the key is in the log:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "key_from=\"key store\"") {
		t.Errorf("the startup line should say where the key came from:\n%s", logs.String())
	}
}

// The key can only be given to the server at startup: there is no endpoint for
// it, and no key page.
func TestRun_HasNoKeyEndpoint(t *testing.T) {
	garminSrv, _ := fakeGarmin(t)
	base, _, _ := start(t, nil, "-garmin-url", garminSrv.URL)
	for _, path := range []string{"/auth", "/"} {
		if code, _ := get(t, base+path, nil); code != 404 {
			t.Errorf("GET %s -> %d, want 404", path, code)
		}
	}
	if code, _ := post(t, base+"/auth", `{"api_key":"`+goodKey+`"}`); code != 404 {
		t.Errorf("POST /auth -> %d, want 404", code)
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

	if err := run(context.Background(), []string{"-healthcheck", "-addr", hostPort}, noEnv, nil, io.Discard, io.Discard, nil); err != nil {
		t.Errorf("a healthy server (even with a token set) must pass: %v", err)
	}
	// A wildcard address, as used inside a container, is probed via loopback.
	port := hostPort[strings.LastIndex(hostPort, ":")+1:]
	for _, wildcard := range []string{":" + port, "0.0.0.0:" + port} {
		if err := run(context.Background(), []string{"-healthcheck", "-addr", wildcard}, noEnv, nil, io.Discard, io.Discard, nil); err != nil {
			t.Errorf("-addr %s: %v", wildcard, err)
		}
	}
	if err := run(context.Background(), []string{"-healthcheck", "-addr", "not-an-address"}, noEnv, nil, io.Discard, io.Discard, nil); err == nil {
		t.Error("a malformed address must fail")
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{"-healthcheck", "-addr", hostPort}, noEnv, nil, io.Discard, io.Discard, nil); err == nil {
		t.Error("a stopped server must be reported unhealthy")
	}
}

func TestRun_KeyFromEnvironment(t *testing.T) {
	garminSrv, _ := fakeGarmin(t)
	// The environment wins over the key store.
	store := filepath.Join(t.TempDir(), "garmin.key")
	if err := os.WriteFile(store, []byte("stale-stored-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	base, logs, _ := start(t, map[string]string{"GARMIN_PERSONAL_API_KEY": goodKey}, "-garmin-url", garminSrv.URL, "-key-store", store)
	if err := getSteps(t, base); err != nil {
		t.Errorf("the key from the environment should be used: %v\n%s", err, logs.String())
	}
	waitForLog(t, logs, "Garmin accepted the API key")
	if !strings.Contains(logs.String(), "key_from=environment") || strings.Contains(logs.String(), goodKey) {
		t.Errorf("log:\n%s", logs.String())
	}
}

func TestRun_NeedsAKeyToStart(t *testing.T) {
	err := run(context.Background(), []string{"-addr", "127.0.0.1:0"}, noEnv, nil, io.Discard, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "GARMIN_PERSONAL_API_KEY") || !strings.Contains(err.Error(), "-set-key") {
		t.Errorf("no key at all: err = %v, want advice on how to supply one", err)
	}
	for name, content := range map[string]*string{"missing": nil, "blank": ptr(" \n")} {
		store := filepath.Join(t.TempDir(), "garmin.key")
		if content != nil {
			_ = os.WriteFile(store, []byte(*content), 0o600)
		}
		err := run(context.Background(), []string{"-addr", "127.0.0.1:0", "-key-store", store}, noEnv, nil, io.Discard, io.Discard, nil)
		if err == nil || !strings.Contains(err.Error(), "-set-key") {
			t.Errorf("%s key store: err = %v", name, err)
		}
	}
}

func ptr[T any](v T) *T { return &v }

// A rejected key is reported but not fatal: the server keeps answering, and
// the tools say what to do.
func TestRun_RejectedKeyIsReported(t *testing.T) {
	garminSrv, _ := fakeGarmin(t)
	base, logs, _ := start(t, map[string]string{"GARMIN_PERSONAL_API_KEY": "not-the-key-SECRET"}, "-garmin-url", garminSrv.URL)
	waitForLog(t, logs, "Garmin rejected the API key")
	if code, _ := get(t, base+"/healthz", nil); code != 200 {
		t.Errorf("healthz: %d", code)
	}
	if err := getSteps(t, base); err == nil || !strings.Contains(err.Error(), "setup.sh --new-key") {
		t.Errorf("the tool should say how to replace the key: %v", err)
	}
	if strings.Contains(logs.String(), "SECRET") {
		t.Errorf("the rejected key leaked into the log:\n%s", logs.String())
	}
}

// A key that cannot be checked because Garmin is unreachable is kept, so a
// network blip at startup does not leave the server unusable.
func TestRun_UnreachableGarminKeepsTheKey(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	unreachable := closed.URL
	closed.Close()

	base, logs, _ := start(t, map[string]string{"GARMIN_PERSONAL_API_KEY": goodKey}, "-garmin-url", unreachable)
	waitForLog(t, logs, "could not check the API key")
	if !strings.Contains(logs.String(), "could not reach Garmin") || strings.Contains(logs.String(), goodKey) {
		t.Errorf("log:\n%s", logs.String())
	}
	if code, _ := get(t, base+"/healthz", nil); code != 200 {
		t.Errorf("healthz: %d", code)
	}
}

func TestRun_SetKeyFailures(t *testing.T) {
	garminSrv, _ := fakeGarmin(t)
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	store := filepath.Join(t.TempDir(), "garmin.key")

	if err := run(context.Background(), []string{"-set-key", "-garmin-url", garminSrv.URL}, noEnv, strings.NewReader(goodKey), io.Discard, io.Discard, nil); err == nil || !strings.Contains(err.Error(), "-key-store") {
		t.Errorf("without -key-store: %v", err)
	}
	if _, err := setKeyCmd(t, garminSrv.URL, store, "  "); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("empty input: %v", err)
	}
	if _, err := setKeyCmd(t, garminSrv.URL, store, strings.Repeat("k", maxKeyInput+1)); err == nil || !strings.Contains(err.Error(), "too long") {
		t.Errorf("oversized input: %v", err)
	}
	for _, in := range []string{goodKey + " " + goodKey, goodKey + "\n" + goodKey, goodKey + "\x00"} {
		if _, err := setKeyCmd(t, garminSrv.URL, store, in); err == nil || !strings.Contains(err.Error(), "one line") {
			t.Errorf("key with whitespace or control characters %q: %v", in, err)
		}
	}
	if _, err := setKeyCmd(t, closed.URL, store, goodKey); err == nil || !strings.Contains(err.Error(), "not saved") {
		t.Errorf("Garmin unreachable: %v", err)
	}
	if _, err := os.Stat(store); !os.IsNotExist(err) {
		t.Errorf("a key that could not be checked must not be saved: %v", err)
	}
	// A save failure is reported.
	file := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(file, nil, 0o600)
	if _, err := setKeyCmd(t, garminSrv.URL, filepath.Join(file, "key"), goodKey); err == nil {
		t.Error("saving under a regular file should fail")
	}
}

func TestRun_KeyStored(t *testing.T) {
	store := filepath.Join(t.TempDir(), "garmin.key")
	stored := func() error {
		return run(context.Background(), []string{"-key-stored", "-key-store", store}, noEnv, nil, io.Discard, io.Discard, nil)
	}
	if err := stored(); err == nil {
		t.Error("no file: want an error")
	}
	_ = os.WriteFile(store, []byte("\n"), 0o600)
	if err := stored(); err == nil {
		t.Error("blank file: want an error")
	}
	_ = os.WriteFile(store, []byte("SECRET-KEY\n"), 0o600)
	var out syncBuf
	if err := run(context.Background(), []string{"-key-stored", "-key-store", store}, noEnv, nil, &out, &out, nil); err != nil || strings.Contains(out.String(), "SECRET") {
		t.Errorf("stored key: err=%v output=%q", err, out.String())
	}
	if err := run(context.Background(), []string{"-key-stored"}, noEnv, nil, io.Discard, io.Discard, nil); err == nil {
		t.Error("without -key-store: want an error")
	}
}

func TestReasonOf(t *testing.T) {
	for err, want := range map[error]string{
		nil:                      "",
		auth.ErrInvalidKey:       "rejected by Garmin",
		auth.ErrMalformedToken:   "unexpected answer from Garmin",
		context.DeadlineExceeded: "Garmin did not answer in time",
		&net.OpError{Op: "dial", Err: errors.New("/secret/path: refused")}: "could not reach Garmin",
		&net.DNSError{Name: "example.invalid"}:                             "could not reach Garmin",
		errors.New("net/http: invalid header field value"):                 "the check failed",
	} {
		if got := reasonOf(err); got != want {
			t.Errorf("reasonOf(%v) = %q, want %q", err, got, want)
		}
	}
}

func TestRun_RefusesToExposeItselfWithoutAToken(t *testing.T) {
	var logs syncBuf
	err := run(context.Background(), []string{"-addr", "0.0.0.0:0"}, keyEnv, nil, io.Discard, &logs, nil)
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("err = %v, want a refusal that mentions the token", err)
	}
	// With a token it is allowed (and then stopped immediately).
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		env := func(k string) string {
			if k == "GARMIN_MCP_TOKEN" {
				return "env-token-0123456789"
			}
			return keyEnv(k)
		}
		done <- run(ctx, []string{"-addr", "0.0.0.0:0", "-garmin-url", "http://127.0.0.1:1"}, env, nil, io.Discard, &logs, func(a string) { ready <- a })
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
	var out syncBuf
	if err := run(context.Background(), []string{"-version"}, noEnv, nil, &out, io.Discard, nil); err != nil || !strings.Contains(out.String(), "garmin-mcp") {
		t.Errorf("-version: %v %q", err, out.String())
	}
	out = syncBuf{}
	if err := run(context.Background(), []string{"-h"}, noEnv, nil, io.Discard, &out, nil); err != nil || !strings.Contains(out.String(), "GARMIN_PERSONAL_API_KEY") {
		t.Errorf("-h: %v %q", err, out.String())
	}
	if err := run(context.Background(), []string{"-nope"}, noEnv, nil, io.Discard, io.Discard, nil); err == nil {
		t.Error("an unknown flag must be an error")
	}
	if err := run(context.Background(), []string{"-log-level", "loud"}, noEnv, nil, io.Discard, io.Discard, nil); err == nil || !strings.Contains(err.Error(), "log-level") {
		t.Errorf("bad log level: %v", err)
	}
	// An address that is already in use is reported, not swallowed.
	l := httptest.NewServer(http.NotFoundHandler())
	defer l.Close()
	if err := run(context.Background(), []string{"-addr", strings.TrimPrefix(l.URL, "http://")}, keyEnv, nil, io.Discard, io.Discard, nil); err == nil {
		t.Error("listening on a busy port must fail")
	}
}
