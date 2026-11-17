package mcpserver

import (
	"context"
	"encoding/json"
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

type authJSON struct {
	Authenticated  bool   `json:"authenticated"`
	TokenExpiresAt string `json:"token_expires_at"`
	Persisted      bool   `json:"persisted"`
	Message        string `json:"message"`
}

func TestAuthEndpoint_ProvideKeyStatusAndRemove(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	base := serveHandler(t, h)

	code, body, hdr := do(t, "GET", base+"/auth", "", "", nil)
	var st authJSON
	_ = json.Unmarshal([]byte(body), &st)
	if code != 200 || st.Authenticated {
		t.Fatalf("initial status: %d %s", code, body)
	}
	if hdr.Get("Cache-Control") != "no-store" || hdr.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("headers = %v", hdr)
	}

	code, body, _ = do(t, "POST", base+"/auth", "application/json", `{"api_key":"`+validKey+`"}`, nil)
	_ = json.Unmarshal([]byte(body), &st)
	if code != 200 || !st.Authenticated || st.TokenExpiresAt == "" {
		t.Fatalf("POST valid key: %d %s", code, body)
	}
	if !h.creds.Status().Authenticated {
		t.Error("credentials not updated")
	}

	code, body, _ = do(t, "GET", base+"/auth", "", "", nil)
	_ = json.Unmarshal([]byte(body), &st)
	if code != 200 || !st.Authenticated {
		t.Errorf("status after: %d %s", code, body)
	}

	code, body, _ = do(t, "DELETE", base+"/auth", "", "", nil)
	_ = json.Unmarshal([]byte(body), &st)
	if code != 200 || st.Authenticated || h.creds.Status().Authenticated {
		t.Errorf("DELETE: %d %s", code, body)
	}
}

func TestAuthEndpoint_Rejections(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	base := serveHandler(t, h)
	tests := []struct {
		name, ctype, body string
		want              int
	}{
		{"key Garmin rejects", "application/json", `{"api_key":"not-the-key"}`, http.StatusUnauthorized},
		{"empty key", "application/json", `{"api_key":"  "}`, http.StatusBadRequest},
		{"missing field", "application/json", `{}`, http.StatusBadRequest},
		{"malformed JSON", "application/json", `{"api_key":`, http.StatusBadRequest},
		{"wrong content type", "text/plain", validKey, http.StatusUnsupportedMediaType},
		{"no content type", "", `{"api_key":"x"}`, http.StatusUnsupportedMediaType},
		{"oversized body", "application/json", `{"api_key":"` + strings.Repeat("a", 10000) + `"}`, http.StatusRequestEntityTooLarge},
		{"oversized form", "application/x-www-form-urlencoded", "api_key=" + strings.Repeat("a", 10000), http.StatusRequestEntityTooLarge},
		// A valid key followed by more data is still a malformed request.
		{"second JSON value", "application/json", `{"api_key":"` + validKey + `"} {"api_key":"x"}`, http.StatusBadRequest},
		{"trailing garbage", "application/json", `{"api_key":"` + validKey + `"}xyz`, http.StatusBadRequest},
	}
	for _, tc := range tests {
		code, body, _ := do(t, "POST", base+"/auth", tc.ctype, tc.body, nil)
		if code != tc.want {
			t.Errorf("%s: status %d (%s), want %d", tc.name, code, body, tc.want)
		}
		if h.creds.Status().Authenticated {
			t.Fatalf("%s: a bad request authenticated the server", tc.name)
		}
	}
	if code, _, hdr := do(t, "PUT", base+"/auth", "", "", nil); code != http.StatusMethodNotAllowed || hdr.Get("Allow") == "" {
		t.Errorf("PUT: %d allow=%q", code, hdr.Get("Allow"))
	}
}

