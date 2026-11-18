package mcpserver

import (
	"crypto/subtle"
	"log/slog"
	"net"
	"net/http"
	"strings"
)

// bearerToken extracts the credentials of an "Authorization: Bearer <token>"
// header. The scheme is case-insensitive (RFC 7235) and surrounding spaces are
// ignored.
func bearerToken(h string) (string, bool) {
	scheme, cred, ok := strings.Cut(strings.TrimSpace(h), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	cred = strings.TrimSpace(cred)
	return cred, cred != ""
}

// requireToken demands "Authorization: Bearer <token>" when token is set.
func requireToken(token string, denied *denyAuditor, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok || subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			reason := "invalid_token"
			if r.Header.Get("Authorization") == "" {
				reason = "missing_token"
			}
			denied.record(r, reason) // never records what was presented
			w.Header().Set("WWW-Authenticate", `Bearer realm="garmin-mcp"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hostGuard rejects requests whose Host header is not in allowed. It defends a
// loopback-bound server against DNS rebinding, where a hostile web page makes
// the browser send requests to localhost under an attacker-controlled name.
// A nil or empty allowed set disables the check. /healthz is never refused.
func hostGuard(allowed []string, denied *denyAuditor, next http.Handler) http.Handler {
	if len(allowed) == 0 {
		return next
	}
	set := make(map[string]bool, len(allowed))
	for _, a := range allowed {
		set[strings.ToLower(a)] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The health check answers only "ok" and reveals nothing, so there is
		// nothing for a rebinding attack to gain from it. Platform health checks
		// (Fly, Kubernetes, load balancers) often send a Host header the operator
		// cannot control, and a check that fails takes the server out of service.
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if !set[strings.ToLower(strings.Trim(host, "[]"))] {
			denied.record(r, "forbidden_host", slog.String("host", truncate(r.Host, maxAuditField)))
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// LoopbackHosts are the Host header values accepted when bound to loopback.
var LoopbackHosts = []string{"localhost", "127.0.0.1", "::1"}

// IsLoopbackAddr reports whether a listen address ("host:port") is bound only
// to the loopback interface. An empty host (all interfaces) is not loopback.
func IsLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host) // SplitHostPort has removed any brackets
	return ip != nil && ip.IsLoopback()
}
