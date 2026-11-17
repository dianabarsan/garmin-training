package mcpserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	garmin "github.com/dianabarsan/garmin-training"
	"github.com/dianabarsan/garmin-training/auth"
)

// minAdminToken is the shortest admin token accepted. The admin token can
// register users and read nothing else, but it is the key to the whole server.
const minAdminToken = 16

// maxAdminBody bounds an admin request body.
const maxAdminBody = 8 << 10

// MultiConfig configures a MultiServer.
type MultiConfig struct {
	// Users holds the registered users. Required.
	Users *UserStore
	// NewClient builds the Garmin client for one user from their credentials.
	// Required.
	NewClient func(garmin.TokenSource) *garmin.Client
	// AuthBaseURL overrides Garmin's host when validating keys (for tests).
	AuthBaseURL string
	// AuthHTTPClient is the HTTP client used to validate keys.
	AuthHTTPClient *http.Client
	// AdminToken must be sent as "Authorization: Bearer <token>" to /admin/...
	// It is separate from every user's token. Required, at least 16 characters.
	AdminToken string
	// Addr, AllowedHosts, Logger, Audit, Now and Version are as in Config.
	Addr         string
	AllowedHosts []string
	Logger       *slog.Logger
	Audit        *slog.Logger
	Now          func() time.Time
	Version      string
}

// userEntry is one user's MCP server and HTTP handler. Giving every user their
// own handler also gives them their own sessions, so one user's session id means
// nothing to another user's requests.
type userEntry struct {
	core    *Server
	handler http.Handler
}

// MultiServer serves many users from one process. Each user authenticates with
// their own token and only ever reaches their own Garmin data; an administrator
// registers users over /admin with a separate token.
type MultiServer struct {
	cfg   MultiConfig
	users *UserStore
	log   *slog.Logger
	audit *slog.Logger

	handler http.Handler

	mu      sync.Mutex
	entries map[string]*userEntry
}

// NewMulti builds a MultiServer.
func NewMulti(cfg MultiConfig) (*MultiServer, error) {
	if cfg.Users == nil || cfg.NewClient == nil {
		return nil, errors.New("mcpserver: Users and NewClient are required")
	}
	if len(cfg.AdminToken) < minAdminToken {
		return nil, fmt.Errorf("mcpserver: the admin token must be at least %d characters", minAdminToken)
	}
	m := &MultiServer{cfg: cfg, users: cfg.Users, log: cfg.Logger, audit: cfg.Audit, entries: map[string]*userEntry{}}
	if m.log == nil {
		m.log = slog.New(slog.DiscardHandler)
	}
	if m.audit == nil {
		m.audit = m.log.With("audit", true)
	}
	hosts, err := allowedHostList(cfg.Addr, cfg.AllowedHosts)
	if err != nil {
		return nil, err
	}

	protect := newCrossOriginProtection(m.audit)
	mux := http.NewServeMux()
	mux.Handle("/mcp", protect.Handler(http.HandlerFunc(m.serveMCP)))
	mux.Handle("/admin/", requireToken(cfg.AdminToken, m.audit, protect.Handler(m.adminMux())))
	mux.HandleFunc("/healthz", healthz)
	m.handler = hostGuard(hosts, m.audit, mux)
	return m, nil
}

// Handler returns the HTTP handler serving /mcp, /admin/... and /healthz.
func (m *MultiServer) Handler() http.Handler { return m.handler }

// UserCount reports how many users are registered.
func (m *MultiServer) UserCount() int { return len(m.users.List()) }

