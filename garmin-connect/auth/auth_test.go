package auth_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	garminconnect "github.com/dianabarsan/garmin-training/garmin-connect"
	"github.com/dianabarsan/garmin-training/garmin-connect/auth"
)

const tokenPath = "/personalapitoken-service/personal-api/personal-api-key/token"

var epoch = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

// makeJWT builds an unsigned JWT-shaped string with the given payload JSON.
func makeJWT(payload string) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(payload)) + ".sig"
}

func jwtExpiring(at time.Time) string { return makeJWT(fmt.Sprintf(`{"exp":%d}`, at.Unix())) }

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakeGarmin serves the token endpoint and counts exchanges.
type fakeGarmin struct {
	srv       *httptest.Server
	calls     atomic.Int32
	validKey  string
	expiresIn time.Duration
	clk       *clock
}

func newFake(t *testing.T, key string, clk *clock) *fakeGarmin {
	t.Helper()
	f := &fakeGarmin{validKey: key, expiresIn: 24 * time.Hour, clk: clk}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != tokenPath {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "ApiKey "+f.validKey {
			http.Error(w, "bad key", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(jwtExpiring(f.clk.Now().Add(f.expiresIn)) + "\n")) // trailing newline like a real body
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGarmin) source(key string, clk *clock, opts ...auth.Option) *auth.TokenSource {
	base := []auth.Option{auth.WithBaseURL(f.srv.URL), auth.WithClock(clk.Now)}
	return auth.NewTokenSource(key, append(base, opts...)...)
}

func TestTokenSource_ExchangesAndCaches(t *testing.T) {
	t.Parallel()
	clk := &clock{t: epoch}
	f := newFake(t, "key-1", clk)
	ts := f.source(" key-1 \n", clk) // surrounding whitespace is trimmed

	for range 3 {
		tok, err := ts.AccessToken(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(tok, "ey") || strings.ContainsAny(tok, " \n") {
			t.Fatalf("token = %q", tok)
		}
	}
	if got := f.calls.Load(); got != 1 {
		t.Errorf("exchanges = %d, want 1", got)
	}
}

func TestTokenSource_RenewsNearExpiry(t *testing.T) {
	t.Parallel()
	clk := &clock{t: epoch}
	f := newFake(t, "k", clk)
	ts := f.source("k", clk, auth.WithRefreshBefore(time.Hour))

	first, _ := ts.AccessToken(context.Background())

	clk.Advance(22 * time.Hour) // 2h left: still fine
	if _, _ = ts.AccessToken(context.Background()); f.calls.Load() != 1 {
		t.Fatalf("renewed too early, calls = %d", f.calls.Load())
	}

	clk.Advance(61 * time.Minute) // under 1h left: renew
	second, err := ts.AccessToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if f.calls.Load() != 2 {
		t.Errorf("calls = %d, want 2", f.calls.Load())
	}
	if first == second {
		t.Error("token was not replaced after renewal")
	}
}

func TestTokenSource_ConcurrentCallersShareOneExchange(t *testing.T) {
	t.Parallel()
	clk := &clock{t: epoch}
	f := newFake(t, "k", clk)
	ts := f.source("k", clk)

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := ts.AccessToken(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := f.calls.Load(); got != 1 {
		t.Errorf("exchanges = %d, want 1", got)
	}
}

func TestTokenSource_Errors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		handler http.HandlerFunc
		key     string
		want    error
	}{
		{"empty key", nil, "  ", auth.ErrNoAPIKey},
		{"unauthorized", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(401) }, "k", auth.ErrInvalidKey},
		{"forbidden", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(403) }, "k", auth.ErrInvalidKey},
		{"not a jwt", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("hello")) }, "k", auth.ErrMalformedToken},
		{"payload not base64", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("a.!!!.c")) }, "k", auth.ErrMalformedToken},
		{"payload not json", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(makeJWT("nope")))
		}, "k", auth.ErrMalformedToken},
		{"missing exp", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(makeJWT(`{"sub":"x"}`)))
		}, "k", auth.ErrMalformedToken},
		{"exp too large", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(makeJWT(`{"exp":1e300}`)))
		}, "k", auth.ErrMalformedToken},
		{"negative exp", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(makeJWT(`{"exp":-1}`)))
		}, "k", auth.ErrMalformedToken},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := tc.handler
			if h == nil {
				h = func(http.ResponseWriter, *http.Request) {}
			}
			srv := httptest.NewServer(h)
			t.Cleanup(srv.Close)
			ts := auth.NewTokenSource(tc.key, auth.WithBaseURL(srv.URL))
			_, err := ts.AccessToken(context.Background())
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestTokenSource_ServerErrorDoesNotLeakBodyOrKey(t *testing.T) {
	t.Parallel()
	const key = "super-secret-key"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "echo "+r.Header.Get("Authorization"), http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)
	_, err := auth.NewTokenSource(key, auth.WithBaseURL(srv.URL)).AccessToken(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), key) {
		t.Errorf("error leaks key: %v", err)
	}
}

