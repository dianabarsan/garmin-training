package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// headerTransport adds fixed headers to every request.
type headerTransport map[string]string

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range h {
		r.Header.Set(k, v)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func readAuditFile(t *testing.T, path string) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("audit line is not JSON: %q", line)
		}
		out = append(out, m)
	}
	return out
}

func TestRun_AuditLogFile(t *testing.T) {
	garminSrv, _ := fakeGarmin(t)
	const token = "audit-token-DO-NOT-LOG"
	env := map[string]string{"GARMIN_PERSONAL_API_KEY": goodKey, "GARMIN_MCP_TOKEN": token}
	dir := filepath.Join(t.TempDir(), "state") // does not exist yet
	path := filepath.Join(dir, "audit.log")

	session := func() {
		base, logs, stop := start(t, env, "-garmin-url", garminSrv.URL, "-audit-log", path)
		get(t, base+"/auth", nil) // refused: no token
		hc := &http.Client{Transport: headerTransport{"Authorization": "Bearer " + token}}
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "audit-test", Version: "1"}, nil).
			Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: base + "/mcp", HTTPClient: hc}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_steps"}); err != nil || res.IsError {
			t.Fatalf("get_steps: %v %+v", err, res)
		}
		cs.Close()
		if err := stop(); err != nil {
			t.Fatal(err)
		}
		// With a file, audit records stay out of the normal log.
		if strings.Contains(logs.String(), "tool_call") || strings.Contains(logs.String(), "access_denied") || strings.Contains(logs.String(), "audit=true") {
			t.Errorf("audit records leaked into the operational log:\n%s", logs.String())
		}
		if !strings.Contains(logs.String(), "audit_log="+path) {
			t.Errorf("the startup line should say where the audit log is:\n%s", logs.String())
		}
	}

	session()
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("audit file: %v %v, want mode 0600", info, err)
	}
	if info, _ := os.Stat(dir); info.Mode().Perm() != 0o700 {
		t.Errorf("audit dir mode = %v, want 0700", info.Mode().Perm())
	}
	first := readAuditFile(t, path)
	var denied, calls, connected int
	for _, m := range first {
		switch m["msg"] {
		case "access_denied":
			if m["reason"] == "missing_token" && m["path"] == "/auth" {
				denied++
			}
		case "tool_call":
			if m["tool"] == "get_steps" && m["outcome"] == "ok" && m["client"] == "audit-test/1" {
				calls++
			}
		case "client_connected":
			connected++
		}
	}
	if denied != 1 || calls != 1 || connected != 1 {
		t.Errorf("records: denied=%d tool_calls=%d connected=%d in %+v", denied, calls, connected, first)
	}

	// A second run appends; it never truncates the earlier trail.
	session()
	second := readAuditFile(t, path)
	if len(second) <= len(first) {
		t.Fatalf("second run added nothing: %d -> %d records", len(first), len(second))
	}
	for i, m := range first {
		if m["time"] != second[i]["time"] || m["msg"] != second[i]["msg"] {
			t.Errorf("record %d was altered by the second run", i)
		}
	}
	raw, _ := os.ReadFile(path)
	for _, secret := range []string{goodKey, token, "DO-NOT-LOG"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("the audit file contains %q", secret)
		}
	}
}

func TestRun_AuditRecordsGoToTheNormalLogByDefault(t *testing.T) {
	garminSrv, _ := fakeGarmin(t)
	base, logs, stop := start(t, map[string]string{"GARMIN_PERSONAL_API_KEY": goodKey}, "-garmin-url", garminSrv.URL)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).
		Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: base + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_steps"})
	cs.Close()
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "tool_call") || !strings.Contains(logs.String(), "audit=true") || !strings.Contains(logs.String(), "tool=get_steps") {
		t.Errorf("audit records should be in the normal log, tagged audit=true:\n%s", logs.String())
	}
}

func TestRun_UnusableAuditLogPathFailsAtStartup(t *testing.T) {
	garminSrv, _ := fakeGarmin(t)
	env := func(string) string { return "" }
	// A directory cannot be opened for appending.
	err := run(context.Background(), []string{"-addr", "127.0.0.1:0", "-garmin-url", garminSrv.URL, "-audit-log", t.TempDir()}, env, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "-audit-log") {
		t.Errorf("err = %v, want a clear -audit-log error", err)
	}
	// So can a path below a regular file.
	file := filepath.Join(t.TempDir(), "plain")
	_ = os.WriteFile(file, nil, 0o600)
	err = run(context.Background(), []string{"-addr", "127.0.0.1:0", "-garmin-url", garminSrv.URL, "-audit-log", filepath.Join(file, "audit.log")}, env, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "-audit-log") {
		t.Errorf("err = %v", err)
	}
}
