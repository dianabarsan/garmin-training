package mcpserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	garmin "github.com/dianabarsan/garmin-training"
)

const multiAdminToken = "admin-token-0123456789abcdef"

// --- a fake Garmin that tells users apart -----------------------------------------

// multiGarmin issues a token per API key, and answers data requests according to
// which key's token made them, so a mix-up between users cannot go unnoticed.
type multiGarmin struct {
	srv *httptest.Server

	mu      sync.Mutex
	labelOf map[string]string // api key -> user label
	steps   map[string]int    // api key -> steps it should see
	revoked map[string]bool   // api key -> rejected everywhere (an expired key)
	calls   []string          // "<label> <path>" for every data request
}

func makeJWTSub(sub string) string {
	enc := base64.RawURLEncoding.EncodeToString
	payload := fmt.Sprintf(`{"exp":%d,"sub":%q}`, time.Now().Add(24*time.Hour).Unix(), sub)
	return enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(payload)) + ".sig"
}

func jwtSub(jwt string) string {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return ""
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var c struct {
		Sub string `json:"sub"`
	}
	_ = json.Unmarshal(b, &c)
	return c.Sub
}

func newMultiGarmin(t *testing.T) *multiGarmin {
	t.Helper()
	g := &multiGarmin{labelOf: map[string]string{}, steps: map[string]int{}, revoked: map[string]bool{}}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		if r.URL.Path == tokenPath {
			key := strings.TrimPrefix(r.Header.Get("Authorization"), "ApiKey ")
			if _, ok := g.labelOf[key]; !ok || g.revoked[key] {
				http.Error(w, "no", http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(makeJWTSub(key)))
			return
		}
		sub := jwtSub(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		label, ok := g.labelOf[sub]
		if !ok || g.revoked[sub] {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		g.calls = append(g.calls, label+" "+r.URL.Path)
		if strings.HasPrefix(r.URL.Path, "/usersummary-service/stats/steps/daily/") {
			_, _ = fmt.Fprintf(w, `[{"calendarDate":"2026-11-15","totalSteps":%d,"stepGoal":8000,"totalDistance":1}]`, g.steps[sub])
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *multiGarmin) addKey(key, label string, steps int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.labelOf[key], g.steps[key] = label, steps
}

func (g *multiGarmin) revoke(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.revoked[key] = true
}

func (g *multiGarmin) callsBy(label string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, c := range g.calls {
		if strings.HasPrefix(c, label+" ") {
			n++
		}
	}
	return n
}

// --- harness --------------------------------------------------------------------------

type multiHarness struct {
	g         *multiGarmin
	usersPath string
	master    []byte
	store     *UserStore
	srv       *MultiServer
	ts        *httptest.Server
	ops       recordingLog
	audit     auditCapture
}

func newMultiHarness(t *testing.T) *multiHarness {
	t.Helper()
	h := &multiHarness{g: newMultiGarmin(t), usersPath: filepath.Join(t.TempDir(), "users.json"), master: testMasterKey(t)}
	h.g.addKey("alice-key", "alice", 1111)
	h.g.addKey("bob-key", "bob", 2222)
	h.start(t)
	return h
}

// start (re)builds the server from the users file, like a restart would.
func (h *multiHarness) start(t *testing.T) {
	t.Helper()
	if h.ts != nil {
		h.ts.Close()
	}
	h.store = openStore(t, h.usersPath, h.master)
	srv, err := NewMulti(MultiConfig{
		Users:       h.store,
		AdminToken:  multiAdminToken,
		AuthBaseURL: h.g.srv.URL,
		Logger:      h.ops.logger(),
		Audit:       h.audit.logger(),
		Now:         func() time.Time { return fixedNow },
		NewClient: func(ts garmin.TokenSource) *garmin.Client {
			return garmin.New(garmin.WithBaseURL(h.g.srv.URL), garmin.WithTokenSource(ts), garmin.WithRetry(0, 0, 0))
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.srv = srv
	h.ts = httptest.NewServer(srv.Handler())
	t.Cleanup(h.ts.Close)
}

func (h *multiHarness) admin(t *testing.T, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var payload string
	if body != nil {
		b, _ := json.Marshal(body)
		payload = string(b)
	}
	code, resp, _ := do(t, method, h.ts.URL+path, "application/json", payload, map[string]string{"Authorization": "Bearer " + multiAdminToken})
	var m map[string]any
	_ = json.Unmarshal([]byte(resp), &m)
	return code, m
}

func (h *multiHarness) register(t *testing.T, name, key string) (id, token string) {
	t.Helper()
	code, m := h.admin(t, "POST", "/admin/users", map[string]string{"name": name, "api_key": key})
	if code != http.StatusCreated {
		t.Fatalf("register %s: HTTP %d %v", name, code, m)
	}
	return str(m, "id"), str(m, "token")
}

func (h *multiHarness) client(t *testing.T, token string) (*mcp.ClientSession, error) {
	t.Helper()
	hc := &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", "Bearer "+token)
		return http.DefaultTransport.RoundTrip(r)
	})}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "multi-test", Version: "1"}, nil).
		Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: h.ts.URL + "/mcp", HTTPClient: hc}, nil)
	if err == nil {
		t.Cleanup(func() { _ = cs.Close() })
	}
	return cs, err
}