func TestTokenSource_RetriesTransientFailure(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(jwtExpiring(time.Now().Add(time.Hour))))
	}))
	t.Cleanup(srv.Close)
	ts := auth.NewTokenSource("k", auth.WithBaseURL(srv.URL))
	if _, err := ts.AccessToken(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2", calls.Load())
	}
}

func TestTokenSource_ContextCancelled(t *testing.T) {
	t.Parallel()
	clk := &clock{t: epoch}
	f := newFake(t, "k", clk)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.source("k", clk).AccessToken(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestTokenSource_UsesStoreAcrossInstances(t *testing.T) {
	t.Parallel()
	clk := &clock{t: epoch}
	f := newFake(t, "k", clk)
	st := &auth.MemoryStore{}

	a, err := f.source("k", clk, auth.WithStore(st)).AccessToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// A new TokenSource (a "restart") must reuse the stored token.
	b, err := f.source("k", clk, auth.WithStore(st)).AccessToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if a != b || f.calls.Load() != 1 {
		t.Errorf("store not reused: same=%v calls=%d", a == b, f.calls.Load())
	}
}

func TestTokenSource_StoreIgnoredForDifferentKeyOrExpired(t *testing.T) {
	t.Parallel()
	clk := &clock{t: epoch}
	f := newFake(t, "new-key", clk)
	st := &auth.MemoryStore{}
	_ = st.Save(context.Background(), auth.Token{
		AccessToken:    jwtExpiring(epoch.Add(24 * time.Hour)),
		ExpiresAt:      epoch.Add(24 * time.Hour),
		KeyFingerprint: "fingerprint-of-some-other-key",
	})
	if _, err := f.source("new-key", clk, auth.WithStore(st)).AccessToken(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.calls.Load() != 1 {
		t.Errorf("stale-key token was reused (calls = %d)", f.calls.Load())
	}
}

type brokenStore struct{}

func (brokenStore) Load(context.Context) (auth.Token, bool, error) {
	return auth.Token{}, false, errors.New("disk on fire")
}
func (brokenStore) Save(context.Context, auth.Token) error { return errors.New("disk on fire") }

func TestTokenSource_BrokenStoreIsNotFatal(t *testing.T) {
	t.Parallel()
	clk := &clock{t: epoch}
	f := newFake(t, "k", clk)
	if _, err := f.source("k", clk, auth.WithStore(brokenStore{})).AccessToken(context.Background()); err != nil {
		t.Fatalf("broken store must not fail token retrieval: %v", err)
	}
}

func TestFileStore(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "nested", "dir", "token.json")
	st := auth.NewFileStore(path)

	if _, ok, err := st.Load(context.Background()); ok || err != nil {
		t.Fatalf("empty Load = ok:%v err:%v, want false,nil", ok, err)
	}

	want := auth.Token{AccessToken: "a.b.c", ExpiresAt: epoch.Add(time.Hour), KeyFingerprint: "fp"}
	if err := st.Save(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := st.Load(context.Background())
	if err != nil || !ok || got.AccessToken != want.AccessToken || !got.ExpiresAt.Equal(want.ExpiresAt) || got.KeyFingerprint != "fp" {
		t.Fatalf("Load = %+v ok:%v err:%v", got, ok, err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %v, want 0600", perm)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("leftover temp files: %v", entries)
	}

	// Overwrite works.
	want.AccessToken = "d.e.f"
	if err := st.Save(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := st.Load(context.Background()); got.AccessToken != "d.e.f" {
		t.Errorf("overwrite failed: %+v", got)
	}
}

func TestFileStore_CorruptFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "token.json")
	if err := os.WriteFile(path, []byte("{garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := auth.NewFileStore(path).Load(context.Background()); err == nil {
		t.Error("expected decode error")
	}
}

func TestFileStore_SaveFailsWhenDirIsAFile(t *testing.T) {
	t.Parallel()
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	st := auth.NewFileStore(filepath.Join(blocker, "token.json"))
	if err := st.Save(context.Background(), auth.Token{}); err == nil {
		t.Error("expected error")
	}
}

func TestDefaultBaseURLMatchesClient(t *testing.T) {
	t.Parallel()
	if auth.DefaultBaseURL != garminconnect.DefaultBaseURL {
		t.Errorf("auth.DefaultBaseURL %q != garminconnect.DefaultBaseURL %q", auth.DefaultBaseURL, garminconnect.DefaultBaseURL)
	}
}

// Compile-time check that the token source plugs into the client.
var _ garminconnect.TokenSource = (*auth.TokenSource)(nil)

func TestTokenSource_ExpiresAt(t *testing.T) {
	t.Parallel()
	clk := &clock{t: epoch}
	f := newFake(t, "k", clk)
	ts := f.source("k", clk)

	if got := ts.ExpiresAt(); !got.IsZero() {
		t.Errorf("before any exchange ExpiresAt = %v, want zero", got)
	}
	if f.calls.Load() != 0 {
		t.Error("ExpiresAt must not trigger an exchange")
	}
	if _, err := ts.AccessToken(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, want := ts.ExpiresAt(), epoch.Add(24*time.Hour); !got.Equal(want) {
		t.Errorf("ExpiresAt = %v, want %v", got, want)
	}
}

func TestTokenSource_InvalidateForcesANewExchange(t *testing.T) {
	t.Parallel()
	clk := &clock{t: epoch}
	f := newFake(t, "k", clk)
	st := &auth.MemoryStore{}
	ts := f.source("k", clk, auth.WithStore(st))
	first, err := ts.AccessToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Second) // so the new token differs
	ts.Invalidate(first)
	second, err := ts.AccessToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second == first || f.calls.Load() != 2 {
		t.Errorf("after Invalidate: same token=%v, exchanges=%d; want a new token from a second exchange", second == first, f.calls.Load())
	}
	// A fresh source sharing the store must not be handed the revoked token
	// by the same instance either: Invalidate on it also skips the store.
	ts2 := f.source("k", clk, auth.WithStore(st))
	ts2.Invalidate(second)
	clk.Advance(time.Second) // so a fresh exchange yields a different token
	if tok, _ := ts2.AccessToken(context.Background()); tok == second || f.calls.Load() != 3 {
		t.Errorf("a revoked token was loaded back from the store (exchanges = %d)", f.calls.Load())
	}
}

// A caller waiting for another caller's exchange gives up when its own
// context ends, instead of waiting for the exchange to finish.
func TestTokenSource_WaitingCallerHonoursItsContext(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
		}
		_, _ = w.Write([]byte(jwtExpiring(time.Now().Add(time.Hour))))
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	ts := auth.NewTokenSource("k", auth.WithBaseURL(srv.URL))

	go func() { _, _ = ts.AccessToken(context.Background()) }()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := ts.AccessToken(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("waited %v; should give up at its own deadline", took)
	}
}

// Callers queued behind a failing exchange get its error instead of each
// repeating the exchange against a server that is down.
func TestTokenSource_QueuedCallersShareAFailure(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			<-release
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	ts := auth.NewTokenSource("k", auth.WithBaseURL(srv.URL), auth.WithHTTPClient(&http.Client{}))

	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := ts.AccessToken(context.Background())
			errs <- err
		}()
	}
	// One caller is in the exchange; wait until the other nine are queued.
	for calls.Load() == 0 || ts.Waiting() < 9 {
		time.Sleep(time.Millisecond)
	}
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err == nil {
			t.Error("a caller got a token from a failing server")
		}
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("exchanges = %d, want 1 shared by every queued caller", got)
	}
}

