package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// auditCapture records the audit trail as JSON lines.
type auditCapture struct{ rec recordingLog }

func (a *auditCapture) logger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(&lockedWriter{&a.rec}, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func (a *auditCapture) all(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(a.rec.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("audit line is not JSON: %q (%v)", line, err)
		}
		out = append(out, m)
	}
	return out
}

func (a *auditCapture) events(t *testing.T, name string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, m := range a.all(t) {
		if m["msg"] == name {
			out = append(out, m)
		}
	}
	return out
}

func str(m map[string]any, k string) string { s, _ := m[k].(string); return s }

func TestAudit_ToolCallsAreRecorded(t *testing.T) {
	t.Parallel()
	var ac auditCapture
	h := newHarness(t, func(c *Config) { c.Audit = ac.logger() })
	h.authenticate(t)
	h.fg.route("/usersummary-service/stats/steps/daily/2026-11-01/2026-11-05", `[{"calendarDate":"2026-11-02","totalSteps":1234,"stepGoal":8000,"totalDistance":900}]`)
	cs := h.connect(t)

	callTool(t, cs, "get_steps", map[string]any{"start": "2026-11-01", "end": "2026-11-05"})
	callTool(t, cs, "get_steps", map[string]any{"start": "not-a-date"}) // a tool error

	calls := ac.events(t, "tool_call")
	if len(calls) != 2 {
		t.Fatalf("got %d tool_call records: %+v", len(calls), calls)
	}
	ok, bad := calls[0], calls[1]
	if str(ok, "tool") != "get_steps" || str(ok, "outcome") != "ok" || str(ok, "client") != "test/1" || str(ok, "level") != "INFO" {
		t.Errorf("successful call record = %+v", ok)
	}
	if args := str(ok, "args"); !strings.Contains(args, `"start":"2026-11-01"`) || !strings.Contains(args, `"end":"2026-11-05"`) {
		t.Errorf("args = %q, want what was asked for", args)
	}
	if n, _ := ok["result_bytes"].(float64); n <= 0 {
		t.Errorf("result_bytes = %v, want the size of what was returned", ok["result_bytes"])
	}
	if _, present := ok["duration_ms"]; !present {
		t.Errorf("no duration_ms in %+v", ok)
	}
	if str(bad, "outcome") != "error" || !strings.Contains(str(bad, "error"), "start must be a date") {
		t.Errorf("failed call record = %+v", bad)
	}
}

func TestAudit_FailuresBeforeAuthenticationAreRecorded(t *testing.T) {
	t.Parallel()
	var ac auditCapture
	h := newHarness(t, func(c *Config) { c.Audit = ac.logger() })
	callTool(t, h.connect(t), "get_sleep", nil)
	calls := ac.events(t, "tool_call")
	if len(calls) != 1 || str(calls[0], "outcome") != "error" || !strings.Contains(str(calls[0], "error"), "no Garmin API key") {
		t.Errorf("records = %+v", calls)
	}
}

func TestAudit_ArgumentsAreBoundedAndOnOneLine(t *testing.T) {
	t.Parallel()
	var ac auditCapture
	h := newHarness(t, func(c *Config) { c.Audit = ac.logger() })
	h.authenticate(t)
	h.fg.route("/activitylist-service/activities/search/activities", `[]`)
	huge := "line one\nline two\r\n" + strings.Repeat("é", 5000)
	callTool(t, h.connect(t), "list_activities", map[string]any{"search": huge})

	if lines := strings.Count(strings.TrimSpace(ac.rec.String()), "\n") + 1; lines != len(ac.all(t)) {
		t.Fatal("a record spans several lines: the log can be forged by a client")
	}
	calls := ac.events(t, "tool_call")
	if len(calls) != 1 {
		t.Fatalf("records = %+v", calls)
	}
	args := str(calls[0], "args")
	if len(args) > maxAuditArgs+len("…") {
		t.Errorf("args is %d bytes, limit %d", len(args), maxAuditArgs)
	}
	if !strings.HasSuffix(args, "…") {
		t.Errorf("a truncated value should say so: %q", args[len(args)-10:])
	}
	if !strings.Contains(ac.rec.String(), `\n`) || strings.Contains(args, "\n") && !strings.Contains(args, `\n`) {
		t.Errorf("newlines in client input must stay escaped")
	}
}