// stepsFor connects with token and returns the steps the server reports.
func (h *multiHarness) stepsFor(t *testing.T, token string) int {
	t.Helper()
	cs, err := h.client(t, token)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	out := decodeResult[dayList[struct {
		Steps int `json:"steps"`
	}]](t, callTool(t, cs, "get_steps", nil))
	if out.Count != 1 {
		t.Fatalf("get_steps returned %+v", out)
	}
	return out.Days[0].Steps
}

// --- tests ------------------------------------------------------------------------------

func TestMulti_RegisterThenUse(t *testing.T) {
	t.Parallel()
	h := newMultiHarness(t)

	code, m := h.admin(t, "POST", "/admin/users", map[string]string{"name": "alice", "api_key": "alice-key"})
	if code != http.StatusCreated {
		t.Fatalf("register: HTTP %d %v", code, m)
	}
	token := str(m, "token")
	if !strings.HasPrefix(token, tokenPrefix) || !strings.HasPrefix(str(m, "id"), "u_") || str(m, "name") != "alice" || str(m, "created_at") == "" {
		t.Errorf("response = %v", m)
	}
	if !strings.Contains(str(m, "note"), "once") {
		t.Errorf("the response should say the token is shown once: %v", m)
	}
	if got := h.stepsFor(t, token); got != 1111 {
		t.Errorf("alice sees %d steps, want 1111", got)
	}
	cs, _ := h.client(t, token)
	if tools, err := cs.ListTools(context.Background(), nil); err != nil || len(tools.Tools) != 14 {
		t.Errorf("tools = %v, %v", tools, err)
	}
	if h.srv.UserCount() != 1 {
		t.Errorf("UserCount = %d", h.srv.UserCount())
	}
}

