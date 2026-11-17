// Package mcpserver exposes the garmin client to AI agents over MCP, and an
// HTTP endpoint for providing the Garmin Personal API key.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dianabarsan/garmin-training/garmin-connect/auth"
)

// ErrNotAuthenticated is returned by tool calls made before an API key has
// been provided.
var ErrNotAuthenticated = errors.New("not authenticated: no Garmin Personal API key has been provided")

// KeyStore persists the API key across restarts. It is optional.
type KeyStore interface {
	// Load returns the stored key; ok is false when none is stored.
	Load() (key string, ok bool, err error)
	Save(key string) error
	Delete() error
}

// Status describes the credential state. It never contains the key itself.
type Status struct {
	Authenticated bool `json:"authenticated"`
	// TokenExpiresAt is when the current short-lived access token expires. A
	// fresh one is fetched automatically; it is zero before the first request.
	TokenExpiresAt time.Time `json:"token_expires_at,omitzero"`
	// Persisted reports whether the key is saved to disk.
	Persisted bool `json:"persisted"`
}

// Credentials holds the Garmin Personal API key. It satisfies
// garminconnect.TokenSource, so one garminconnect.Client keeps working when the key is
// replaced at runtime. The key is only ever held here and in the auth package;
// no method returns or logs it.
type Credentials struct {
	baseURL    string
	httpClient *http.Client
	store      KeyStore

	mu sync.RWMutex
	ts *auth.TokenSource
}

// CredentialsOption configures NewCredentials.
type CredentialsOption func(*Credentials)

// WithKeyStore persists keys that are set, and clears them on Clear.
func WithKeyStore(s KeyStore) CredentialsOption { return func(c *Credentials) { c.store = s } }

// WithAuthBaseURL overrides Garmin's host (for tests).
func WithAuthBaseURL(u string) CredentialsOption { return func(c *Credentials) { c.baseURL = u } }

// WithAuthHTTPClient sets the HTTP client used to validate keys.
func WithAuthHTTPClient(hc *http.Client) CredentialsOption {
	return func(c *Credentials) { c.httpClient = hc }
}

// NewCredentials returns an empty Credentials.
func NewCredentials(opts ...CredentialsOption) *Credentials {
	c := &Credentials{}
	for _, o := range opts {
		o(c)
	}
	return c
}

func (c *Credentials) newSource(key string) *auth.TokenSource {
	var opts []auth.Option
	if c.baseURL != "" {
		opts = append(opts, auth.WithBaseURL(c.baseURL))
	}
	if c.httpClient != nil {
		opts = append(opts, auth.WithHTTPClient(c.httpClient))
	}
	return auth.NewTokenSource(key, opts...)
}

// Set validates key by exchanging it with Garmin and, only if Garmin accepts
// it, makes it the active key (replacing any previous one) and persists it when
// a store is configured. A rejected key leaves the current credentials alone.
// It returns auth.ErrInvalidKey (wrapped) when Garmin rejects the key.
func (c *Credentials) Set(ctx context.Context, key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return auth.ErrNoAPIKey
	}
	ts := c.newSource(key)
	if _, err := ts.AccessToken(ctx); err != nil {
		return err
	}
	if c.store != nil {
		if err := c.store.Save(key); err != nil {
			return fmt.Errorf("saving key: %w", err)
		}
	}
	c.mu.Lock()
	c.ts = ts
	c.mu.Unlock()
	return nil
}

// LoadStored activates the key from the store without contacting Garmin, so a
// restart does not need the network. It reports whether a key was found.
func (c *Credentials) LoadStored() (bool, error) {
	if c.store == nil {
		return false, nil
	}
	key, ok, err := c.store.Load()
	if err != nil || !ok || strings.TrimSpace(key) == "" {
		return false, err
	}
	c.mu.Lock()
	c.ts = c.newSource(key)
	c.mu.Unlock()
	return true, nil
}

// Clear forgets the key, and deletes it from the store when there is one.
func (c *Credentials) Clear() error {
	c.mu.Lock()
	c.ts = nil
	c.mu.Unlock()
	if c.store != nil {
		return c.store.Delete()
	}
	return nil
}

// Status reports the credential state.
func (c *Credentials) Status() Status {
	c.mu.RLock()
	ts := c.ts
	c.mu.RUnlock()
	if ts == nil {
		return Status{Persisted: c.store != nil && c.hasStored()}
	}
	return Status{Authenticated: true, TokenExpiresAt: ts.ExpiresAt(), Persisted: c.store != nil && c.hasStored()}
}

func (c *Credentials) hasStored() bool {
	_, ok, err := c.store.Load()
	return err == nil && ok
}

// AccessToken implements garminconnect.TokenSource.
func (c *Credentials) AccessToken(ctx context.Context) (string, error) {
	c.mu.RLock()
	ts := c.ts
	c.mu.RUnlock()
	if ts == nil {
		return "", ErrNotAuthenticated
	}
	return ts.AccessToken(ctx)
}
