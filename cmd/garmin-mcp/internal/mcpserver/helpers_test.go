package mcpserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	garminconnect "github.com/dianabarsan/garmin-training/garmin-connect"
)

const (
	tokenPath = "/personalapitoken-service/personal-api/personal-api-key/token"
	validKey  = "test-garmin-key-0123456789"
)

var fixedNow = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

func makeJWT(exp time.Time) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc(fmt.Appendf(nil, `{"exp":%d}`, exp.Unix())) + ".sig"
}

// fakeGarmin stands in for Garmin's Personal API.
type fakeGarmin struct {
	srv *httptest.Server

	mu        sync.Mutex
	routes    map[string]string // path -> JSON body
	queries   map[string]string // path -> last raw query
	exchanges int
	status    map[string]int // path -> forced status
}

func newFakeGarmin(t *testing.T) *fakeGarmin {
	t.Helper()
	f := &fakeGarmin{routes: map[string]string{}, queries: map[string]string{}, status: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.URL.Path == tokenPath {
			f.exchanges++
			if r.Header.Get("Authorization") != "ApiKey "+validKey {
				http.Error(w, "nope", http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(makeJWT(time.Now().Add(24 * time.Hour))))
			return
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ey") {
			http.Error(w, "no bearer", http.StatusUnauthorized)
			return
		}
		f.queries[r.URL.Path] = r.URL.RawQuery
		if code, ok := f.status[r.URL.Path]; ok {
			w.WriteHeader(code)
			return
		}
		body, ok := f.routes[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGarmin) route(path, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[path] = body
}

func (f *fakeGarmin) query(path string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.queries[path]
}

func (f *fakeGarmin) exchangeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.exchanges
}

// recordingLog captures everything logged, so tests can prove no secret leaks.
type recordingLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (r *recordingLog) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&lockedWriter{r}, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type lockedWriter struct{ r *recordingLog }

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.r.mu.Lock()
	defer w.r.mu.Unlock()
	return w.r.buf.Write(p)
}

func (r *recordingLog) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.String()
}

// harness wires a Server to a fake Garmin.
type harness struct {
	fg    *fakeGarmin
	creds *Credentials
	srv   *Server
	log   *recordingLog
}

type harnessOpt func(*Config)

func newHarness(t *testing.T, opts ...harnessOpt) *harness {
	t.Helper()
	h := &harness{fg: newFakeGarmin(t), log: &recordingLog{}}
	h.creds = NewCredentials(WithAuthBaseURL(h.fg.srv.URL))
	cfg := Config{
		API:         garminconnect.New(garminconnect.WithBaseURL(h.fg.srv.URL), garminconnect.WithTokenSource(h.creds), garminconnect.WithRetry(0, 0, 0)),
		Credentials: h.creds,
		Logger:      h.log.logger(),
		Now:         func() time.Time { return fixedNow },
	}
	for _, o := range opts {
		o(&cfg)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.srv = srv
	return h
}

func (h *harness) authenticate(t *testing.T) {
	t.Helper()
	if err := h.creds.Set(context.Background(), validKey); err != nil {
		t.Fatal(err)
	}
}

// connect opens an in-memory MCP client session.
func (h *harness) connect(t *testing.T) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := h.srv.MCPServer().Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func callTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s): protocol error: %v", name, err)
	}
	return res
}

func resultText(res *mcp.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

// decodeResult unmarshals a tool's structured output into T, failing the test
// if the call reported an error.
func decodeResult[T any](t *testing.T, res *mcp.CallToolResult) T {
	t.Helper()
	if res.IsError {
		t.Fatalf("tool returned an error: %s", resultText(res))
	}
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var out T
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decoding %s: %v", b, err)
	}
	return out
}
