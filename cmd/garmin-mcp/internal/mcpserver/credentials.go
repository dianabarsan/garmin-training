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

// ErrKeyNotSaved is returned (wrapped) by Set when Garmin accepted the key but
// the key store could not save it. Set does not activate the key in that case;
// a caller that wants to run with it anyway, as the server does at startup for
// the key in its own configuration, can activate it with UseUnverified.
var ErrKeyNotSaved = errors.New("the key could not be saved")

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

	// writeMu serialises the operations that change the key (Set, UseUnverified,
	// LoadStored, Clear), so the key store and the active key always agree.
	writeMu sync.Mutex

	mu sync.RWMutex // guards the fields below
	ts *auth.TokenSource
	// persisted caches whether the store holds a key, so Status needs no file
	// I/O. It is read from the store once, on first use, then kept up to date.
	persisted      bool
	persistedKnown bool
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
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	ts := c.newSource(key)
	if _, err := ts.AccessToken(ctx); err != nil {
		return err
	}
	if c.store != nil {
		if err := c.store.Save(key); err != nil {
			return fmt.Errorf("%w: %w", ErrKeyNotSaved, err)
		}
	}
	c.mu.Lock()
	c.ts = ts
	if c.store != nil {
		c.persisted, c.persistedKnown = true, true
	}
	c.mu.Unlock()
	return nil
}

// UseUnverified activates key without contacting Garmin and without saving it.
// It is for a key from the server's own configuration that could not be
// checked at startup (for example while the network is down): the key is
// checked on the first request instead, and a rejection is reported then.
func (c *Credentials) UseUnverified(key string) {
	key = strings.TrimSpace(key)
	if key == "" {
		return
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.mu.Lock()
	c.ts = c.newSource(key)
	c.mu.Unlock()
}

// LoadStored activates the key from the store without contacting Garmin, so a
// restart does not need the network. It reports whether a key was found.
func (c *Credentials) LoadStored() (bool, error) {
	if c.store == nil {
		return false, nil
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	key, ok, err := c.store.Load()
	if err != nil {
		return false, err
	}
	ok = ok && strings.TrimSpace(key) != ""
	c.mu.Lock()
	c.persisted, c.persistedKnown = ok, true
	if ok {
		c.ts = c.newSource(key)
	}
	c.mu.Unlock()
	return ok, nil
}

// Clear forgets the key, and deletes it from the store when there is one. The
// key is forgotten in memory even if deleting the stored copy fails; the error
// then says so, and Status still reports it as persisted.
func (c *Credentials) Clear() error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.mu.Lock()
	c.ts = nil
	c.mu.Unlock()
	if c.store == nil {
		return nil
	}
	if err := c.store.Delete(); err != nil {
		return err
	}
	c.mu.Lock()
	c.persisted, c.persistedKnown = false, true
	c.mu.Unlock()
	return nil
}

// Status reports the credential state.
func (c *Credentials) Status() Status {
	c.mu.RLock()
	ts, persisted, known := c.ts, c.persisted, c.persistedKnown
	c.mu.RUnlock()
	if c.store != nil && !known {
		persisted = c.loadPersisted()
	}
	if ts == nil {
		return Status{Persisted: persisted}
	}
	return Status{Authenticated: true, TokenExpiresAt: ts.ExpiresAt(), Persisted: persisted}
}

// loadPersisted reads, once, whether the store holds a key. It does not take
// writeMu, so it never waits for a Set that is talking to Garmin; holding mu is
// enough, because writers update the flag under mu after changing the store.
func (c *Credentials) loadPersisted() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.persistedKnown {
		_, ok, err := c.store.Load()
		c.persisted, c.persistedKnown = err == nil && ok, err == nil
	}
	return c.persisted
}

// Invalidate implements garminconnect.TokenInvalidator: the client calls it
// when Garmin refuses a token that has not expired, and then retries with a
// fresh one.
func (c *Credentials) Invalidate(token string) {
	c.mu.RLock()
	ts := c.ts
	c.mu.RUnlock()
	if ts != nil {
		ts.Invalidate(token)
	}
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
