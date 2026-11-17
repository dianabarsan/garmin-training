package mcpserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The audit log answers "who asked for what, and who was turned away". It records
// which tools were called with which arguments, whether they succeeded, how much
// came back, and every refused request. It never records the Garmin key, a
// bearer token (neither a valid nor a presented one), or any data a tool returned.
//
// Arguments are logged because they say what was read (a date range, an
// activity id). They can include free text such as a search term, so the log
// should be kept as private as the data itself.

const (
	maxAuditArgs  = 400 // bytes of a tool's arguments kept in a record
	maxAuditField = 120 // bytes kept of any other client-supplied string
	maxAuditError = 200 // bytes kept of an error message
)

// truncate shortens s to at most n bytes, never splitting a UTF-8 character.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// shortHash is a few bytes of a SHA-256: enough to tell sessions apart in the
// log without exposing an identifier that could be replayed.
func shortHash(s string) string {
	if s == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:4])
}

// compactJSON renders raw JSON on one line, truncated.
func compactJSON(raw json.RawMessage, n int) string {
	if len(raw) == 0 {
		return ""
	}
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return truncate(string(raw), n)
	}
	if out := b.String(); out == "null" || out == "{}" {
		return "" // no arguments: nothing worth recording
	}
	return truncate(b.String(), n)
}

// deniedAttrs describes a refused HTTP request. It deliberately has no
// Authorization header: only what is safe to keep.
func deniedAttrs(r *http.Request, reason string, extra ...slog.Attr) []slog.Attr {
	attrs := []slog.Attr{
		slog.String("reason", reason),
		slog.String("method", r.Method),
		slog.String("path", truncate(r.URL.Path, maxAuditField)),
	}
	attrs = append(attrs, remoteAttrs(r)...)
	if ua := r.UserAgent(); ua != "" {
		attrs = append(attrs, slog.String("user_agent", truncate(ua, maxAuditField)))
	}
	return append(attrs, extra...)
}

// remoteAttrs identifies the peer. Behind a reverse proxy remote is the proxy's
// address; forwarded_for is the X-Forwarded-For header exactly as received and
// can be forged by the client, so treat it as a hint.
func remoteAttrs(r *http.Request) []slog.Attr {
	attrs := []slog.Attr{slog.String("remote", r.RemoteAddr)}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		attrs = append(attrs, slog.String("forwarded_for", truncate(xff, maxAuditField)))
	}
	return attrs
}

// Refused requests are recorded at most deniedBurst times per remote address
// in each deniedWindow, so anyone who can reach the port cannot flood the audit
// log (and the disk) with refusals or bury the tool calls among them.
const (
	deniedBurst      = 20
	deniedWindow     = time.Minute
	deniedMaxSources = 1024 // addresses tracked at once; the rest share one budget
)

// denyAuditor records refused requests, throttled per remote address. When an
// address exceeds its budget, one access_denied_throttled record says so; the
// number of refusals left out is reported in the next record for that address
// after the window ends.
type denyAuditor struct {
	log *slog.Logger
	now func() time.Time

	mu      sync.Mutex
	sources map[string]*denySource
}

type denySource struct {
	start      time.Time
	count      int // refusals recorded in this window
	suppressed int // refusals left out in this window
}

func newDenyAuditor(log *slog.Logger, now func() time.Time) *denyAuditor {
	return &denyAuditor{log: log, now: now, sources: map[string]*denySource{}}
}

// record logs a refused request, unless its source has used up its budget.
func (d *denyAuditor) record(r *http.Request, reason string, extra ...slog.Attr) {
	src := r.RemoteAddr
	if h, _, err := net.SplitHostPort(src); err == nil {
		src = h
	}
	now := d.now()

	d.mu.Lock()
	s := d.source(src, now)
	var missed int
	if now.Sub(s.start) >= deniedWindow {
		missed = s.suppressed
		*s = denySource{start: now}
	}
	s.count++
	verdict := "log"
	switch {
	case s.count == deniedBurst+1:
		verdict = "throttle"
		s.suppressed++
	case s.count > deniedBurst+1:
		verdict = "drop"
		s.suppressed++
	}
	until := s.start.Add(deniedWindow)
	d.mu.Unlock()

	ctx := r.Context()
	switch verdict {
	case "log":
		attrs := deniedAttrs(r, reason, extra...)
		if missed > 0 {
			attrs = append(attrs, slog.Int("previously_unrecorded", missed))
		}
		d.log.LogAttrs(ctx, slog.LevelWarn, "access_denied", attrs...)
	case "throttle":
		attrs := append(remoteAttrs(r),
			slog.Int("recorded", deniedBurst),
			slog.Time("resumes_at", until.UTC()))
		d.log.LogAttrs(ctx, slog.LevelWarn, "access_denied_throttled", attrs...)
	}
}

