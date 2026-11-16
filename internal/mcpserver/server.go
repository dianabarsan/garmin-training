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

	garmin "github.com/dianabarsan/garmin-training"
	"github.com/dianabarsan/garmin-training/auth"
)

// garminAPI is the part of *garmin.Client the tools use. *garmin.Client
// satisfies it; the interface exists so the server depends on what it needs.
type garminAPI interface {
	Activities(ctx context.Context, opts garmin.ActivitiesOptions) ([]garmin.Activity, error)
	ActivityDetail(ctx context.Context, id int64) (garmin.ActivityDetail, error)
	ActivitySeries(ctx context.Context, id int64) (garmin.ActivitySeries, error)
	DailySummaries(ctx context.Context, start, end time.Time) ([]garmin.DailySummary, error)
	Steps(ctx context.Context, start, end time.Time) ([]garmin.StepsDay, error)
	Stress(ctx context.Context, start, end time.Time) ([]garmin.StressDay, error)
	SleepRange(ctx context.Context, start, end time.Time) ([]garmin.Sleep, error)
	HeartRate(ctx context.Context, date time.Time) (garmin.HeartRateDay, error)
	HRVRange(ctx context.Context, start, end time.Time) ([]garmin.HRVSummary, error)
	BodyBattery(ctx context.Context, start, end time.Time) ([]garmin.BodyBatteryDay, error)
	VO2Max(ctx context.Context, start, end time.Time, sports ...string) ([]garmin.VO2MaxDay, error)
	HeartRateZonesFor(ctx context.Context, sport string, opts ...garmin.ZonesOption) (garmin.HeartRateZones, error)
}

// Config configures a Server.
type Config struct {
	// API is the Garmin client. Required. Build it with the same Credentials as
	// the one below so a key provided at /auth takes effect immediately.
	API garminAPI
	// Credentials holds the API key. Required.
	Credentials *Credentials
	// Token, when set, must be sent as "Authorization: Bearer <token>" to /mcp
	// and /auth. The key-entry page at "/" is only served when it is empty.
	Token string
	// Addr is the address the server will listen on. A non-loopback address
	// requires Token.
	Addr string
	// AllowedHosts are the public hostnames (no scheme or port) the server may
	// be reached as, for example behind a reverse proxy. When set, requests
	// for any other Host are refused; localhost names stay allowed for health
	// checks and local administration. When empty, only loopback binds are
	// host-checked.
	AllowedHosts []string
	// Logger receives operational logs, never keys or request bodies.
	Logger *slog.Logger
	// Now returns the current time; defaults to time.Now.
	Now func() time.Time
	// Version is reported to MCP clients.
	Version string
}

// Server is the MCP server plus its HTTP endpoints.
type Server struct {
	api   garminAPI
	creds *Credentials
	log   *slog.Logger
	now   func() time.Time

	mcp     *mcp.Server
	handler http.Handler
}

// New builds a Server. It refuses a non-loopback Addr without a Token, because
// the endpoints expose personal health data and accept the Garmin key.
func New(cfg Config) (*Server, error) {
	if cfg.API == nil || cfg.Credentials == nil {
		return nil, errors.New("mcpserver: API and Credentials are required")
	}
	if cfg.Addr != "" && !IsLoopbackAddr(cfg.Addr) && cfg.Token == "" {
		return nil, fmt.Errorf("mcpserver: refusing to listen on %q without a token: it is not a loopback address, so anyone who can reach it could read your health data and replace your key", cfg.Addr)
	}
	s := &Server{api: cfg.API, creds: cfg.Credentials, log: cfg.Logger, now: cfg.Now}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.now == nil {
		s.now = time.Now
	}
	version := cfg.Version
	if version == "" {
		version = "dev"
	}

	s.mcp = mcp.NewServer(&mcp.Implementation{Name: "garmin-mcp", Title: "Garmin Connect (personal)", Version: version},
		&mcp.ServerOptions{Instructions: instructions})
	s.registerTools()

	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s.mcp },
		&mcp.StreamableHTTPOptions{SessionTimeout: 30 * time.Minute})

	protect := http.NewCrossOriginProtection()
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
	mux.Handle("/mcp", requireToken(cfg.Token, protect.Handler(mcpHandler)))
	mux.Handle("/auth", requireToken(cfg.Token, protect.Handler(authHandler(s.creds, s.log))))
	if cfg.Token == "" {
		mux.Handle("/", protect.Handler(indexHandler(s.creds)))
	}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok\n"))
	})
	s.handler = hostGuard(hosts, mux)
	return s, nil
}

// Handler returns the HTTP handler serving /mcp, /auth, "/" and /healthz.
func (s *Server) Handler() http.Handler { return s.handler }

// MCPServer returns the underlying MCP server, for connecting over other
// transports (the tests use an in-memory one).
func (s *Server) MCPServer() *mcp.Server { return s.mcp }

const instructions = `Read-only access to the user's own Garmin Connect data (Personal API).

Dates are the user's local calendar dates in YYYY-MM-DD form. Tools that take a date range default to the last 7 days ending today. Ranges are limited to 366 days.
Units: distances in metres, durations in seconds, speeds in metres per second, pace in seconds per kilometre, weights in kilograms, heart rate in beats per minute, elevation in metres.
If a tool reports that no API key has been provided, ask the user to open the server's address in a browser (or POST their Garmin Personal API key to /auth); you cannot provide it yourself.
Activities: use list_activities to find an activity id, then get_activity, get_activity_splits or get_activity_series. Garmin does not expose laps or individual strength sets.`

// userError turns a failure into a message that tells an agent what to do next.
func userError(err error) error {
	switch {
	case errors.Is(err, ErrNotAuthenticated):
		return errors.New("no Garmin API key has been provided. Ask the user to open this server's address in a browser and paste their Garmin Personal API key (or POST it to /auth), then retry")
	case errors.Is(err, auth.ErrInvalidKey), errors.Is(err, garmin.ErrUnauthorized):
		return errors.New("Garmin rejected the API key; it has probably expired or been deleted. Ask the user to create a new one at connect.garmin.com/app/settings/personalApiToken and provide it at /auth")
	case errors.Is(err, garmin.ErrRateLimited):
		return errors.New("Garmin is rate limiting requests; wait a little and retry")
	case errors.Is(err, garmin.ErrNotFound):
		return errors.New("Garmin has nothing for that request (not found); check the id or date")
	case errors.Is(err, garmin.ErrNoDistance):
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