func TestAuthEndpoint_FormFlowFromTheKeyPage(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	base := serveHandler(t, h)

	code, page, hdr := do(t, "GET", base+"/", "", "", nil)
	if code != 200 || !strings.Contains(page, `<form method="post" action="/auth">`) || !strings.Contains(page, `type="password"`) {
		t.Fatalf("key page: %d %s", code, page)
	}
	if !strings.HasPrefix(hdr.Get("Content-Type"), "text/html") || !strings.Contains(hdr.Get("Content-Security-Policy"), "default-src 'none'") {
		t.Errorf("page headers = %v", hdr)
	}
	if code, _, _ := do(t, "GET", base+"/nope", "", "", nil); code != 404 {
		t.Errorf("unknown path: %d", code)
	}
	if code, _, _ := do(t, "POST", base+"/", "application/x-www-form-urlencoded", "", nil); code != http.StatusMethodNotAllowed {
		t.Errorf("POST /: %d", code)
	}

	bad := url.Values{"api_key": {"nope-nope"}}.Encode()
	code, page, _ = do(t, "POST", base+"/auth", "application/x-www-form-urlencoded", bad, nil)
	if code != http.StatusUnauthorized || !strings.Contains(page, "Garmin rejected") || strings.Contains(page, "nope-nope") {
		t.Errorf("bad form key: %d %s", code, page)
	}

	good := url.Values{"api_key": {validKey}}.Encode()
	code, page, _ = do(t, "POST", base+"/auth", "application/x-www-form-urlencoded", good, nil)
	if code != 200 || !strings.Contains(page, "authenticated") || strings.Contains(page, validKey) || !h.creds.Status().Authenticated {
		t.Errorf("good form key: %d %s", code, page)
	}
}

// The key must never be echoed in any response, header or log line, whichever
// path the request took.
func TestAuthEndpoint_KeyNeverLeaks(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	base := serveHandler(t, h)
	const badKey = "wrong-key-SECRETVALUE-987"

	var seen strings.Builder
	record := func(body string, hdr http.Header) {
		seen.WriteString(body)
		for k, v := range hdr {
			fmt.Fprintf(&seen, "%s: %s\n", k, strings.Join(v, ","))
		}
	}
	for _, step := range []struct{ method, ctype, body string }{
		{"POST", "application/json", `{"api_key":"` + badKey + `"}`},
		{"POST", "application/json", `{"api_key":"` + validKey + `"}`},
		{"POST", "application/x-www-form-urlencoded", url.Values{"api_key": {badKey}}.Encode()},
		{"POST", "application/x-www-form-urlencoded", url.Values{"api_key": {validKey}}.Encode()},
		{"POST", "application/json", `{"api_key":` + `"` + badKey}, // malformed
		{"GET", "", ""},
		{"DELETE", "", ""},
	} {
		_, body, hdr := do(t, step.method, base+"/auth", step.ctype, step.body, nil)
		record(body, hdr)
	}
	for _, f := range []string{"/", "/healthz"} {
		_, body, hdr := do(t, "GET", base+f, "", "", nil)
		record(body, hdr)
	}
	// And through tool results.
	h.authenticate(t)
	cs := h.connect(t)
	record(resultText(callTool(t, cs, "auth_status", nil)), nil)
	record(resultText(callTool(t, cs, "get_steps", map[string]any{"start": "nonsense"})), nil)

	for _, secret := range []string{badKey, validKey, "SECRETVALUE"} {
		if strings.Contains(seen.String(), secret) {
			t.Errorf("a response or header contains the key %q", secret)
		}
		if strings.Contains(h.log.String(), secret) {
			t.Errorf("the log contains the key %q:\n%s", secret, h.log.String())
		}
	}
	if !strings.Contains(h.log.String(), "API key accepted") {
		t.Errorf("expected an acceptance log line, got:\n%s", h.log.String())
	}
}