// serveMCP authenticates a request by its user token and hands it to that
// user's own MCP handler.
func (m *MultiServer) serveMCP(w http.ResponseWriter, r *http.Request) {
	token, ok := bearerToken(r)
	var user User
	if ok {
		user, ok = m.users.Lookup(token)
	}
	if !ok {
		auditDenied(m.audit, r, tokenDenialReason(r)) // never records what was presented
		w.Header().Set("WWW-Authenticate", `Bearer realm="garmin-mcp"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	e, err := m.entryFor(user)
	if err != nil {
		m.log.Error("preparing a user's session failed", "user_id", user.ID, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	e.handler.ServeHTTP(w, r)
}

// entryFor returns the user's MCP server, building it from their stored key on
// first use.
func (m *MultiServer) entryFor(u User) (*userEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.entries[u.ID]; ok {
		return e, nil
	}
	key, err := m.users.APIKey(u.ID)
	if err != nil {
		return nil, err
	}
	creds := m.newCredentials()
	creds.Activate(key)
	core := newCore(coreConfig{
		api:          m.cfg.NewClient(creds),
		creds:        creds,
		log:          m.log,
		audit:        m.audit.With("user", u.Name, "user_id", u.ID),
		now:          m.cfg.Now,
		version:      m.cfg.Version,
		adminManaged: true,
	})
	e := &userEntry{
		core: core,
		handler: mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return core.mcp },
			&mcp.StreamableHTTPOptions{SessionTimeout: 30 * time.Minute}),
	}
	m.entries[u.ID] = e
	return e, nil
}

// dropEntry forgets a user's MCP server, ending their sessions; the next request
// builds a fresh one from whatever the store then holds.
func (m *MultiServer) dropEntry(id string) {
	m.mu.Lock()
	delete(m.entries, id)
	m.mu.Unlock()
}

func (m *MultiServer) newCredentials() *Credentials {
	var opts []CredentialsOption
	if m.cfg.AuthBaseURL != "" {
		opts = append(opts, WithAuthBaseURL(m.cfg.AuthBaseURL))
	}
	if m.cfg.AuthHTTPClient != nil {
		opts = append(opts, WithAuthHTTPClient(m.cfg.AuthHTTPClient))
	}
	return NewCredentials(opts...)
}

// --- admin API ----------------------------------------------------------------

func (m *MultiServer) adminMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /admin/users", m.register)
	mux.HandleFunc("GET /admin/users", m.list)
	mux.HandleFunc("DELETE /admin/users/{id}", m.remove)
	mux.HandleFunc("POST /admin/users/{id}/token", m.rotateToken)
	mux.HandleFunc("PUT /admin/users/{id}/key", m.setKey)
	return mux
}

func adminJSON(w http.ResponseWriter, code int, v any) {
	securityHeaders(w.Header())
	writeJSON(w, code, v)
}

func adminError(w http.ResponseWriter, code int, msg string) {
	adminJSON(w, code, map[string]string{"error": msg})
}

// readAdminJSON decodes a small JSON body. Requiring the JSON content type means
// a browser cannot send the request without a preflight, as a second defence
// next to the cross-origin check.
func readAdminJSON(w http.ResponseWriter, r *http.Request, into any) bool {
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		adminError(w, http.StatusUnsupportedMediaType, "use Content-Type: application/json")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxAdminBody)
	if err := json.NewDecoder(r.Body).Decode(into); err != nil {
		adminError(w, http.StatusBadRequest, "the body must be valid JSON of the documented shape")
		return false
	}
	return true
}

// checkKey asks Garmin whether a key is valid, replying to the client and
// returning false when it is not.
func (m *MultiServer) checkKey(w http.ResponseWriter, r *http.Request, key string) bool {
	err := m.newCredentials().Set(r.Context(), key)
	switch {
	case err == nil:
		return true
	case errors.Is(err, auth.ErrNoAPIKey):
		adminError(w, http.StatusBadRequest, ErrNoKey.Error())
	case errors.Is(err, auth.ErrInvalidKey):
		adminError(w, http.StatusUnprocessableEntity, "Garmin rejected that key")
	default:
		// Network trouble; details go to the log, never to the client.
		m.log.Error("validating a Garmin key failed", "err", err)
		adminError(w, http.StatusBadGateway, "could not validate the key with Garmin; try again")
	}
	return false
}

func (m *MultiServer) userAudit(r *http.Request, event string, u User, level slog.Level) {
	attrs := append([]slog.Attr{slog.String("user_id", u.ID), slog.String("user", u.Name)}, remoteAttrs(r)...)
	m.audit.LogAttrs(r.Context(), level, event, attrs...)
}

// storeError maps store failures to HTTP replies.
func (m *MultiServer) storeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrUserNotFound):
		adminError(w, http.StatusNotFound, ErrUserNotFound.Error())
	case errors.Is(err, ErrUserExists):
		adminError(w, http.StatusConflict, ErrUserExists.Error())
	case errors.Is(err, ErrInvalidName):
		adminError(w, http.StatusBadRequest, ErrInvalidName.Error())
	case errors.Is(err, ErrNoKey):
		adminError(w, http.StatusBadRequest, ErrNoKey.Error())
	default:
		m.log.Error("user store failed", "err", err)
		adminError(w, http.StatusInternalServerError, "could not save the change")
	}
}

type registerRequest struct {
	Name   string `json:"name"`
	APIKey string `json:"api_key"`
}

// register adds a user, after Garmin has accepted their key, and returns the
// token they will use. The token is shown only in this response.
func (m *MultiServer) register(w http.ResponseWriter, r *http.Request) {
	var in registerRequest
	if !readAdminJSON(w, r, &in) {
		return
	}
	if err := m.users.CheckName(in.Name); err != nil { // cheap checks before calling Garmin
		m.storeError(w, err)
		return
	}
	if strings.TrimSpace(in.APIKey) == "" {
		adminError(w, http.StatusBadRequest, ErrNoKey.Error())
		return
	}
	if !m.checkKey(w, r, in.APIKey) {
		m.audit.LogAttrs(r.Context(), slog.LevelWarn, "user_key_rejected",
			append([]slog.Attr{slog.String("user", truncate(strings.TrimSpace(in.Name), maxAuditField))}, remoteAttrs(r)...)...)
		return
	}
	user, token, err := m.users.Add(in.Name, in.APIKey)
	if err != nil {
		m.storeError(w, err)
		return
	}
	m.log.Info("user registered", "user_id", user.ID)
	m.userAudit(r, "user_registered", user, slog.LevelInfo)
	adminJSON(w, http.StatusCreated, map[string]any{
		"id": user.ID, "name": user.Name, "created_at": user.CreatedAt,
		"token": token,
		"note":  "This token is shown once. Give it to the user as their Bearer token for /mcp.",
	})
}

func (m *MultiServer) list(w http.ResponseWriter, _ *http.Request) {
	users := m.users.List()
	adminJSON(w, http.StatusOK, map[string]any{"count": len(users), "users": users})
}

func (m *MultiServer) remove(w http.ResponseWriter, r *http.Request) {
	u, err := m.users.Get(r.PathValue("id"))
	if err == nil {
		err = m.users.Remove(u.ID)
	}
	if err != nil {
		m.storeError(w, err)
		return
	}
	m.dropEntry(u.ID)
	m.log.Info("user removed", "user_id", u.ID)
	m.userAudit(r, "user_removed", u, slog.LevelWarn)
	securityHeaders(w.Header())
	w.WriteHeader(http.StatusNoContent)
}

// rotateToken replaces a user's token; the old one stops working at once.
func (m *MultiServer) rotateToken(w http.ResponseWriter, r *http.Request) {
	u, err := m.users.Get(r.PathValue("id"))
	if err != nil {
		m.storeError(w, err)
		return
	}
	token, err := m.users.RotateToken(u.ID)
	if err != nil {
		m.storeError(w, err)
		return
	}
	m.userAudit(r, "user_token_rotated", u, slog.LevelInfo)
	adminJSON(w, http.StatusOK, map[string]any{
		"id": u.ID, "name": u.Name, "token": token,
		"note": "This token is shown once. The previous token no longer works.",
	})
}

// setKey replaces a user's Garmin key, for example when theirs expires.
func (m *MultiServer) setKey(w http.ResponseWriter, r *http.Request) {
	u, err := m.users.Get(r.PathValue("id"))
	if err != nil {
		m.storeError(w, err)
		return
	}
	var in struct {
		APIKey string `json:"api_key"`
	}
	if !readAdminJSON(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.APIKey) == "" {
		adminError(w, http.StatusBadRequest, ErrNoKey.Error())
		return
	}
	if !m.checkKey(w, r, in.APIKey) {
		m.userAudit(r, "user_key_rejected", u, slog.LevelWarn)
		return
	}
	if err := m.users.SetAPIKey(u.ID, in.APIKey); err != nil {
		m.storeError(w, err)
		return
	}
	m.dropEntry(u.ID) // sessions restart with the new key
	m.userAudit(r, "user_key_updated", u, slog.LevelInfo)
	adminJSON(w, http.StatusOK, map[string]any{"id": u.ID, "name": u.Name})
}