func TestMulti_ListShowsNoSecrets(t *testing.T) {
	t.Parallel()
	h := newMultiHarness(t)
	_, ta := h.register(t, "alice", "alice-key")
	h.register(t, "bob", "bob-key")

	code, body, _ := do(t, "GET", h.ts.URL+"/admin/users", "", "", map[string]string{"Authorization": "Bearer " + multiAdminToken})
	if code != 200 {
		t.Fatalf("list: %d", code)
	}
	var out struct {
		Count int                         `json:"count"`
		Users []struct{ ID, Name string } `json:"users"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || out.Count != 2 || len(out.Users) != 2 || out.Users[0].Name != "alice" || out.Users[1].Name != "bob" {
		t.Fatalf("list = %s (%v)", body, err)
	}
	for _, secret := range []string{"alice-key", "bob-key", ta, "token", "api_key", "hash", multiAdminToken} {
		if strings.Contains(body, secret) {
			t.Errorf("the user list contains %q: %s", secret, body)
		}
	}
}

func TestMulti_UsersOnlyEverSeeTheirOwnData(t *testing.T) {
	t.Parallel()
	h := newMultiHarness(t)
	_, ta := h.register(t, "alice", "alice-key")
	_, tb := h.register(t, "bob", "bob-key")

	for i := 0; i < 3; i++ { // interleaved, repeatedly
		if got := h.stepsFor(t, ta); got != 1111 {
			t.Fatalf("round %d: alice saw %d", i, got)
		}
		if got := h.stepsFor(t, tb); got != 2222 {
			t.Fatalf("round %d: bob saw %d", i, got)
		}
	}
	// Garmin confirms it: Bob's requests were only ever made with Bob's key.
	if h.g.callsBy("alice") == 0 || h.g.callsBy("bob") == 0 {
		t.Errorf("calls: alice=%d bob=%d", h.g.callsBy("alice"), h.g.callsBy("bob"))
	}
	if n := len(h.g.calls); n != h.g.callsBy("alice")+h.g.callsBy("bob") {
		t.Errorf("some data request was made with an unknown identity (%d calls)", n)
	}
}

func rawMCP(t *testing.T, base, token, session, body string) (int, string, http.Header) {
	t.Helper()
	hdr := map[string]string{"Authorization": "Bearer " + token, "Accept": "application/json, text/event-stream"}
	if session != "" {
		hdr["Mcp-Session-Id"] = session
	}
	return do(t, "POST", base+"/mcp", "application/json", body, hdr)
}

const (
	rawInit     = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"raw","version":"1"}}}`
	rawInitDone = `{"jsonrpc":"2.0","method":"notifications/initialized"}`
	rawGetSteps = `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_steps","arguments":{}}}`
)

// One user's session id must be useless to another user, even one with a valid
// token of their own.
func TestMulti_SessionsAreNotSharedBetweenUsers(t *testing.T) {
	t.Parallel()
	h := newMultiHarness(t)
	_, ta := h.register(t, "alice", "alice-key")
	_, tb := h.register(t, "bob", "bob-key")

	code, _, hdr := rawMCP(t, h.ts.URL, ta, "", rawInit)
	sid := hdr.Get("Mcp-Session-Id")
	if code != 200 || sid == "" {
		t.Fatalf("alice initialize: %d, session %q", code, sid)
	}
	rawMCP(t, h.ts.URL, ta, sid, rawInitDone)

	code, body, _ := rawMCP(t, h.ts.URL, ta, sid, rawGetSteps)
	if code != 200 || !strings.Contains(body, `"steps":1111`) {
		t.Fatalf("alice with her own session: %d %s", code, body)
	}
	code, body, _ = rawMCP(t, h.ts.URL, tb, sid, rawGetSteps) // Bob's token, Alice's session
	if code == 200 || strings.Contains(body, "1111") {
		t.Errorf("Bob reached Alice's session: HTTP %d %s", code, body)
	}
}

func TestMulti_TokenRules(t *testing.T) {
	t.Parallel()
	h := newMultiHarness(t)
	_, ta := h.register(t, "alice", "alice-key")
	mcpBody := `{}`
	for name, tc := range map[string]struct {
		method, path, token string
		want                int
	}{
		"no token on /mcp":             {"POST", "/mcp", "", 401},
		"wrong token on /mcp":          {"POST", "/mcp", "gmcp_" + strings.Repeat("0", 64), 401},
		"admin token on /mcp":          {"POST", "/mcp", multiAdminToken, 401},
		"no token on /admin":           {"GET", "/admin/users", "", 401},
		"wrong token on /admin":        {"GET", "/admin/users", "nope", 401},
		"a user token on /admin":       {"GET", "/admin/users", ta, 401},
		"the admin token on /admin":    {"GET", "/admin/users", multiAdminToken, 200},
		"no key page in multi-user":    {"GET", "/", "", 404},
		"no single-user /auth":         {"GET", "/auth", multiAdminToken, 404},
		"healthz needs no token":       {"GET", "/healthz", "", 200},
		"unknown admin path":           {"GET", "/admin/nope", multiAdminToken, 404},
		"wrong method on /admin/users": {"PATCH", "/admin/users", multiAdminToken, 405},
	} {
		hdr := map[string]string{}
		if tc.token != "" {
			hdr["Authorization"] = "Bearer " + tc.token
		}
		if code, body, _ := do(t, tc.method, h.ts.URL+tc.path, "application/json", mcpBody, hdr); code != tc.want {
			t.Errorf("%s: %s %s -> %d (%s), want %d", name, tc.method, tc.path, code, truncate(body, 80), tc.want)
		}
	}
	if _, err := h.client(t, "gmcp_"+strings.Repeat("a", 64)); err == nil {
		t.Error("an MCP client with an unknown token must not connect")
	}
}

func TestMulti_RegistrationValidation(t *testing.T) {
	t.Parallel()
	h := newMultiHarness(t)
	h.register(t, "alice", "alice-key")
	admin := map[string]string{"Authorization": "Bearer " + multiAdminToken}

	for name, tc := range map[string]struct {
		body any
		want int
	}{
		"Garmin rejects the key": {map[string]string{"name": "carol", "api_key": "not-a-real-key"}, 422},
		"missing key":            {map[string]string{"name": "carol"}, 400},
		"blank key":              {map[string]string{"name": "carol", "api_key": "   "}, 400},
		"missing name":           {map[string]string{"api_key": "bob-key"}, 400},
		"bad name":               {map[string]string{"name": "../etc", "api_key": "bob-key"}, 400},
		"duplicate name":         {map[string]string{"name": "alice", "api_key": "bob-key"}, 409},
		"duplicate, other case":  {map[string]string{"name": "ALICE", "api_key": "bob-key"}, 409},
	} {
		if code, m := h.admin(t, "POST", "/admin/users", tc.body); code != tc.want {
			t.Errorf("%s: HTTP %d %v, want %d", name, code, m, tc.want)
		}
	}
	if code, _, _ := do(t, "POST", h.ts.URL+"/admin/users", "text/plain", `{"name":"x","api_key":"bob-key"}`, admin); code != http.StatusUnsupportedMediaType {
		t.Errorf("wrong content type: %d", code)
	}
	if code, _, _ := do(t, "POST", h.ts.URL+"/admin/users", "application/json", `{"name":`, admin); code != 400 {
		t.Errorf("malformed JSON: %d", code)
	}
	huge := `{"name":"big","api_key":"` + strings.Repeat("a", 20000) + `"}`
	if code, _, _ := do(t, "POST", h.ts.URL+"/admin/users", "application/json", huge, admin); code != 400 {
		t.Errorf("oversized body: %d", code)
	}
	if n := len(h.store.List()); n != 1 {
		t.Errorf("failed registrations created users: %d total", n)
	}
	// A rejected key is not an authenticated user, and is not stored anywhere.
	if raw, _ := os.ReadFile(h.usersPath); strings.Contains(string(raw), "carol") || strings.Contains(string(raw), "not-a-real-key") {
		t.Errorf("a rejected registration left traces in the users file")
	}
}

func TestMulti_GarminBeingUnreachableIsReportedNotStored(t *testing.T) {
	t.Parallel()
	h := newMultiHarness(t)
	h.g.srv.Close() // Garmin is down
	code, m := h.admin(t, "POST", "/admin/users", map[string]string{"name": "alice", "api_key": "alice-key"})
	if code != http.StatusBadGateway || !strings.Contains(str(m, "error"), "try again") {
		t.Errorf("HTTP %d %v", code, m)
	}
	if len(h.store.List()) != 0 {
		t.Error("a user was stored although their key could not be checked")
	}
}

func TestMulti_RemoveRevokesAtOnce(t *testing.T) {
	t.Parallel()
	h := newMultiHarness(t)
	id, ta := h.register(t, "alice", "alice-key")
	_, tb := h.register(t, "bob", "bob-key")

	_, _, hdr := rawMCP(t, h.ts.URL, ta, "", rawInit)
	sid := hdr.Get("Mcp-Session-Id")
	rawMCP(t, h.ts.URL, ta, sid, rawInitDone)
	if code, _, _ := rawMCP(t, h.ts.URL, ta, sid, rawGetSteps); code != 200 {
		t.Fatalf("before removal: %d", code)
	}

	if code, _ := h.admin(t, "DELETE", "/admin/users/"+id, nil); code != http.StatusNoContent {
		t.Fatalf("DELETE: %d", code)
	}
	if code, _, _ := rawMCP(t, h.ts.URL, ta, sid, rawGetSteps); code != 401 {
		t.Errorf("an existing session of a removed user: HTTP %d, want 401", code)
	}
	if _, err := h.client(t, ta); err == nil {
		t.Error("a removed user's token still connects")
	}
	if got := h.stepsFor(t, tb); got != 2222 {
		t.Errorf("removing alice disturbed bob: %d", got)
	}
	if code, _ := h.admin(t, "DELETE", "/admin/users/"+id, nil); code != 404 {
		t.Errorf("deleting twice: %d", code)
	}
	if code, m := h.admin(t, "GET", "/admin/users", nil); code != 200 || m["count"].(float64) != 1 {
		t.Errorf("list after removal: %d %v", code, m)
	}
}

func TestMulti_RotatingATokenReplacesIt(t *testing.T) {
	t.Parallel()
	h := newMultiHarness(t)
	id, old := h.register(t, "alice", "alice-key")

	code, m := h.admin(t, "POST", "/admin/users/"+id+"/token", nil)
	fresh := str(m, "token")
	if code != 200 || fresh == "" || fresh == old || !strings.HasPrefix(fresh, tokenPrefix) {
		t.Fatalf("rotate: %d %v", code, m)
	}
	if _, err := h.client(t, old); err == nil {
		t.Error("the old token still connects")
	}
	if got := h.stepsFor(t, fresh); got != 1111 {
		t.Errorf("the new token sees %d", got)
	}
	if code, _ := h.admin(t, "POST", "/admin/users/u_nope/token", nil); code != 404 {
		t.Errorf("unknown user: %d", code)
	}
}

func TestMulti_ReplacingAnExpiredKey(t *testing.T) {
	t.Parallel()
	h := newMultiHarness(t)
	id, ta := h.register(t, "alice", "alice-key")
	cs, err := h.client(t, ta)
	if err != nil {
		t.Fatal(err)
	}

	h.g.revoke("alice-key") // Garmin: the key has expired
	res := callTool(t, cs, "get_steps", nil)
	if !res.IsError || !strings.Contains(resultText(res), "administrator") || strings.Contains(resultText(res), "/auth") {
		t.Fatalf("an expired key should send the user to the administrator, not to /auth: %q", resultText(res))
	}

	h.g.addKey("alice-key-2", "alice", 1111)
	if code, m := h.admin(t, "PUT", "/admin/users/"+id+"/key", map[string]string{"api_key": "bob-key-typo"}); code != 422 {
		t.Errorf("a key Garmin rejects: %d %v", code, m)
	}
	if k, _ := h.store.APIKey(id); k != "alice-key" {
		t.Errorf("a rejected replacement changed the stored key to %q", k)
	}
	if code, m := h.admin(t, "PUT", "/admin/users/"+id+"/key", map[string]string{"api_key": "alice-key-2"}); code != 200 {
		t.Fatalf("PUT key: %d %v", code, m)
	}
	if k, _ := h.store.APIKey(id); k != "alice-key-2" {
		t.Errorf("stored key = %q", k)
	}
	// The old session ended; a new connection with the SAME token works again.
	if res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_steps"}); err == nil && !res.IsError {
		t.Error("the old session should not survive a key replacement")
	}
	if got := h.stepsFor(t, ta); got != 1111 {
		t.Errorf("after the key was replaced, alice sees %d", got)
	}
	for name, tc := range map[string]struct {
		path string
		body any
		want int
	}{
		"unknown user": {"/admin/users/u_nope/key", map[string]string{"api_key": "alice-key-2"}, 404},
		"blank key":    {"/admin/users/" + id + "/key", map[string]string{"api_key": " "}, 400},
	} {
		if code, _ := h.admin(t, "PUT", tc.path, tc.body); code != tc.want {
			t.Errorf("%s: %d, want %d", name, code, tc.want)
		}
	}
}