func TestBearerToken_ProtectsMCPAndAuth(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *Config) { c.Token = "s3cret-token" })
	base := serveHandler(t, h)

	for _, p := range []string{"/auth", "/mcp"} {
		for name, hdr := range map[string]map[string]string{
			"no header":    nil,
			"wrong token":  {"Authorization": "Bearer nope"},
			"wrong scheme": {"Authorization": "Basic s3cret-token"},
			"token only":   {"Authorization": "s3cret-token"},
		} {
			code, _, rh := do(t, "GET", base+p, "", "", hdr)
			if code != http.StatusUnauthorized || rh.Get("WWW-Authenticate") == "" {
				t.Errorf("%s with %s: %d, WWW-Authenticate=%q", p, name, code, rh.Get("WWW-Authenticate"))
			}
		}
	}
	// A rejected request must not have reached the handler.
	if code, _, _ := do(t, "POST", base+"/auth", "application/json", `{"api_key":"`+validKey+`"}`, nil); code != 401 || h.creds.Status().Authenticated {
		t.Error("an unauthenticated POST /auth got through")
	}
	if code, _, _ := do(t, "GET", base+"/auth", "", "", map[string]string{"Authorization": "Bearer s3cret-token"}); code != 200 {
		t.Errorf("valid token on /auth: %d", code)
	}
	if code, _, _ := do(t, "POST", base+"/auth", "application/json", `{"api_key":"`+validKey+`"}`, map[string]string{"Authorization": "Bearer s3cret-token"}); code != 200 {
		t.Errorf("valid token POST: %d", code)
	}
	if code, _, _ := do(t, "GET", base+"/healthz", "", "", nil); code != 200 {
		t.Errorf("/healthz must stay open: %d", code)
	}
	// The key page cannot send a bearer header, so it is not served here.
	if code, _, _ := do(t, "GET", base+"/", "", "", nil); code != 404 {
		t.Errorf("key page with a token configured: %d, want 404", code)
	}
}

