package mcpserver

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDisableAuthEndpoint_RemovesAuthAndTheKeyPage(t *testing.T) {
	t.Parallel()
	for name, token := range map[string]string{"without a token": "", "with a token": "tok-xyz"} {
		h := newHarness(t, func(c *Config) { c.DisableAuthEndpoint = true; c.Token = token })
		base := serveHandler(t, h)
		bearer := map[string]string{}
		if token != "" {
			bearer["Authorization"] = "Bearer " + token // even a valid token must not reach /auth
		}

		for _, method := range []string{"GET", "POST", "DELETE", "PUT"} {
			code, _, _ := do(t, method, base+"/auth", "application/json", `{"api_key":"`+validKey+`"}`, bearer)
			if code != http.StatusNotFound {
				t.Errorf("%s: %s /auth -> %d, want 404", name, method, code)
			}
		}
		for _, p := range []string{"/", "/index.html"} {
			if code, _, _ := do(t, "GET", base+p, "", "", bearer); code != http.StatusNotFound {
				t.Errorf("%s: GET %s -> %d, want 404 (no key page)", name, p, code)
			}
		}
		if h.creds.Status().Authenticated {
			t.Errorf("%s: a POST to the removed endpoint authenticated the server", name)
		}
		if code, _, _ := do(t, "GET", base+"/healthz", "", "", nil); code != 200 {
			t.Errorf("%s: /healthz must stay available: %d", name, code)
		}
	}
}

func TestDisableAuthEndpoint_MCPStillWorksWithAConfiguredKey(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *Config) { c.DisableAuthEndpoint = true; c.Token = "tok-xyz" })
	h.authenticate(t) // what the server's own configuration does at startup
	h.fg.route("/usersummary-service/stats/steps/daily/2026-11-10/2026-11-16", `[{"calendarDate":"2026-11-15","totalSteps":99,"stepGoal":1,"totalDistance":1}]`)
	base := serveHandler(t, h)

	hc := &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", "Bearer tok-xyz")
		return http.DefaultTransport.RoundTrip(r)
	})}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).
		Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: base + "/mcp", HTTPClient: hc}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	out := decodeResult[dayList[struct {
		Steps int `json:"steps"`
	}]](t, callTool(t, cs, "get_steps", nil))
	if out.Count != 1 || out.Days[0].Steps != 99 {
		t.Errorf("steps = %+v", out)
	}
}

// Without an endpoint for providing a key, telling an agent to "POST to /auth"
// would be wrong advice. Every message must point at the server's own
// configuration instead.
func TestDisableAuthEndpoint_GuidanceNeverMentionsAuth(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *Config) { c.DisableAuthEndpoint = true })
	cs := h.connect(t)

	check := func(what, text string) {
		t.Helper()
		if strings.Contains(text, "/auth") || strings.Contains(strings.ToLower(text), "browser") {
			t.Errorf("%s still points at the removed endpoint: %q", what, text)
		}
		if !strings.Contains(text, "GARMIN_PERSONAL_API_KEY") || !strings.Contains(text, "restart") {
			t.Errorf("%s does not say how to provide the key: %q", what, text)
		}
	}

	check("server instructions", cs.InitializeResult().Instructions)

	status := decodeResult[struct {
		Help string `json:"help"`
	}](t, callTool(t, cs, "auth_status", nil))
	check("auth_status help", status.Help)

	res := callTool(t, cs, "get_steps", nil) // not authenticated
	if !res.IsError {
		t.Fatal("expected an error before a key is set")
	}
	check("the unauthenticated tool error", resultText(res))

	// And when Garmin rejects a configured key.
	h.authenticate(t)
	h.fg.status["/usersummary-service/stats/steps/daily/2026-11-10/2026-11-16"] = 401
	res = callTool(t, cs, "get_steps", nil)
	if !res.IsError || !strings.Contains(resultText(res), "expired") {
		t.Fatalf("expected the expired-key error, got %q", resultText(res))
	}
	check("the rejected-key tool error", resultText(res))
}

func TestAuthEndpointEnabled_GuidanceStillMentionsIt(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	cs := h.connect(t)
	if !strings.Contains(cs.InitializeResult().Instructions, "/auth") {
		t.Errorf("instructions = %q", cs.InitializeResult().Instructions)
	}
	h.authenticate(t)
	h.fg.status["/usersummary-service/stats/steps/daily/2026-11-10/2026-11-16"] = 401
	if res := callTool(t, cs, "get_steps", nil); !strings.Contains(resultText(res), "/auth") {
		t.Errorf("rejected-key error = %q", resultText(res))
	}
}