// Whatever happens, none of these may ever reach the audit trail: the Garmin
// key, the server token (valid or presented), and data a tool returned.
func TestAudit_NeverContainsSecretsOrData(t *testing.T) {
	t.Parallel()
	var ac auditCapture
	const token = "server-token-DO-NOT-LOG-1234"
	const wrongToken = "attacker-guess-DO-NOT-LOG-5678"
	const secretData = "UniqueActivityNameDoNotLog"
	h := newHarness(t, func(c *Config) { c.Audit = ac.logger(); c.Token = token })
	base := serveHandler(t, h)
	bearer := map[string]string{"Authorization": "Bearer " + token}

	h.fg.route("/activitylist-service/activities/search/activities", `[{"activityId":1,"activityName":"`+secretData+`","activityType":{"typeKey":"running"}}]`)
	do(t, "POST", base+"/auth", "application/json", `{"api_key":"wrong-key-LEAKCHECK"}`, bearer)
	do(t, "POST", base+"/auth", "application/json", `{"api_key":"`+validKey+`"}`, bearer)
	do(t, "GET", base+"/auth", "", "", map[string]string{"Authorization": "Bearer " + wrongToken})
	do(t, "DELETE", base+"/auth", "", "", bearer)
	h.authenticate(t)
	hc := &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", "Bearer "+token)
		return http.DefaultTransport.RoundTrip(r)
	})}
	_ = hc // the MCP path below uses the in-memory session; the HTTP flow above covers the headers
	res := callTool(t, h.connect(t), "list_activities", nil)
	if !strings.Contains(resultText(res), secretData) {
		t.Fatalf("test setup: the tool result should contain the data, got %q", resultText(res))
	}

	got := ac.rec.String()
	for _, secret := range []string{validKey, "wrong-key-LEAKCHECK", token, wrongToken, "DO-NOT-LOG", secretData} {
		if strings.Contains(got, secret) {
			t.Errorf("the audit trail contains %q:\n%s", secret, got)
		}
	}
}

func TestAudit_RefusedRequestsAreRecorded(t *testing.T) {
	t.Parallel()
	var ac auditCapture
	h := newHarness(t, func(c *Config) { c.Audit = ac.logger(); c.Token = "tok-real" })
	base := serveHandler(t, h)

	do(t, "POST", base+"/mcp", "application/json", `{}`, map[string]string{"User-Agent": "scanner/1.0", "X-Forwarded-For": "203.0.113.9"})
	do(t, "GET", base+"/auth", "", "", map[string]string{"Authorization": "Bearer nope"})
	do(t, "GET", base+"/mcp", "", "", map[string]string{"Host": "evil.example"})

	denied := ac.events(t, "access_denied")
	if len(denied) != 3 {
		t.Fatalf("got %d access_denied records: %+v", len(denied), denied)
	}
	for _, d := range denied {
		if str(d, "level") != "WARN" || str(d, "remote") == "" || str(d, "method") == "" {
			t.Errorf("incomplete denial record: %+v", d)
		}
	}
	missing, invalid, host := denied[0], denied[1], denied[2]
	if str(missing, "reason") != "missing_token" || str(missing, "path") != "/mcp" || str(missing, "user_agent") != "scanner/1.0" || str(missing, "forwarded_for") != "203.0.113.9" {
		t.Errorf("missing-token record = %+v", missing)
	}
	if str(invalid, "reason") != "invalid_token" || str(invalid, "path") != "/auth" {
		t.Errorf("invalid-token record = %+v", invalid)
	}
	if str(host, "reason") != "forbidden_host" || str(host, "host") != "evil.example" {
		t.Errorf("forbidden-host record = %+v", host)
	}
}

