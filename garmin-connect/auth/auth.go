// Package auth exchanges a Garmin Connect Personal API key for short-lived
// access tokens and caches them in memory.
//
// Create a key at https://connect.garmin.com/app/settings/personalApiToken.
// The key cannot call data endpoints directly: it is exchanged for a JWT that
// is valid for up to 24 hours. Garmin does not issue refresh tokens, so
// renewing means exchanging the key again, which TokenSource does for you.
//
// TokenSource satisfies garminconnect.TokenSource:
//
//	ts := auth.NewTokenSource(os.Getenv("GARMIN_PERSONAL_API_KEY"))
//	c := garminconnect.New(garminconnect.WithTokenSource(ts))
package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dianabarsan/garmin-training/garmin-connect/internal/transport"
)

const (
	// DefaultBaseURL is the host serving the Personal API.
	DefaultBaseURL = "https://personal.connectapi.garmin.com"
	// tokenPath is the key-for-token exchange endpoint.
	tokenPath = "/personalapitoken-service/personal-api/personal-api-key/token"

	defaultRefreshBefore = 10 * time.Minute
	maxBody              = 1 << 20 // a JWT is a few KB; cap anything absurd
)

var (
	// ErrNoAPIKey is returned when the TokenSource has an empty key.
	ErrNoAPIKey = errors.New("auth: no API key configured")
	// ErrInvalidKey is returned when Garmin rejects the key (expired, deleted
	// or mistyped). Create a new key in Garmin Connect.
	ErrInvalidKey = errors.New("auth: API key rejected")
	// ErrMalformedToken is returned when the exchange response is not a JWT
	// carrying an "exp" claim.
	ErrMalformedToken = errors.New("auth: malformed access token")
	// ErrInsecureBaseURL is returned (wrapped) when WithBaseURL names a
	// plain-http host other than localhost: the key must not travel unencrypted.
	ErrInsecureBaseURL = transport.ErrInsecureURL
)

const (
	// attemptTimeout bounds each attempt of the exchange (see transport.Retry).
	attemptTimeout = 30 * time.Second
	// maxExp bounds the "exp" claim (the end of year 9999), so an absurd value
	// cannot overflow when converted to a time.
	maxExp = 253402300799
)

// cachedToken is an access token and its expiry.
type cachedToken struct {
	AccessToken string
	ExpiresAt   time.Time
	// IssuedAt is when the token was obtained. With ExpiresAt it gives the
	// token's lifetime, which bounds how early it is renewed.
	IssuedAt time.Time
}

// TokenSource returns access tokens for one API key. It is safe for
// concurrent use; concurrent callers share a single exchange.
type TokenSource struct {
	apiKey        string
	baseURL       string
	urlErr        error // from checking baseURL; returned by AccessToken
	httpClient    *http.Client
	now           func() time.Time
	refreshBefore time.Duration

	// sem is held by the one caller performing an exchange. Others wait on it
	// while honouring their own context.
	sem chan struct{}
	// waiting counts callers blocked on sem; tests read it.
	waiting atomic.Int32

	mu       sync.Mutex // guards the fields below
	token    cachedToken
	revoked  string // an access token reported invalid through Invalidate
	attempts uint64 // exchanges finished, successful or not
	lastErr  error  // error of the last exchange, nil if it succeeded
}

// Option configures a TokenSource.
type Option func(*TokenSource)

// WithBaseURL overrides the host, for tests and sandboxes. It must use https,
// except for a loopback host such as a local test server; otherwise
// AccessToken returns ErrInsecureBaseURL.
func WithBaseURL(u string) Option {
	return func(s *TokenSource) { s.baseURL = strings.TrimRight(u, "/") }
}

// WithHTTPClient sets the HTTP client used for the exchange. It is used as
// is: unlike the default client, it gets no retries and no per-attempt
// timeout, so give it a Timeout or bound AccessToken with a context.
func WithHTTPClient(hc *http.Client) Option {
	return func(s *TokenSource) {
		if hc != nil {
			s.httpClient = hc
		}
	}
}

// WithRefreshBefore sets how long before expiry a token is replaced. For a
// token whose whole lifetime is shorter than twice this, it is renewed halfway
// through its life instead, so a short-lived token is not exchanged on every
// call.
func WithRefreshBefore(d time.Duration) Option {
	return func(s *TokenSource) { s.refreshBefore = d }
}

// withClock replaces time.Now. Tests reach it through export_test.go.
func withClock(now func() time.Time) Option {
	return func(s *TokenSource) { s.now = now }
}

// NewTokenSource returns a TokenSource for apiKey.
func NewTokenSource(apiKey string, opts ...Option) *TokenSource {
	s := &TokenSource{
		apiKey:        strings.TrimSpace(apiKey),
		baseURL:       DefaultBaseURL,
		now:           time.Now,
		refreshBefore: defaultRefreshBefore,
		sem:           make(chan struct{}, 1),
	}
	for _, o := range opts {
		if o != nil {
			o(s)
		}
	}
	s.urlErr = transport.CheckBaseURL(s.baseURL)
	if s.httpClient == nil {
		// No overall timeout: each attempt is bounded instead, and the
		// caller's context bounds the whole exchange.
		s.httpClient = &http.Client{
			Transport: &transport.Retry{MaxRetries: 2, MinBackoff: 200 * time.Millisecond, MaxBackoff: 5 * time.Second, Timeout: attemptTimeout},
		}
	}
	return s
}