// A token that lives less than twice the refresh margin is renewed halfway
// through its life, not on every call.
func TestTokenSource_ShortLivedTokenIsNotExchangedOnEveryCall(t *testing.T) {
	t.Parallel()
	clk := &clock{t: epoch}
	f := newFake(t, "k", clk)
	f.expiresIn = 5 * time.Minute // shorter than the default 10-minute margin
	ts := f.source("k", clk)
	for range 3 {
		if _, err := ts.AccessToken(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if f.calls.Load() != 1 {
		t.Fatalf("exchanges = %d, want 1", f.calls.Load())
	}
	clk.Advance(3 * time.Minute) // past half its life
	if _, err := ts.AccessToken(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.calls.Load() != 2 {
		t.Errorf("exchanges = %d, want 2 after half the lifetime", f.calls.Load())
	}
}

func TestTokenSource_RefusesPlainHTTPToARemoteHost(t *testing.T) {
	t.Parallel()
	for _, u := range []string{"http://example.com", "http://192.0.2.1:8080"} {
		_, err := auth.NewTokenSource("k", auth.WithBaseURL(u)).AccessToken(context.Background())
		if !errors.Is(err, auth.ErrInsecureBaseURL) {
			t.Errorf("%s: err = %v, want ErrInsecureBaseURL", u, err)
		}
	}
}

func TestTokenSource_StoredTokenIsSalted(t *testing.T) {
	t.Parallel()
	clk := &clock{t: epoch}
	f := newFake(t, "k", clk)
	st := &auth.MemoryStore{}
	if _, err := f.source("k", clk, auth.WithStore(st)).AccessToken(context.Background()); err != nil {
		t.Fatal(err)
	}
	a, _, _ := st.Load(context.Background())
	if a.KeySalt == "" || a.KeyFingerprint == "" || a.IssuedAt.IsZero() {
		t.Fatalf("stored token = %+v", a)
	}
	// The same key gets a different fingerprint each time, so the file cannot
	// be matched against a precomputed hash of a guessed key.
	st2 := &auth.MemoryStore{}
	clk.Advance(time.Second)
	if _, err := f.source("k", clk, auth.WithStore(st2)).AccessToken(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, _, _ := st2.Load(context.Background())
	if a.KeyFingerprint == b.KeyFingerprint || a.KeySalt == b.KeySalt {
		t.Error("two tokens for the same key share a fingerprint")
	}
}

func TestFileStore_LoadMakesALooseFilePrivate(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "token.json")
	if err := os.WriteFile(path, []byte(`{"access_token":"a.b.c"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(path, 0o644)
	if _, ok, err := auth.NewFileStore(path).Load(context.Background()); !ok || err != nil {
		t.Fatalf("Load: ok=%v err=%v", ok, err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestWithStore_NilPointerMeansNoStore(t *testing.T) {
	t.Parallel()
	clk := &clock{t: epoch}
	f := newFake(t, "k", clk)
	for _, st := range []auth.Store{(*auth.FileStore)(nil), (*auth.MemoryStore)(nil), (*customStore)(nil), nil} {
		if _, err := f.source("k", clk, auth.WithStore(st)).AccessToken(context.Background()); err != nil {
			t.Errorf("%T: %v", st, err)
		}
	}
}

// customStore is a Store type the auth package does not know about.
type customStore struct{ auth.MemoryStore }