func TestHostGuard_BlocksDNSRebinding(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	base := serveHandler(t, h)
	for _, host := range []string{"evil.example", "evil.example:8808", "169.254.169.254", "localhost.evil.example"} {
		for _, p := range []string{"/", "/auth", "/mcp"} {
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
	body := `{"api_key":"` + validKey + `"}`

	for name, hdr := range map[string]map[string]string{
		"foreign Origin":           {"Origin": "http://evil.example"},
		"cross-site fetch":         {"Sec-Fetch-Site": "cross-site"},
		"same-site fetch":          {"Sec-Fetch-Site": "same-site"},
		"foreign Origin on DELETE": {"Origin": "https://attacker.example", "X-Method": "DELETE"},
	} {
		method := "POST"
		if hdr["X-Method"] == "DELETE" {
			method = "DELETE"
			delete(hdr, "X-Method")
		}
		if code, _, _ := do(t, method, base+"/auth", "application/json", body, hdr); code != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", name, code)
		}
	}
	if h.creds.Status().Authenticated {
		t.Fatal("a cross-origin request replaced the key")
	}
	// Same-origin browser requests, and plain API clients, still work.
	u, _ := url.Parse(base)
	if code, _, _ := do(t, "POST", base+"/auth", "application/json", body, map[string]string{"Origin": "http://" + u.Host, "Sec-Fetch-Site": "same-origin"}); code != 200 {
		t.Errorf("same-origin: %d", code)
	}
	if code, _, _ := do(t, "GET", base+"/auth", "", "", map[string]string{"Origin": "http://evil.example"}); code != 200 {
		t.Errorf("safe GET must not be blocked: %d", code)
	}
}

func TestNew_RefusesExposedServerWithoutToken(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	base := Config{API: h.srv.api, Credentials: h.creds}
	for _, addr := range []string{"0.0.0.0:8808", ":8808", "192.168.1.5:8808", "example.com:80", "[::]:8808"} {
		cfg := base
		cfg.Addr = addr
		if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "token") {
			t.Errorf("Addr %q without token: err = %v, want a refusal", addr, err)
		}
		cfg.Token = "tok"
		if _, err := New(cfg); err != nil {
			t.Errorf("Addr %q with token: %v", addr, err)
		}
	}
	for _, addr := range []string{"", "127.0.0.1:8808", "localhost:8808", "[::1]:8808"} {
		cfg := base
		cfg.Addr = addr
		if _, err := New(cfg); err != nil {
			t.Errorf("loopback %q should be allowed without a token: %v", addr, err)
		}
	}
	if _, err := New(Config{}); err == nil {
		t.Error("missing API and Credentials must be an error")
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

// End to end over real HTTP: an MCP client connects to /mcp, calls a tool
// before the key is provided, provides it at /auth, and calls again.
func TestMCPOverHTTP_AuthFlow(t *testing.T) {
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

	res := callTool(t, cs, "get_steps", nil)
	if !res.IsError || !strings.Contains(resultText(res), "API key") || !strings.Contains(resultText(res), "/auth") {
		t.Fatalf("before auth: IsError=%v text=%q", res.IsError, resultText(res))
	}
	if code, body, _ := do(t, "POST", base+"/auth", "application/json", `{"api_key":"`+validKey+`"}`, nil); code != 200 {
		t.Fatalf("auth: %d %s", code, body)
	}
	type steps struct {
		Count int `json:"count"`
		Days  []struct {
			Steps int `json:"steps"`
		} `json:"days"`
	}
	out := decodeResult[steps](t, callTool(t, cs, "get_steps", nil))
	if out.Count != 1 || out.Days[0].Steps != 1234 {
		t.Errorf("after auth: %+v", out)
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
	if res := callTool(t, cs, "auth_status", nil); res.IsError {
		t.Errorf("auth_status: %s", resultText(res))
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
		if code, _, _ := do(t, "GET", base+"/auth", "", "", map[string]string{"Host": host}); code != want {
			t.Errorf("Host %q -> %d, want %d", host, code, want)
		}
	}
	// The hostname check does not replace the token.
	if code, _, _ := do(t, "GET", base+"/auth", "", "", map[string]string{"Host": "garmin.example.com"}); code != http.StatusUnauthorized {
		t.Errorf("an allowed host without the token: %d, want 401", code)
	}
}

func TestAllowedHosts_AlsoApplyOnLoopbackBinds(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *Config) { c.AllowedHosts = []string{"garmin.example.com"} })
	base := serveHandler(t, h)
	if code, _, _ := do(t, "GET", base+"/auth", "", "", map[string]string{"Host": "garmin.example.com"}); code != 200 {
		t.Errorf("the public name must be allowed in addition to loopback: %d", code)
	}
	if code, _, _ := do(t, "GET", base+"/auth", "", "", map[string]string{"Host": "evil.example"}); code != 403 {
		t.Errorf("other names stay blocked: %d", code)
	}
}

func TestAllowedHosts_RejectMalformedEntries(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for _, bad := range []string{"https://garmin.example.com", "garmin.example.com/mcp", "user@garmin.example.com", "garmin example.com"} {
		if _, err := New(Config{API: h.srv.api, Credentials: h.creds, AllowedHosts: []string{bad}}); err == nil || !strings.Contains(err.Error(), "hostname") {
			t.Errorf("%q: err = %v, want a rejection", bad, err)
		}
	}
	if _, err := New(Config{API: h.srv.api, Credentials: h.creds, AllowedHosts: []string{"", "  "}}); err != nil {
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
		if code, _, _ := do(t, "GET", base+"/auth", "", "", map[string]string{"Host": "evil.example"}); code != 403 && code != 404 {
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
		if code, _, _ := do(t, "GET", base+"/auth", "", "", map[string]string{"Authorization": hdr}); code != http.StatusOK {
			t.Errorf("%q: status %d, want 200", hdr, code)
		}
	}
	if code, _, _ := do(t, "GET", base+"/auth", "", "", map[string]string{"Authorization": "Bearer " + token + "x"}); code != http.StatusUnauthorized {
		t.Errorf("a wrong token: status %d, want 401", code)
	}
}
