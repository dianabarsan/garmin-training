package main

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

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
