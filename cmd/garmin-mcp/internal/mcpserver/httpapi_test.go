package mcpserver

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// do sends a request and returns status, body and headers.
func do(t *testing.T, method, url, contentType, body string, hdr map[string]string) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range hdr {
		if strings.EqualFold(k, "Host") {
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
	return resp.StatusCode, string(b), resp.Header
}

func serveHandler(t *testing.T, h *harness) string {
	t.Helper()
	ts := httptest.NewServer(h.srv.Handler())
	t.Cleanup(ts.Close)
	return ts.URL
}

// The key must never appear in any response, header or log line.
func TestKeyNeverLeaks(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fg.route("/usersummary-service/stats/steps/daily/2026-11-10/2026-11-16", `[{"calendarDate":"2026-11-16","totalSteps":1}]`)
	base := serveHandler(t, h)

	var seen strings.Builder
	record := func(body string, hdr http.Header) {
		seen.WriteString(body)
		for k, v := range hdr {
			fmt.Fprintf(&seen, "%s: %s\n", k, strings.Join(v, ","))
		}
	}
	for _, p := range []string{"/", "/healthz", "/mcp"} {
		_, body, hdr := do(t, "GET", base+p, "", "", nil)
		record(body, hdr)
	}
	cs := h.connect(t)
	record(resultText(callTool(t, cs, "get_steps", nil)), nil)
	record(resultText(callTool(t, cs, "get_steps", map[string]any{"start": "nonsense"})), nil)

	if strings.Contains(seen.String(), validKey) {
		t.Error("a response or header contains the key")
	}
	if strings.Contains(h.log.String(), validKey) {
		t.Errorf("the log contains the key:\n%s", h.log.String())
	}
}

func TestBearerToken_ProtectsMCP(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *Config) { c.Token = "s3cret-token" })
	base := serveHandler(t, h)

	for name, hdr := range map[string]map[string]string{
		"no header":    nil,
		"wrong token":  {"Authorization": "Bearer nope"},
		"wrong scheme": {"Authorization": "Basic s3cret-token"},
		"token only":   {"Authorization": "s3cret-token"},
	} {
		code, _, rh := do(t, "GET", base+"/mcp", "", "", hdr)
		if code != http.StatusUnauthorized || rh.Get("WWW-Authenticate") == "" {
			t.Errorf("/mcp with %s: %d, WWW-Authenticate=%q", name, code, rh.Get("WWW-Authenticate"))
		}
	}
	if code, _, _ := do(t, "GET", base+"/mcp", "", "", map[string]string{"Authorization": "Bearer s3cret-token"}); code == http.StatusUnauthorized {
		t.Errorf("valid token on /mcp: %d", code)
	}
	if code, _, _ := do(t, "GET", base+"/healthz", "", "", nil); code != 200 {
		t.Errorf("/healthz must stay open: %d", code)
	}
}

func TestHostGuard_BlocksDNSRebinding(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	base := serveHandler(t, h)
	for _, host := range []string{"evil.example", "evil.example:8808", "169.254.169.254", "localhost.evil.example"} {
		for _, p := range []string{"/", "/mcp"} {
			if code, _, _ := do(t, "GET", base+p, "", "", map[string]string{"Host": host}); code != http.StatusForbidden {
				t.Errorf("Host %q on %s: %d, want 403", host, p, code)
			}
		}
	}
	for _, host := range []string{"localhost", "localhost:8808", "127.0.0.1:1234", "[::1]:8808", "LOCALHOST"} {
		if code, _, _ := do(t, "GET", base+"/healthz", "", "", map[string]string{"Host": host}); code != 200 {
			t.Errorf("Host %q: %d, want 200", host, code)
		}
	}
}

func TestHostGuard_NotAppliedWhenBoundToAllInterfaces(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *Config) { c.Addr = "0.0.0.0:8808"; c.Token = "tok" })
	base := serveHandler(t, h)
	if code, _, _ := do(t, "GET", base+"/healthz", "", "", map[string]string{"Host": "my-server.lan"}); code != 200 {
		t.Errorf("a LAN hostname must be accepted when the token protects the server: %d", code)
	}
}

func TestCrossOriginRequestsAreRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	base := serveHandler(t, h)
	const body = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`

	for name, hdr := range map[string]map[string]string{
		"foreign Origin":   {"Origin": "http://evil.example"},
		"cross-site fetch": {"Sec-Fetch-Site": "cross-site"},
		"same-site fetch":  {"Sec-Fetch-Site": "same-site"},
	} {
		if code, _, _ := do(t, "POST", base+"/mcp", "application/json", body, hdr); code != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", name, code)
		}
	}
	// Same-origin browser requests, and plain API clients, get through.
	u, _ := url.Parse(base)
	if code, _, _ := do(t, "POST", base+"/mcp", "application/json", body, map[string]string{"Origin": "http://" + u.Host, "Sec-Fetch-Site": "same-origin"}); code == http.StatusForbidden {
		t.Errorf("same-origin: %d", code)
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	t.Parallel()
	for addr, want := range map[string]bool{
		"127.0.0.1:8808": true, "127.5.5.5:1": true, "localhost:80": true, "LocalHost:80": true, "[::1]:80": true,
		":8808": false, "0.0.0.0:80": false, "[::]:80": false, "192.168.1.5:80": false, "10.0.0.1:80": false,
		"example.com:80": false, "garbage": false, "": false,
	} {
		if got := IsLoopbackAddr(addr); got != want {
			t.Errorf("IsLoopbackAddr(%q) = %v, want %v", addr, got, want)
		}
	}
}

// End to end over real HTTP: an MCP client connects to /mcp and calls a tool.
func TestMCPOverHTTP(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fg.route("/usersummary-service/stats/steps/daily/2026-11-10/2026-11-16", `[{"calendarDate":"2026-11-16","totalSteps":1234,"stepGoal":8000,"totalDistance":900.4}]`)
	base := serveHandler(t, h)

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).
		Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: base + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	type steps struct {
		Count int `json:"count"`
		Days  []struct {
			Steps int `json:"steps"`
		} `json:"days"`
	}
	out := decodeResult[steps](t, callTool(t, cs, "get_steps", nil))
	if out.Count != 1 || out.Days[0].Steps != 1234 {
		t.Errorf("get_steps: %+v", out)
	}
}

func TestMCPOverHTTP_RequiresTheBearerToken(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *Config) { c.Token = "tok-123" })
	base := serveHandler(t, h)

	_, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).
		Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: base + "/mcp"}, nil)
	if err == nil {
		t.Fatal("connecting without the token must fail")
	}
	hc := &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", "Bearer tok-123")
		return http.DefaultTransport.RoundTrip(r)
	})}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).
		Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: base + "/mcp", HTTPClient: hc}, nil)
	if err != nil {
		t.Fatalf("connecting with the token: %v", err)
	}
	defer cs.Close()
	if _, err := cs.ListTools(context.Background(), nil); err != nil {
		t.Errorf("listing tools with the token: %v", err)
	}
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAllowedHosts_ForReverseProxies(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *Config) {
		c.Addr = "0.0.0.0:8808"
		c.Token = "tok"
		c.AllowedHosts = []string{"Garmin.Example.com", " other.example.org:8443 ", ""}
	})
	base := serveHandler(t, h)
	for host, want := range map[string]int{
		"garmin.example.com": 401, "garmin.example.com:443": 401, "GARMIN.EXAMPLE.COM": 401,
		"other.example.org": 401, "other.example.org:8443": 401,
		"localhost": 401, "127.0.0.1:8808": 401,
		"evil.example": 403, "garmin.example.com.evil.example": 403, "example.com": 403,
	} {
		if code, _, _ := do(t, "GET", base+"/mcp", "", "", map[string]string{"Host": host}); code != want {
			t.Errorf("Host %q -> %d, want %d", host, code, want)
		}
	}
	// The hostname check does not replace the token.
	if code, _, _ := do(t, "GET", base+"/mcp", "", "", map[string]string{"Host": "garmin.example.com"}); code != http.StatusUnauthorized {
		t.Errorf("an allowed host without the token: %d, want 401", code)
	}
}

func TestAllowedHosts_AlsoApplyOnLoopbackBinds(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *Config) { c.AllowedHosts = []string{"garmin.example.com"} })
	base := serveHandler(t, h)
	if code, _, _ := do(t, "GET", base+"/", "", "", map[string]string{"Host": "garmin.example.com"}); code != 404 {
		t.Errorf("the public name must be allowed in addition to loopback: %d, want 404 (\"/\" serves nothing)", code)
	}
	if code, _, _ := do(t, "GET", base+"/", "", "", map[string]string{"Host": "evil.example"}); code != 403 {
		t.Errorf("other names stay blocked: %d", code)
	}
}

func TestAllowedHosts_RejectMalformedEntries(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for _, bad := range []string{"https://garmin.example.com", "garmin.example.com/mcp", "user@garmin.example.com", "garmin example.com"} {
		if _, err := New(Config{API: h.srv.api, AllowedHosts: []string{bad}}); err == nil || !strings.Contains(err.Error(), "hostname") {
			t.Errorf("%q: err = %v, want a rejection", bad, err)
		}
	}
	if _, err := New(Config{API: h.srv.api, AllowedHosts: []string{"", "  "}}); err != nil {
		t.Errorf("blank entries are ignored: %v", err)
	}
}

// Platform health checks send whatever Host header the platform likes. The check
// answers only "ok", so it is exempt from the Host guard on every kind of bind.
func TestHealthzIgnoresTheHostHeader(t *testing.T) {
	t.Parallel()
	for name, opts := range map[string]func(*Config){
		"loopback bind":            nil,
		"allowed hosts configured": func(c *Config) { c.AllowedHosts = []string{"garmin.example.com"} },
		"exposed bind with a token": func(c *Config) {
			c.Addr = "0.0.0.0:8808"
			c.Token = "tok"
			c.AllowedHosts = []string{"garmin.example.com"}
		},
	} {
		var o []harnessOpt
		if opts != nil {
			o = append(o, opts)
		}
		base := serveHandler(t, newHarness(t, o...))
		for _, host := range []string{"10.0.0.7:8808", "fdaa:0:1::2", "evil.example", "localhost", ""} {
			code, body, _ := do(t, "GET", base+"/healthz", "", "", map[string]string{"Host": host})
			if code != 200 || strings.TrimSpace(body) != "ok" {
				t.Errorf("%s: Host %q -> %d %q, want 200 ok", name, host, code, body)
			}
		}
		// ... and nothing else is exempted by accident.
		if code, _, _ := do(t, "GET", base+"/mcp", "", "", map[string]string{"Host": "evil.example"}); code != 403 {
			t.Errorf("%s: another path let a foreign Host through: %d", name, code)
		}
	}
}

func TestBearerToken(t *testing.T) {
	t.Parallel()
	for h, want := range map[string]string{
		"Bearer abc":    "abc",
		"bearer abc":    "abc", // the scheme is case-insensitive (RFC 7235)
		"BEARER abc":    "abc",
		"Bearer   abc ": "abc",
		" Bearer abc":   "abc",
		"Bearer":        "",
		"Bearer ":       "",
		"Basic abc":     "",
		"":              "",
		"Bearerabc":     "",
	} {
		got, ok := bearerToken(h)
		if got != want || ok != (want != "") {
			t.Errorf("bearerToken(%q) = %q, %v; want %q", h, got, ok, want)
		}
	}
}

func TestRequireToken_AcceptsAnyCaseOfTheScheme(t *testing.T) {
	t.Parallel()
	const token = "tok-real-0123456789"
	h := newHarness(t, func(c *Config) { c.Token = token })
	base := serveHandler(t, h)
	for _, hdr := range []string{"Bearer " + token, "bearer " + token, "Bearer  " + token} {
		if code, _, _ := do(t, "GET", base+"/mcp", "", "", map[string]string{"Authorization": hdr}); code == http.StatusUnauthorized {
			t.Errorf("%q: status %d, want the token accepted", hdr, code)
		}
	}
	if code, _, _ := do(t, "GET", base+"/mcp", "", "", map[string]string{"Authorization": "Bearer " + token + "x"}); code != http.StatusUnauthorized {
		t.Errorf("a wrong token: status %d, want 401", code)
	}
}
