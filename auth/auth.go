// Package auth exchanges a Garmin Connect Personal API key for short-lived
// access tokens and caches them.
//
// Create a key at https://connect.garmin.com/app/settings/personalApiToken.
// The key cannot call data endpoints directly: it is exchanged for a JWT that
// is valid for up to 24 hours. Garmin does not issue refresh tokens, so
// renewing means exchanging the key again, which TokenSource does for you.
//
// TokenSource satisfies garmin.TokenSource:
//
//	ts := auth.NewTokenSource(os.Getenv("GARMIN_PERSONAL_API_KEY"))
//	c := garmin.New(garmin.WithTokenSource(ts))
package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dianabarsan/garmin-training/internal/transport"
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
)

// Token is an access token and its expiry.
type Token struct {
	AccessToken string    `json:"access_token"`
	ExpiresAt   time.Time `json:"expires_at"`
	// KeyFingerprint ties a cached token to the key and host that produced
	// it, so changing either invalidates the cache. It is a one-way hash.
	KeyFingerprint string `json:"key_fingerprint"`
}

// TokenSource returns access tokens for one API key. It is safe for
// concurrent use; concurrent callers share a single exchange.
type TokenSource struct {
	apiKey        string
	baseURL       string
	httpClient    *http.Client
	store         Store
	now           func() time.Time
	refreshBefore time.Duration

	mu    sync.Mutex
	token Token
}

// Option configures a TokenSource.
type Option func(*TokenSource)

// WithBaseURL overrides the host (for tests).
func WithBaseURL(u string) Option {
	return func(s *TokenSource) { s.baseURL = strings.TrimRight(u, "/") }
}

// WithHTTPClient sets the HTTP client used for the exchange.
func WithHTTPClient(hc *http.Client) Option {
	return func(s *TokenSource) {
		if hc != nil {
			s.httpClient = hc
		}
	}
}

// WithStore persists tokens across process restarts.
func WithStore(st Store) Option {
	return func(s *TokenSource) { s.store = st }
}

// WithRefreshBefore sets how long before expiry a token is replaced.
func WithRefreshBefore(d time.Duration) Option {
	return func(s *TokenSource) { s.refreshBefore = d }
}

// WithClock replaces time.Now (for tests).
func WithClock(now func() time.Time) Option {
	return func(s *TokenSource) { s.now = now }
}

// NewTokenSource returns a TokenSource for apiKey.
func NewTokenSource(apiKey string, opts ...Option) *TokenSource {
	s := &TokenSource{
		apiKey:        strings.TrimSpace(apiKey),
		baseURL:       DefaultBaseURL,
		now:           time.Now,
		refreshBefore: defaultRefreshBefore,
	}
	for _, o := range opts {
		o(s)
	}
	if s.httpClient == nil {
		s.httpClient = &http.Client{
			Timeout:   30 * time.Second,
			Transport: &transport.Retry{MaxRetries: 2, MinBackoff: 200 * time.Millisecond, MaxBackoff: 5 * time.Second},
		}
	}
	return s
}

// AccessToken returns a valid access token, exchanging the API key when the
// cached one is missing or close to expiry.
func (s *TokenSource) AccessToken(ctx context.Context) (string, error) {
	if s.apiKey == "" {
		return "", ErrNoAPIKey
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	fp := s.fingerprint()
	if s.usable(s.token, fp) {
		return s.token.AccessToken, nil
	}
	if s.store != nil {
		if t, ok, err := s.store.Load(ctx); err == nil && ok && s.usable(t, fp) {
			s.token = t
			return t.AccessToken, nil
		}
		// A broken cache must never block fetching a fresh token.
	}

	t, err := s.exchange(ctx)
	if err != nil {
		return "", err
	}
	t.KeyFingerprint = fp
	s.token = t
	if s.store != nil {
		if err := s.store.Save(ctx, t); err != nil {
			// The token is good; failing to cache it is not fatal.
			return t.AccessToken, nil
		}
	}
	return t.AccessToken, nil
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

func (s *TokenSource) usable(t Token, fp string) bool {
	return t.AccessToken != "" &&
		t.KeyFingerprint == fp &&
		s.now().Add(s.refreshBefore).Before(t.ExpiresAt)
}

func (s *TokenSource) fingerprint() string {
	sum := sha256.Sum256([]byte(s.baseURL + "\x00" + s.apiKey))
	return hex.EncodeToString(sum[:])
}

func (s *TokenSource) exchange(ctx context.Context) (Token, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+tokenPath, nil)
	if err != nil {
		return Token{}, fmt.Errorf("auth: building request: %w", err)
	}
	req.Header.Set("Authorization", "ApiKey "+s.apiKey)
	req.Header.Set("Accept", "*/*")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return Token{}, fmt.Errorf("auth: token exchange: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return Token{}, fmt.Errorf("auth: reading token response: %w", err)
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return Token{}, fmt.Errorf("%w (HTTP %d)", ErrInvalidKey, resp.StatusCode)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		// The body is deliberately omitted: it is not needed to diagnose a
		// status code and we never want credential-adjacent text in logs.
		return Token{}, fmt.Errorf("auth: token exchange failed: HTTP %d", resp.StatusCode)
	}

	raw := strings.TrimSpace(string(body))
	exp, err := jwtExpiry(raw)
	if err != nil {
		return Token{}, err
	}
	return Token{AccessToken: raw, ExpiresAt: exp}, nil
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
	return time.Unix(int64(*claims.Exp), 0), nil
}