func TestMulti_UsersSurviveARestart(t *testing.T) {
	t.Parallel()
	h := newMultiHarness(t)
	_, ta := h.register(t, "alice", "alice-key")
	_, tb := h.register(t, "bob", "bob-key")
	h.start(t) // a restart: new process state, same users file and master key
	if got := h.stepsFor(t, ta); got != 1111 {
		t.Errorf("alice after restart: %d", got)
	}
	if got := h.stepsFor(t, tb); got != 2222 {
		t.Errorf("bob after restart: %d", got)
	}
	if h.srv.UserCount() != 2 {
		t.Errorf("UserCount = %d", h.srv.UserCount())
	}
}

func TestMulti_AdminEndpointsAreGuardedLikeTheRest(t *testing.T) {
	t.Parallel()
	h := newMultiHarness(t)
	admin := map[string]string{"Authorization": "Bearer " + multiAdminToken}
	body := `{"name":"mallory","api_key":"alice-key"}`

	if code, _, _ := do(t, "POST", h.ts.URL+"/admin/users", "application/json", body, map[string]string{"Authorization": admin["Authorization"], "Host": "evil.example"}); code != 403 {
		t.Errorf("a foreign Host reached /admin: %d", code)
	}
	if code, _, _ := do(t, "POST", h.ts.URL+"/admin/users", "application/json", body, map[string]string{"Authorization": admin["Authorization"], "Origin": "http://evil.example", "Sec-Fetch-Site": "cross-site"}); code != 403 {
		t.Errorf("a cross-site request reached /admin: %d", code)
	}
	if code, _, _ := do(t, "POST", h.ts.URL+"/mcp", "application/json", `{}`, map[string]string{"Authorization": "Bearer x", "Host": "evil.example"}); code != 403 {
		t.Errorf("a foreign Host reached /mcp: %d", code)
	}
	if n := len(h.store.List()); n != 0 {
		t.Errorf("a refused request still registered %d user(s)", n)
	}
}