func TestAudit_CrossOriginRequestsAreRecorded(t *testing.T) {
	t.Parallel()
	var ac auditCapture
	h := newHarness(t, func(c *Config) { c.Audit = ac.logger() })
	base := serveHandler(t, h)
	code, _, _ := do(t, "POST", base+"/auth", "application/json", `{"api_key":"x"}`, map[string]string{"Origin": "http://evil.example", "Sec-Fetch-Site": "cross-site"})
	if code != http.StatusForbidden {
		t.Fatalf("status = %d", code)
	}
	denied := ac.events(t, "access_denied")
	if len(denied) != 1 || str(denied[0], "reason") != "cross_origin" || str(denied[0], "origin") != "http://evil.example" || str(denied[0], "sec_fetch_site") != "cross-site" {
		t.Errorf("records = %+v", denied)
	}
}

func TestAudit_KeyChangesAreRecorded(t *testing.T) {
	t.Parallel()
	var ac auditCapture
	h := newHarness(t, func(c *Config) { c.Audit = ac.logger() })
	base := serveHandler(t, h)

	do(t, "POST", base+"/auth", "application/json", `{"api_key":"not-the-key"}`, nil)
	do(t, "POST", base+"/auth", "application/json", `{"api_key":"`+validKey+`"}`, nil)
	do(t, "POST", base+"/auth", "application/x-www-form-urlencoded", url.Values{"api_key": {validKey}}.Encode(), map[string]string{"X-Forwarded-For": "198.51.100.7"})
	do(t, "DELETE", base+"/auth", "", "", nil)

	var names []string
	for _, m := range ac.all(t) {
		names = append(names, str(m, "msg"))
		if str(m, "remote") == "" {
			t.Errorf("key event without a remote address: %+v", m)
		}
	}
	if got, want := strings.Join(names, ","), "key_rejected,key_set,key_set,key_cleared"; got != want {
		t.Errorf("events = %s, want %s", got, want)
	}
	if rejected := ac.events(t, "key_rejected"); str(rejected[0], "level") != "WARN" {
		t.Errorf("a rejected key is worth a warning: %+v", rejected[0])
	}
	if set := ac.events(t, "key_set"); str(set[1], "forwarded_for") != "198.51.100.7" {
		t.Errorf("forwarded_for = %+v", set[1])
	}
}

func TestAudit_WithoutAnAuditLoggerRecordsGoToTheOperationalLogTagged(t *testing.T) {
	t.Parallel()
	h := newHarness(t) // no Config.Audit
	h.authenticate(t)
	h.fg.route("/usersummary-service/stats/steps/daily/2026-11-10/2026-11-16", `[]`)
	callTool(t, h.connect(t), "get_steps", nil)
	log := h.log.String()
	if !strings.Contains(log, "tool_call") || !strings.Contains(log, "audit=true") || !strings.Contains(log, "tool=get_steps") {
		t.Errorf("audit records should appear in the normal log, tagged:\n%s", log)
	}
}

func TestTruncate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"exactly10!", 10, "exactly10!"},
		{"this is too long", 7, "this is…"},
		{"éééé", 5, "éé…"}, // never splits a two-byte character
		{"", 5, ""},
	} {
		if got := truncate(tc.in, tc.n); got != tc.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
	if shortHash("") != "" || len(shortHash("session")) != 8 || shortHash("a") == shortHash("b") {
		t.Error("shortHash misbehaves")
	}
}

// The SDK does not fire its connection hook on the in-memory test transport, so
// connections are checked over real streamable HTTP, which is how clients connect.
func TestAudit_ClientConnectionsAreRecordedWithAHashedSession(t *testing.T) {
	t.Parallel()
	var ac auditCapture
	h := newHarness(t, func(c *Config) { c.Audit = ac.logger() })
	base := serveHandler(t, h)

	connect := func(name string) *mcp.ClientSession {
		cs, err := mcp.NewClient(&mcp.Implementation{Name: name, Version: "9.9"}, nil).
			Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: base + "/mcp"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cs.Close() })
		return cs
	}
	a, b := connect("client-a"), connect("client-b")
	callTool(t, a, "auth_status", nil)
	callTool(t, b, "auth_status", nil)

	// The handshake notification arrives asynchronously.
	deadline := time.Now().Add(3 * time.Second)
	for len(ac.events(t, "client_connected")) < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	conns := ac.events(t, "client_connected")
	if len(conns) != 2 {
		t.Fatalf("client_connected records = %+v", conns)
	}
	sessions := map[string]string{} // client -> session hash
	for _, c := range conns {
		sessions[str(c, "client")] = str(c, "session")
	}
	if sessions["client-a/9.9"] == "" || sessions["client-a/9.9"] == sessions["client-b/9.9"] {
		t.Errorf("each connection needs its own session hash: %v", sessions)
	}
	if len(sessions["client-a/9.9"]) != 8 {
		t.Errorf("the session is logged as a short hash, not the identifier: %q", sessions["client-a/9.9"])
	}
	// The same hash ties a client's tool calls to its connection.
	for _, call := range ac.events(t, "tool_call") {
		if want := sessions[str(call, "client")]; want == "" || str(call, "session") != want {
			t.Errorf("tool_call %+v is not tied to its client's session %q", call, want)
		}
	}
}

