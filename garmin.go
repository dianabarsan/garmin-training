// Package garmin is a client for the Garmin Connect Personal API, which gives
// you pull-based access to your own Garmin Connect data (activities, sleep,
// daily summaries, and so on).
//
// The Personal API is for a single user's own data and is not for commercial
// multi-user applications; see Garmin's terms. Authentication uses a personal
// API key exchanged for a short-lived token, which the auth package handles:
//
//	ts := auth.NewTokenSource(os.Getenv("GARMIN_PERSONAL_API_KEY"))
//	c := garmin.New(garmin.WithTokenSource(ts))
//	settings, err := c.UserSettings(ctx)
//
// Garmin publishes no response schemas for these endpoints, so methods return
// json.RawMessage until typed models are derived from real responses.
package garmin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dianabarsan/garmin-training/internal/transport"
)

// DefaultBaseURL is the host serving the Garmin Connect Personal API.
const DefaultBaseURL = "https://personal.connectapi.garmin.com"

// TokenSource supplies OAuth2 access tokens. Implementations must be safe for
// concurrent use and are responsible for refreshing expired tokens.
type TokenSource interface {
	AccessToken(ctx context.Context) (string, error)
}

// Client talks to the Garmin API. It is safe for concurrent use.
type Client struct {
	baseURL    string
	httpClient *http.Client
	tokens     TokenSource
	logger     *slog.Logger
	userAgent  string

	retries    int
	minBackoff time.Duration
	maxBackoff time.Duration
}

// New returns a Client configured by opts.
func New(opts ...Option) *Client {
	c := &Client{
		baseURL:    DefaultBaseURL,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		logger:     slog.New(slog.DiscardHandler),
		userAgent:  "garmin-training-go",
		retries:    3,
		minBackoff: 200 * time.Millisecond,
		maxBackoff: 10 * time.Second,
	}
	for _, o := range opts {
		o(c)
	}
	c.baseURL = strings.TrimRight(c.baseURL, "/")

	if c.retries > 0 {
		// Copy so the caller's http.Client is not mutated.
		hc := *c.httpClient
		hc.Transport = &transport.Retry{
			Base:       hc.Transport,
			MaxRetries: c.retries,
			MinBackoff: c.minBackoff,
			MaxBackoff: c.maxBackoff,
		}
		c.httpClient = &hc
	}
	return c
}

// UserSettings returns the authenticated user's profile settings.
//
// The path comes from Garmin's connect-personal-api-python client; the
// response shape is unverified until captured from a live account.
func (c *Client) UserSettings(ctx context.Context) (json.RawMessage, error) {
	var out json.RawMessage
	if err := c.get(ctx, "/userprofile-service/userprofile/settings", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// get performs an authenticated GET and decodes the JSON response into out.
func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	return c.do(ctx, http.MethodGet, path, q, out)
}

// do performs an authenticated request. A nil out discards the body.
func (c *Client) do(ctx context.Context, method, path string, q url.Values, out any) error {
	resp, err := c.send(ctx, method, path, q, "application/json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("garmin: %s %s: empty response body", method, path)
		}
		return fmt.Errorf("garmin: decoding %s response: %w", path, err)
	}
	return nil
}

// stream performs an authenticated GET and copies the response body to w.
func (c *Client) stream(ctx context.Context, path, accept string, w io.Writer) error {
	resp, err := c.send(ctx, http.MethodGet, path, nil, accept)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if _, err := io.Copy(w, resp.Body); err != nil {
		return fmt.Errorf("garmin: reading %s response: %w", path, err)
	}
	return nil
}

// send performs an authenticated request and returns the response when its
// status is 2xx. Other statuses become *APIError. The caller closes the body.
func (c *Client) send(ctx context.Context, method, path string, q url.Values, accept string) (*http.Response, error) {
	if c.tokens == nil {
		return nil, ErrNoTokenSource
	}
	tok, err := c.tokens.AccessToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("garmin: obtaining access token: %w", err)
	}

	u := c.baseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return nil, fmt.Errorf("garmin: building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", c.userAgent)

	c.logger.DebugContext(ctx, "garmin request", "method", method, "path", path)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("garmin: %s %s: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return nil, &APIError{StatusCode: resp.StatusCode, Method: method, URL: u, Body: string(b)}
	}
	return resp, nil
}
