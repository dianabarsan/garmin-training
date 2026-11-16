package mcpserver

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"strings"

	"github.com/dianabarsan/garmin-training/auth"
)

// maxAuthBody bounds the /auth request body; an API key is a couple of hundred
// bytes.
const maxAuthBody = 4 << 10

// securityHeaders are set on every response from the auth endpoints.
func securityHeaders(h http.Header) {
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'")
}

// requireToken demands "Authorization: Bearer <token>" when token is set.
func requireToken(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), want) != 1 {
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
// A nil or empty allowed set disables the check.
func hostGuard(allowed []string, next http.Handler) http.Handler {
	if len(allowed) == 0 {
		return next
	}
	set := make(map[string]bool, len(allowed))
	for _, a := range allowed {
		set[strings.ToLower(a)] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if !set[strings.ToLower(strings.Trim(host, "[]"))] {
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
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

type authResponse struct {
	Status
	Message string `json:"message,omitempty"`
}

// authHandler serves /auth: GET reports status, POST provides a key, DELETE
// forgets it. It never returns or logs the key.
func authHandler(creds *Credentials, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		securityHeaders(w.Header())
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			writeJSON(w, http.StatusOK, authResponse{Status: creds.Status()})
		case http.MethodPost:
			setKey(w, r, creds, log)
		case http.MethodDelete:
			if err := creds.Clear(); err != nil {
				log.Error("clearing key failed", "err", err)
				writeJSON(w, http.StatusInternalServerError, authResponse{Status: creds.Status(), Message: "could not remove the stored key"})
				return
			}
			log.Info("API key cleared")
			writeJSON(w, http.StatusOK, authResponse{Status: creds.Status(), Message: "key removed"})
		default:
			w.Header().Set("Allow", "GET, POST, DELETE")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

func setKey(w http.ResponseWriter, r *http.Request, creds *Credentials, log *slog.Logger) {
	r.Body = http.MaxBytesReader(w, r.Body, maxAuthBody)
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))

	var key string
	form := false
	switch mt {
	case "application/json":
		var in struct {
			APIKey string `json:"api_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			respondAuth(w, creds, false, http.StatusBadRequest, `body must be JSON like {"api_key": "..."}`)
			return
		}
		key = in.APIKey
	case "application/x-www-form-urlencoded":
		form = true
		if err := r.ParseForm(); err != nil {
			respondAuth(w, creds, true, http.StatusBadRequest, "could not read the form")
			return
		}
		key = r.PostForm.Get("api_key")
	default:
		respondAuth(w, creds, false, http.StatusUnsupportedMediaType, "use Content-Type: application/json")
		return
	}

	if strings.TrimSpace(key) == "" {
		respondAuth(w, creds, form, http.StatusBadRequest, "api_key is required")
		return
	}
	if err := creds.Set(r.Context(), key); err != nil {
		switch {
		case errors.Is(err, auth.ErrInvalidKey):
			log.Warn("API key rejected by Garmin")
			respondAuth(w, creds, form, http.StatusUnauthorized, "Garmin rejected that key. Create a new one at connect.garmin.com/app/settings/personalApiToken")
		case errors.Is(err, auth.ErrMalformedToken):
			respondAuth(w, creds, form, http.StatusBadGateway, "Garmin answered, but not with an access token")
		default:
			// Network trouble or a failure saving the key; details are logged,
			// never sent back (they could reveal paths).
			log.Error("setting API key failed", "err", err)
			respondAuth(w, creds, form, http.StatusBadGateway, "could not validate the key with Garmin; try again")
		}
		return
	}
	log.Info("API key accepted")
	respondAuth(w, creds, form, http.StatusOK, "key accepted")
}

func respondAuth(w http.ResponseWriter, creds *Credentials, asHTML bool, code int, msg string) {
	if asHTML {
		renderPage(w, code, creds.Status(), msg)
		return
	}
	writeJSON(w, code, authResponse{Status: creds.Status(), Message: msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

var pageTmpl = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Garmin MCP</title>
<style>body{font:16px/1.5 system-ui,sans-serif;max-width:34rem;margin:3rem auto;padding:0 1rem}
input[type=password]{width:100%;padding:.5rem;font-size:1rem;box-sizing:border-box}
button{margin-top:.75rem;padding:.5rem 1rem;font-size:1rem}.msg{padding:.5rem .75rem;background:#eef}
.ok{color:#0a6b2d}.no{color:#a12}</style></head><body>
<h1>Garmin MCP server</h1>
{{if .Message}}<p class="msg">{{.Message}}</p>{{end}}
<p>Status: {{if .Status.Authenticated}}<b class="ok">authenticated</b>{{else}}<b class="no">no API key</b>{{end}}</p>
<p>Paste your Garmin Connect <em>Personal API</em> key. It is checked with Garmin and kept in memory{{if .Status.Persisted}} and on disk{{end}}; it is never shown again.</p>
<form method="post" action="/auth">
<label for="k">API key</label>
<input id="k" name="api_key" type="password" autocomplete="off" required>
<button type="submit">Save key</button></form>
<p>Create a key at <code>connect.garmin.com/app/settings/personalApiToken</code>. MCP clients connect to <code>/mcp</code>.</p>
</body></html>`))

func renderPage(w http.ResponseWriter, code int, st Status, msg string) {
	securityHeaders(w.Header())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	_ = pageTmpl.Execute(w, struct {
		Status  Status
		Message string
	}{st, msg})
}

// indexHandler serves the key-entry page at "/".
func indexHandler(creds *Credentials) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		renderPage(w, http.StatusOK, creds.Status(), "")
	})
}