// Keys, tokens and the admin token must not leak into any log. A token is shown
// once, in the response that issues it, and nowhere else.
func TestMulti_SecretsNeverLeak(t *testing.T) {
	t.Parallel()
	h := newMultiHarness(t)
	ida, ta := h.register(t, "alice", "alice-key")
	_, tb := h.register(t, "bob", "bob-key")
	h.stepsFor(t, ta)
	h.stepsFor(t, tb)
	rotated := ""
	if _, m := h.admin(t, "POST", "/admin/users/"+ida+"/token", nil); true {
		rotated = str(m, "token")
	}
	h.admin(t, "POST", "/admin/users", map[string]string{"name": "carol", "api_key": "wrong-key-LEAKCHECK"})
	do(t, "GET", h.ts.URL+"/admin/users", "", "", map[string]string{"Authorization": "Bearer attacker-guess-LEAKCHECK"})
	h.g.addKey("alice-key-2", "alice", 1)
	h.admin(t, "PUT", "/admin/users/"+ida+"/key", map[string]string{"api_key": "alice-key-2"})
	h.admin(t, "DELETE", "/admin/users/"+ida, nil)

	all := h.ops.String() + h.audit.rec.String()
	for _, secret := range []string{"alice-key", "bob-key", "alice-key-2", "wrong-key-LEAKCHECK", "attacker-guess", multiAdminToken, ta, tb, rotated, strings.TrimPrefix(ta, tokenPrefix), hashToken(ta)} {
		if strings.Contains(all, secret) {
			t.Errorf("a log contains %q", secret)
		}
	}
	raw, _ := os.ReadFile(h.usersPath)
	for _, secret := range []string{"alice-key", "bob-key", ta, tb, rotated, multiAdminToken} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("the users file contains %q", secret)
		}
	}
}

