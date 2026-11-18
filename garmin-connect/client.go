// Package garminconnect is a client for the Garmin Connect Personal API, which gives
// you pull-based access to your own Garmin Connect data (activities, sleep,
// daily summaries, and so on).
//
// The Personal API is for a single user's own data and is not for commercial
// multi-user applications; see Garmin's terms. Authentication uses a personal
// API key exchanged for a short-lived token, which the auth package handles:
//
//	ts := auth.NewTokenSource(os.Getenv("GARMIN_PERSONAL_API_KEY"))
//	c := garminconnect.New(garminconnect.WithTokenSource(ts))
//	settings, err := c.UserSettings(ctx)
//
// Garmin publishes no response schemas, so the typed models cover the fields
// that have been verified against real responses, and every model keeps the
// original object, available through its Raw method.
//
// The models are for decoding Garmin's responses. Their JSON tags describe how
// they encode, which is not Garmin's format, so encoding a model and decoding
// it again does not give back the same value. To store or forward a response,
// keep Raw(): it decodes into the same model.
//
// Build a Client with New; the zero Client has no token source and every
// request fails with ErrNoTokenSource.
package garminconnect

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/dianabarsan/garmin-training/garmin-connect/internal/transport"
)

// DefaultBaseURL is the host serving the Garmin Connect Personal API.
const DefaultBaseURL = "https://personal.connectapi.garmin.com"

// TokenSource supplies OAuth2 access tokens. Implementations must be safe for
// concurrent use and are responsible for refreshing expired tokens.
type TokenSource interface {
	AccessToken(ctx context.Context) (string, error)
}

// TokenInvalidator is optionally implemented by a TokenSource. When the API
// refuses a token with HTTP 401, the client calls Invalidate with it and
// retries the request once with a fresh token, so a token revoked before its
// expiry does not keep failing until then. Concurrent requests refused with
// the same token invalidate it only once and all retry with its replacement.
// A different token is invalidated at most once a minute: a 401 soon after is
// returned as is, so an endpoint that keeps refusing does not cost a key
// exchange per request. Invalidate must return promptly and must not use the
// Client: other requests refused at the same time wait for it.
// *auth.TokenSource implements it.
type TokenInvalidator interface {
	Invalidate(token string)
}

// Default limits; see WithMaxResponseBytes and WithAttemptTimeout.
const (
	DefaultMaxResponseBytes = 64 << 20
	DefaultAttemptTimeout   = 30 * time.Second
)

// Client talks to the Garmin API. It is safe for concurrent use. Create it
// with New.
type Client struct {
	baseURL    string
	urlErr     error // from checking baseURL; returned by every request
	httpClient *http.Client
	tokens     TokenSource
	logger     *slog.Logger
	userAgent  string

	retries        int
	minBackoff     time.Duration
	maxBackoff     time.Duration
	attemptTimeout time.Duration
	maxResponse    int64

	// invMu guards the token a 401 last made the client invalidate and when
	// (see invalidate).
	invMu         sync.Mutex
	invalidated   string
	invalidatedAt time.Time
}

// invalidateEvery limits how often a 401 makes the client invalidate a token.
// A token refused again so soon after a fresh one was obtained is unlikely to
// be at fault, and replacing it would cost a key exchange per request.
const invalidateEvery = time.Minute

