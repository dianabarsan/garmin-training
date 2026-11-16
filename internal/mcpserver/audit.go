package mcpserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
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

// auditDenied records a refused request.
func auditDenied(l *slog.Logger, r *http.Request, reason string, extra ...slog.Attr) {
	l.LogAttrs(r.Context(), slog.LevelWarn, "access_denied", deniedAttrs(r, reason, extra...)...)
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