func TestMulti_AuditNamesTheUser(t *testing.T) {
	t.Parallel()
	h := newMultiHarness(t)
	ida, ta := h.register(t, "alice", "alice-key")
	_, tb := h.register(t, "bob", "bob-key")
	h.stepsFor(t, ta)
	h.stepsFor(t, tb)
	h.admin(t, "POST", "/admin/users/"+ida+"/token", nil)
	h.admin(t, "DELETE", "/admin/users/"+ida, nil)
	h.admin(t, "POST", "/admin/users", map[string]string{"name": "carol", "api_key": "wrong"})
	do(t, "POST", h.ts.URL+"/mcp", "application/json", `{}`, nil)

	// Every tool call says whose it was.
	byUser := map[string]int{}
	for _, c := range h.audit.events(t, "tool_call") {
		byUser[str(c, "user")]++
		if str(c, "user_id") == "" {
			t.Errorf("tool_call without a user_id: %+v", c)
		}
	}
	if byUser["alice"] != 1 || byUser["bob"] != 1 {
		t.Errorf("tool calls per user = %v", byUser)
	}
	for _, event := range []string{"user_registered", "user_token_rotated", "user_removed", "user_key_rejected", "access_denied"} {
		if len(h.audit.events(t, event)) == 0 {
			t.Errorf("no %q audit record", event)
		}
	}
	for _, e := range h.audit.events(t, "user_registered") {
		if str(e, "user") == "" || str(e, "user_id") == "" || str(e, "remote") == "" {
			t.Errorf("incomplete registration record: %+v", e)
		}
	}
	if removed := h.audit.events(t, "user_removed"); len(removed) != 1 || str(removed[0], "level") != "WARN" || str(removed[0], "user") != "alice" {
		t.Errorf("removal records = %+v", removed)
	}
}

func TestNewMulti_Validation(t *testing.T) {
	t.Parallel()
	store := openStore(t, filepath.Join(t.TempDir(), "u.json"), testMasterKey(t))
	client := func(garmin.TokenSource) *garmin.Client { return garmin.New() }
	for name, cfg := range map[string]MultiConfig{
		"no users":          {NewClient: client, AdminToken: multiAdminToken},
		"no client builder": {Users: store, AdminToken: multiAdminToken},
		"no admin token":    {Users: store, NewClient: client},
		"short admin token": {Users: store, NewClient: client, AdminToken: "short"},
		"bad allowed host":  {Users: store, NewClient: client, AdminToken: multiAdminToken, AllowedHosts: []string{"https://x"}},
	} {
		if _, err := NewMulti(cfg); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if _, err := NewMulti(MultiConfig{Users: store, NewClient: client, AdminToken: multiAdminToken, Addr: "0.0.0.0:8808"}); err != nil {
		t.Errorf("an exposed address is fine because the admin token is mandatory: %v", err)
	}
}