// AccessToken returns a valid access token, exchanging the API key when the
// cached one is missing, close to expiry, or was reported invalid.
//
// Only one exchange runs at a time. Callers that arrive meanwhile wait for it,
// giving up when their own context ends, and then share its result: its token,
// or its error, so a failing exchange is not repeated once per waiting caller.
func (s *TokenSource) AccessToken(ctx context.Context) (string, error) {
	if s.apiKey == "" {
		return "", ErrNoAPIKey
	}
	if s.urlErr != nil {
		return "", fmt.Errorf("auth: %w", s.urlErr)
	}
	s.mu.Lock()
	if s.usable(s.token) {
		tok := s.token.AccessToken
		s.mu.Unlock()
		return tok, nil
	}
	seen := s.attempts
	s.mu.Unlock()

	s.waiting.Add(1)
	select {
	case s.sem <- struct{}{}:
		s.waiting.Add(-1)
	case <-ctx.Done():
		s.waiting.Add(-1)
		return "", ctx.Err()
	}
	defer func() { <-s.sem }()

	s.mu.Lock()
	if s.usable(s.token) { // another caller just obtained one
		tok := s.token.AccessToken
		s.mu.Unlock()
		return tok, nil
	}
	if s.attempts > seen && s.lastErr != nil { // another caller just failed
		err := s.lastErr
		s.mu.Unlock()
		return "", err
	}
	s.mu.Unlock()

	t, err := s.exchange(ctx)
	s.mu.Lock()
	s.attempts++
	if err != nil {
		// A failure caused by this caller's own context says nothing about
		// the key or Garmin, so it is not handed to the callers waiting.
		s.lastErr = nil
		if ctx.Err() == nil {
			s.lastErr = err
		}
		s.mu.Unlock()
		return "", err
	}
	s.lastErr = nil
	s.token = t
	s.mu.Unlock()
	return t.AccessToken, nil
}

// Invalidate reports that token was refused by the API although it had not
// expired, for example because Garmin revoked it. The next AccessToken call
// exchanges the key for a new one instead of returning it again. It
// satisfies garminconnect.TokenInvalidator, which the client uses to recover
// from such a refusal on its own.
func (s *TokenSource) Invalidate(token string) {
	if token == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revoked = token
	if s.token.AccessToken == token {
		s.token = cachedToken{}
	}
}

// ExpiresAt returns when the currently cached access token expires, or the zero
// time when none is cached. It does not trigger an exchange.
func (s *TokenSource) ExpiresAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token.AccessToken == "" {
		return time.Time{}
	}
	return s.token.ExpiresAt
}

// usable reports whether t can be handed out now. The caller holds s.mu.
func (s *TokenSource) usable(t cachedToken) bool {
	return t.AccessToken != "" &&
		t.AccessToken != s.revoked &&
		s.now().Add(s.renewMargin(t)).Before(t.ExpiresAt)
}

// renewMargin is how long before expiry t is replaced: refreshBefore, or half
// the token's lifetime when that is shorter.
func (s *TokenSource) renewMargin(t cachedToken) time.Duration {
	m := s.refreshBefore
	if !t.IssuedAt.IsZero() {
		if half := t.ExpiresAt.Sub(t.IssuedAt) / 2; half >= 0 && half < m {
			m = half
		}
	}
	return m
}

func (s *TokenSource) exchange(ctx context.Context) (cachedToken, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+tokenPath, nil)
	if err != nil {
		return cachedToken{}, fmt.Errorf("auth: building request: %w", err)
	}
	req.Header.Set("Authorization", "ApiKey "+s.apiKey)
	req.Header.Set("Accept", "*/*")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return cachedToken{}, fmt.Errorf("auth: token exchange: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return cachedToken{}, fmt.Errorf("auth: reading token response: %w", err)
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return cachedToken{}, fmt.Errorf("%w (HTTP %d)", ErrInvalidKey, resp.StatusCode)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		// The body is deliberately omitted: it is not needed to diagnose a
		// status code and we never want credential-adjacent text in logs.
		return cachedToken{}, fmt.Errorf("auth: token exchange failed: HTTP %d", resp.StatusCode)
	}

	raw := strings.TrimSpace(string(body))
	exp, err := jwtExpiry(raw)
	if err != nil {
		return cachedToken{}, err
	}
	return cachedToken{AccessToken: raw, ExpiresAt: exp, IssuedAt: s.now()}, nil
}

// jwtExpiry reads the "exp" claim from a JWT without verifying its signature.
// Verification is the server's job; we only need to know when to renew.
func jwtExpiry(token string) (time.Time, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, fmt.Errorf("%w: expected 3 segments, got %d", ErrMalformedToken, len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: payload is not base64url", ErrMalformedToken)
	}
	var claims struct {
		Exp *float64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return time.Time{}, fmt.Errorf("%w: payload is not JSON", ErrMalformedToken)
	}
	if claims.Exp == nil {
		return time.Time{}, fmt.Errorf("%w: missing exp claim", ErrMalformedToken)
	}
	if e := *claims.Exp; !(e >= 0 && e <= maxExp) { // also rejects NaN
		return time.Time{}, fmt.Errorf("%w: exp claim out of range", ErrMalformedToken)
	}
	return time.Unix(int64(*claims.Exp), 0), nil
}
