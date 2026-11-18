// Package mcpserver serves the garmin-connect client to AI agents over MCP.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	garminconnect "github.com/dianabarsan/garmin-training/garmin-connect"
	"github.com/dianabarsan/garmin-training/garmin-connect/auth"
)

// garminAPI is the part of *garminconnect.Client the tools use. *garminconnect.Client
// satisfies it; the interface exists so the server depends on what it needs.
type garminAPI interface {
	Activities(ctx context.Context, opts garminconnect.ActivitiesOptions) ([]garminconnect.Activity, error)
	ActivityDetail(ctx context.Context, id int64) (garminconnect.ActivityDetail, error)
	ActivitySeries(ctx context.Context, id int64) (garminconnect.ActivitySeries, error)
	DailySummaries(ctx context.Context, start, end time.Time) ([]garminconnect.DailySummary, error)
	Steps(ctx context.Context, start, end time.Time) ([]garminconnect.StepsDay, error)
	Stress(ctx context.Context, start, end time.Time) ([]garminconnect.StressDay, error)
	SleepRange(ctx context.Context, start, end time.Time) ([]garminconnect.Sleep, error)
	HeartRate(ctx context.Context, date time.Time) (garminconnect.HeartRateDay, error)
	HRVRange(ctx context.Context, start, end time.Time) ([]garminconnect.HRVSummary, error)
	BodyBattery(ctx context.Context, start, end time.Time) ([]garminconnect.BodyBatteryDay, error)
	VO2Max(ctx context.Context, start, end time.Time, sports ...string) ([]garminconnect.VO2MaxDay, error)
	HeartRateZonesFor(ctx context.Context, sport string, opts ...garminconnect.ZonesOption) (garminconnect.HeartRateZones, error)
}

// Config configures a Server.
type Config struct {
	// API is the Garmin client, holding the API key. Required.
	API garminAPI
	// Token, when set, must be sent as "Authorization: Bearer <token>" to /mcp.
	Token string
	// Addr is the address the server will listen on. A non-loopback address
	// requires Token.
	Addr string
	// AllowedHosts are the public hostnames (no scheme or port) the server may
	// be reached as, for example behind a reverse proxy. When set, requests
	// for any other Host are refused; localhost names stay allowed for health
	// checks. When empty, only loopback binds are
	// host-checked.
	AllowedHosts []string
	// Logger receives operational logs, never keys or request bodies.
	Logger *slog.Logger
	// Audit receives the audit trail: tool calls and refused requests. When
	// nil, audit records go to Logger tagged audit=true.
	Audit *slog.Logger
	// Now returns the current time; defaults to time.Now.
	Now func() time.Time
	// Version is reported to MCP clients.
	Version string
}

// Server is the MCP server plus its HTTP endpoints.
type Server struct {
	api    garminAPI
	log    *slog.Logger
	now    func() time.Time
	audit  *slog.Logger
	denied *denyAuditor // refused requests, throttled

	mcp     *mcp.Server
	handler http.Handler
}

// New builds a Server. It refuses a non-loopback Addr without a Token, because
// /mcp exposes personal health data.
func New(cfg Config) (*Server, error) {
	if cfg.API == nil {
		return nil, errors.New("mcpserver: API is required")
	}
	if cfg.Addr != "" && !IsLoopbackAddr(cfg.Addr) && cfg.Token == "" {
		return nil, fmt.Errorf("mcpserver: refusing to listen on %q without a token: it is not a loopback address, so anyone who can reach it could read your health data", cfg.Addr)
	}
	s := &Server{api: cfg.API, log: cfg.Logger, now: cfg.Now}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.now == nil {
		s.now = time.Now
	}
	s.audit = cfg.Audit
	if s.audit == nil {
		s.audit = s.log.With("audit", true)
	}
	s.denied = newDenyAuditor(s.audit, s.now)
	version := cfg.Version
	if version == "" {
		version = "dev"
	}

	s.mcp = mcp.NewServer(&mcp.Implementation{Name: "garmin-mcp", Title: "Garmin Connect (personal)", Version: version},
		&mcp.ServerOptions{Instructions: instructions, InitializedHandler: s.auditConnected})
	s.mcp.AddReceivingMiddleware(s.auditMiddleware())
	s.registerTools()

	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s.mcp },
		&mcp.StreamableHTTPOptions{SessionTimeout: 30 * time.Minute})

	protect := http.NewCrossOriginProtection()
	protect.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.denied.record(r, "cross_origin",
			slog.String("origin", truncate(r.Header.Get("Origin"), maxAuditField)),
			slog.String("sec_fetch_site", truncate(r.Header.Get("Sec-Fetch-Site"), maxAuditField)))
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
	}))
	allowed, err := normalizeHosts(cfg.AllowedHosts)
	if err != nil {
		return nil, err
	}
	var hosts []string
	switch {
	case len(allowed) > 0:
		hosts = append(allowed, LoopbackHosts...)
	case cfg.Addr == "" || IsLoopbackAddr(cfg.Addr):
		hosts = LoopbackHosts
	}
	mux := http.NewServeMux()
	mux.Handle("/mcp", requireToken(cfg.Token, s.denied, protect.Handler(mcpHandler)))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok\n"))
	})
	s.handler = hostGuard(hosts, s.denied, mux)
	return s, nil
}