// New returns a Client configured by opts.
//
// Requests have no overall timeout of their own: bound them with the context
// you pass. Each attempt is bounded instead (see WithAttemptTimeout), so a
// server that stops answering is retried, while a large download on a working
// connection is never cut off.
func New(opts ...Option) *Client {
	c := &Client{
		baseURL:        DefaultBaseURL,
		httpClient:     &http.Client{},
		logger:         slog.New(slog.DiscardHandler),
		userAgent:      "garmin-training-go",
		retries:        3,
		minBackoff:     200 * time.Millisecond,
		maxBackoff:     10 * time.Second,
		attemptTimeout: DefaultAttemptTimeout,
		maxResponse:    DefaultMaxResponseBytes,
	}
	for _, o := range opts {
		if o != nil {
			o(c)
		}
	}
	c.baseURL = strings.TrimRight(c.baseURL, "/")
	c.urlErr = transport.CheckBaseURL(c.baseURL)

	// Copy so the caller's http.Client is not mutated. The retry transport is
	// installed even with retries disabled, for its per-attempt timeout.
	hc := *c.httpClient
	hc.Transport = &transport.Retry{
		Base:       hc.Transport,
		MaxRetries: max(c.retries, 0),
		MinBackoff: c.minBackoff,
		MaxBackoff: c.maxBackoff,
		Timeout:    c.attemptTimeout,
	}
	c.httpClient = &hc
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

// do performs an authenticated request. A nil out discards the body. The body
// must be exactly one JSON value no larger than the client's response limit.
//
// A response whose body stops arriving for the attempt timeout is requested
// again, up to the client's retry count: the transport only retries failures
// it sees before handing the response over.
func (c *Client) do(ctx context.Context, method, path string, q url.Values, out any) error {
	for attempt := 0; ; attempt++ {
		err := c.doOnce(ctx, method, path, q, out)
		var stalled *stalledError
		if attempt >= c.retries || !errors.As(err, &stalled) || ctx.Err() != nil {
			return err
		}
		c.logger.DebugContext(ctx, "garmin response stalled; requesting it again", "path", path)
		// Drop anything decoded from the partial body before decoding again.
		if v := reflect.ValueOf(out); v.Kind() == reflect.Pointer && !v.IsNil() {
			v.Elem().SetZero()
		}
	}
}

// stalledError marks a doOnce failure caused by the body stalling, which do
// retries. It wraps the error (which matches ErrAttemptTimeout) unchanged.
type stalledError struct{ err error }

func (e *stalledError) Error() string { return e.err.Error() }
func (e *stalledError) Unwrap() error { return e.err }

// doOnce performs one request for do.
func (c *Client) doOnce(ctx context.Context, method, path string, q url.Values, out any) error {
	resp, err := c.send(ctx, method, path, q, "application/json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// HTTP 204 is how Garmin says "no data for that date". Leave out untouched
	// so the caller sees its zero value, instead of failing on an empty body.
	if out == nil || resp.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
		return nil
	}
	body := io.Reader(resp.Body)
	var limited *io.LimitedReader
	if c.maxResponse > 0 {
		limited = &io.LimitedReader{R: resp.Body, N: c.maxResponse + 1}
		body = limited
	}
	tooLarge := func() bool { return limited != nil && limited.N <= 0 }

	dec := json.NewDecoder(body)
	err = dec.Decode(out)
	if err == nil {
		// Anything after the value means the response is not what it seems.
		if _, terr := dec.Token(); !errors.Is(terr, io.EOF) {
			err = cmp.Or(terr, errors.New("unexpected data after the JSON value"))
		}
	}
	switch {
	case err == nil:
		return nil
	case tooLarge():
		return fmt.Errorf("garmin: %s %s: %w (limit %d bytes)", method, path, ErrResponseTooLarge, c.maxResponse)
	case errors.Is(err, ErrAttemptTimeout):
		return &stalledError{fmt.Errorf("garmin: reading %s response: %w", path, err)}
	case errors.Is(err, io.EOF):
		return fmt.Errorf("garmin: %s %s: %w", method, path, ErrEmptyResponse)
	default:
		return fmt.Errorf("garmin: decoding %s response: %w", path, err)
	}
}

// stream performs an authenticated GET and copies the response body to w. It
// is not subject to the response size limit. A body that stalls is not
// requested again, because part of it has already been written to w.
func (c *Client) stream(ctx context.Context, path, accept string, w io.Writer) error {
	resp, err := c.send(ctx, http.MethodGet, path, nil, accept)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return fmt.Errorf("garmin: GET %s: %w", path, ErrEmptyResponse)
	}
	ew := &errWriter{w: w}
	if _, err := io.Copy(ew, resp.Body); err != nil {
		if ew.err != nil {
			return fmt.Errorf("garmin: writing %s response: %w", path, ew.err)
		}
		return fmt.Errorf("garmin: reading %s response: %w", path, err)
	}
	return nil
}

// errWriter remembers a write error, so a failed copy can say which side
// failed.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) Write(p []byte) (int, error) {
	n, err := e.w.Write(p)
	if err != nil {
		e.err = err
	}
	return n, err
}

// send performs an authenticated request and returns the response when its
// status is 2xx. Other statuses become *APIError. The caller closes the body.
// A 401 for a token the source can invalidate is retried once, if the source
// then hands out a different token (see invalidate).
func (c *Client) send(ctx context.Context, method, path string, q url.Values, accept string) (*http.Response, error) {
	if c.tokens == nil {
		return nil, ErrNoTokenSource
	}
	if c.urlErr != nil {
		return nil, fmt.Errorf("garmin: %w", c.urlErr)
	}
	u := c.baseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	inv, canInvalidate := c.tokens.(TokenInvalidator)
	var refused string    // the token a 401 was given for
	var refusal *APIError // that 401
	for {
		tok, err := c.tokens.AccessToken(ctx)
		if err != nil {
			return nil, fmt.Errorf("garmin: obtaining access token: %w", err)
		}
		if refused != "" && tok == refused {
			// The source kept the token, so resending would only repeat the 401.
			return nil, refusal
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
		if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
			return resp, nil
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		resp.Body.Close()
		apiErr := &APIError{StatusCode: resp.StatusCode, Method: method, URL: u, Body: string(bytes.ToValidUTF8(b, nil))}
		if resp.StatusCode == http.StatusUnauthorized && canInvalidate && refused == "" {
			c.logger.DebugContext(ctx, "garmin refused the access token; retrying with a new one", "path", path)
			c.invalidate(inv, tok)
			refused, refusal = tok, apiErr
			continue
		}
		return nil, apiErr
	}
}

// invalidate invalidates tok after a 401, unless another request already
// invalidated it, or invalidated a different token less than invalidateEvery
// ago. Requests refused together wait here until the first has invalidated
// the token, so they all ask the source for its replacement. The elapsed time
// is read from the monotonic clock, so a change to the system clock does not
// affect it.
func (c *Client) invalidate(inv TokenInvalidator, tok string) {
	c.invMu.Lock()
	defer c.invMu.Unlock()
	if tok == c.invalidated {
		return
	}
	if !c.invalidatedAt.IsZero() && time.Since(c.invalidatedAt) < invalidateEvery {
		return
	}
	inv.Invalidate(tok)
	c.invalidated, c.invalidatedAt = tok, time.Now()
}