func TestAudit_NoArgumentsMeansNoArgsField(t *testing.T) {
	t.Parallel()
	var ac auditCapture
	h := newHarness(t, func(c *Config) { c.Audit = ac.logger() })
	callTool(t, h.connect(t), "auth_status", nil)
	calls := ac.events(t, "tool_call")
	if len(calls) != 1 {
		t.Fatalf("records = %+v", calls)
	}
	if _, present := calls[0]["args"]; present {
		t.Errorf("empty arguments should not be logged as %q", calls[0]["args"])
	}
}

// deniedRequest is a refused request from addr, for driving a denyAuditor.
func deniedRequest(addr string) *http.Request {
	r, _ := http.NewRequest("GET", "http://127.0.0.1/mcp", nil)
	r.RemoteAddr = addr
	return r
}

func TestDenyAuditor_ThrottlesEachSource(t *testing.T) {
	t.Parallel()
	var ac auditCapture
	now := fixedNow
	d := newDenyAuditor(ac.logger(), func() time.Time { return now })

	for range deniedBurst + 5 {
		d.record(deniedRequest("203.0.113.1:1000"), "invalid_token")
	}
	// Another address has its own budget.
	d.record(deniedRequest("203.0.113.2:1000"), "invalid_token")

	if n := len(ac.events(t, "access_denied")); n != deniedBurst+1 {
		t.Errorf("access_denied records = %d, want %d (the burst plus the other address)", n, deniedBurst+1)
	}
	thr := ac.events(t, "access_denied_throttled")
	if len(thr) != 1 || str(thr[0], "remote") != "203.0.113.1:1000" || str(thr[0], "resumes_at") == "" {
		t.Fatalf("throttled records = %+v", thr)
	}

	// After the window, recording resumes and reports what was left out.
	now = now.Add(deniedWindow)
	d.record(deniedRequest("203.0.113.1:2000"), "invalid_token")
	all := ac.events(t, "access_denied")
	last := all[len(all)-1]
	if got, _ := last["previously_unrecorded"].(float64); got != 5 {
		t.Errorf("previously_unrecorded = %v, want 5 (%+v)", last["previously_unrecorded"], last)
	}
}

func TestDenyAuditor_TracksABoundedNumberOfSources(t *testing.T) {
	t.Parallel()
	var ac auditCapture
	d := newDenyAuditor(ac.logger(), func() time.Time { return fixedNow })
	for i := range deniedMaxSources + 50 {
		d.record(deniedRequest(fmt.Sprintf("10.%d.%d.%d:1", i>>16&255, i>>8&255, i&255)), "invalid_token")
	}
	if n := len(d.sources); n > deniedMaxSources+1 {
		t.Errorf("tracked sources = %d, want at most %d", n, deniedMaxSources+1)
	}
}

func TestAudit_RefusalFloodIsThrottledEndToEnd(t *testing.T) {
	t.Parallel()
	var ac auditCapture
	h := newHarness(t, func(c *Config) { c.Audit = ac.logger(); c.Token = "tok-real-0123456789" })
	base := serveHandler(t, h)
	for range deniedBurst * 2 {
		do(t, "POST", base+"/mcp", "application/json", `{}`, nil)
	}
	if n := len(ac.events(t, "access_denied")); n != deniedBurst {
		t.Errorf("access_denied records = %d, want %d", n, deniedBurst)
	}
	if n := len(ac.events(t, "access_denied_throttled")); n != 1 {
		t.Errorf("access_denied_throttled records = %d, want 1", n)
	}
}