// Handler returns the HTTP handler serving /mcp and /healthz.
func (s *Server) Handler() http.Handler { return s.handler }

// MCPServer returns the underlying MCP server, for connecting over other
// transports (the tests use an in-memory one).
func (s *Server) MCPServer() *mcp.Server { return s.mcp }

// instructions is the text MCP clients show their model about this server.
const instructions = `Read-only access to the user's own Garmin Connect data (Personal API).

Dates are the user's local calendar dates in YYYY-MM-DD form. Tools that take a date range default to the last 7 days ending today. Ranges are limited to 366 days.
Units: distances in metres, durations in seconds, speeds in metres per second, pace in seconds per kilometre, weights in kilograms, heart rate in beats per minute, elevation in metres.
If a tool reports that the Garmin API key was rejected, only the user can replace it; you cannot do it yourself.
Activities: use list_activities to find an activity id, then get_activity, get_activity_splits or get_activity_series. Garmin does not expose laps or individual strength sets.`

// keyRejected is the error a tool returns when Garmin rejects the API key.
const keyRejected = "the Garmin API key was rejected; it has probably expired or been deleted. " +
	"Ask the user to create a new one at connect.garmin.com/app/settings/personalApiToken and give it to the server " +
	"(with ./setup.sh --new-key when it was installed with the repository's setup script), then retry"

// userError turns a failure into a message that tells an agent what to do next.
func (s *Server) userError(err error) error {
	switch {
	case errors.Is(err, auth.ErrInvalidKey), errors.Is(err, garminconnect.ErrUnauthorized):
		return errors.New(keyRejected)
	case errors.Is(err, garminconnect.ErrRateLimited):
		return errors.New("the Garmin API is rate limiting requests; wait a little and retry")
	case errors.Is(err, garminconnect.ErrNotFound):
		return errors.New("not found: Garmin has nothing for that request; check the id or date")
	case errors.Is(err, garminconnect.ErrNoZones):
		return errors.New("no heart rate zones are configured for that sport, and there are no default zones; the user can set them up in Garmin Connect")
	case errors.Is(err, garminconnect.ErrServer):
		return errors.New("the Garmin API returned a server error; wait a little and retry")
	case errors.Is(err, garminconnect.ErrNoDistance):
		return errors.New("this activity has no distance data (for example strength training or an indoor workout), so it cannot be split by distance")
	}
	return err
}

// normalizeHosts validates and lower-cases allowed hostnames. A port, if given,
// is dropped because the Host check compares names only.
func normalizeHosts(in []string) ([]string, error) {
	var out []string
	for _, h := range in {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" {
			continue
		}
		if strings.Contains(h, "/") || strings.Contains(h, "@") || strings.ContainsAny(h, " \t") {
			return nil, fmt.Errorf("mcpserver: allowed host %q must be a bare hostname like garmin.example.com, not a URL", h)
		}
		if name, _, err := net.SplitHostPort(h); err == nil {
			h = name
		}
		out = append(out, strings.Trim(h, "[]"))
	}
	return out, nil
}