// source returns the counters for src, creating them. The map is bounded:
// expired entries are dropped when it is full, and if it is still full, new
// sources share one entry.
func (d *denyAuditor) source(src string, now time.Time) *denySource {
	if s, ok := d.sources[src]; ok {
		return s
	}
	if len(d.sources) >= deniedMaxSources {
		for k, s := range d.sources {
			if now.Sub(s.start) >= deniedWindow && s.suppressed == 0 {
				delete(d.sources, k)
			}
		}
		if len(d.sources) >= deniedMaxSources {
			src = "(other)"
			if s, ok := d.sources[src]; ok {
				return s
			}
		}
	}
	s := &denySource{start: now}
	d.sources[src] = s
	return s
}

// auditMiddleware records every tool call, for all tools at once, without each
// handler having to know about it.
func (s *Server) auditMiddleware() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			switch method {
			case "tools/call":
				start := time.Now()
				res, err := next(ctx, method, req)
				s.auditToolCall(ctx, req, res, err, time.Since(start))
				return res, err
			}
			return next(ctx, method, req)
		}
	}
}

// clientAttrs names the connected MCP client and a hash of its session.
func clientAttrs(req mcp.Request) []slog.Attr {
	var attrs []slog.Attr
	ss, ok := req.GetSession().(*mcp.ServerSession)
	if !ok || ss == nil {
		return attrs
	}
	if ip := ss.InitializeParams(); ip != nil && ip.ClientInfo != nil {
		attrs = append(attrs, slog.String("client", truncate(ip.ClientInfo.Name+"/"+ip.ClientInfo.Version, maxAuditField)))
	}
	if id := shortHash(ss.ID()); id != "" {
		attrs = append(attrs, slog.String("session", id))
	}
	return attrs
}

func (s *Server) auditToolCall(ctx context.Context, req mcp.Request, res mcp.Result, err error, took time.Duration) {
	attrs := []slog.Attr{}
	if p, ok := req.GetParams().(*mcp.CallToolParamsRaw); ok && p != nil {
		attrs = append(attrs, slog.String("tool", truncate(p.Name, maxAuditField)))
		if args := compactJSON(p.Arguments, maxAuditArgs); args != "" {
			attrs = append(attrs, slog.String("args", args))
		}
	}

	outcome, errText, size := "ok", "", 0
	if r, ok := res.(*mcp.CallToolResult); ok && r != nil {
		for _, c := range r.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				size += len(tc.Text)
				if r.IsError && errText == "" {
					errText = truncate(tc.Text, maxAuditError)
				}
			}
		}
		if r.IsError {
			outcome = "error"
		}
	}
	if err != nil {
		outcome, errText = "error", truncate(err.Error(), maxAuditError)
	}

	attrs = append(attrs, slog.String("outcome", outcome))
	if errText != "" {
		attrs = append(attrs, slog.String("error", errText))
	}
	attrs = append(attrs,
		slog.Int64("duration_ms", took.Milliseconds()),
		slog.Int("result_bytes", size),
	)
	attrs = append(attrs, clientAttrs(req)...)
	s.audit.LogAttrs(ctx, slog.LevelInfo, "tool_call", attrs...)
}

// auditConnected records a client finishing the MCP handshake. The SDK does not
// pass the initialize request through middleware, so this is hooked in as the
// server's InitializedHandler instead.
func (s *Server) auditConnected(ctx context.Context, req *mcp.InitializedRequest) {
	attrs := []slog.Attr{}
	if req != nil && req.Session != nil {
		if ip := req.Session.InitializeParams(); ip != nil && ip.ClientInfo != nil {
			attrs = append(attrs, slog.String("client", truncate(ip.ClientInfo.Name+"/"+ip.ClientInfo.Version, maxAuditField)))
		}
		if id := shortHash(req.Session.ID()); id != "" {
			attrs = append(attrs, slog.String("session", id))
		}
	}
	s.audit.LogAttrs(ctx, slog.LevelInfo, "client_connected", attrs...)
}
