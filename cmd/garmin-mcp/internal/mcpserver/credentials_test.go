package mcpserver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/dianabarsan/garmin-training/garmin-connect/auth"
)

func TestCredentials_NotAuthenticatedUntilSet(t *testing.T) {
	t.Parallel()
	c := NewCredentials(WithAuthBaseURL(newFakeGarmin(t).srv.URL))
	if c.Status().Authenticated {
		t.Error("a new Credentials must not be authenticated")
	}
	if _, err := c.AccessToken(context.Background()); !errors.Is(err, ErrNotAuthenticated) {
		t.Errorf("err = %v, want ErrNotAuthenticated", err)
	}
}

func TestCredentials_SetValidatesWithGarmin(t *testing.T) {
	t.Parallel()
	fg := newFakeGarmin(t)
	c := NewCredentials(WithAuthBaseURL(fg.srv.URL))

	if err := c.Set(context.Background(), "wrong-key"); !errors.Is(err, auth.ErrInvalidKey) {
		t.Fatalf("bad key err = %v, want ErrInvalidKey", err)
	}
	if c.Status().Authenticated {
		t.Error("a rejected key must not authenticate")
	}

	if err := c.Set(context.Background(), "  "+validKey+"\n"); err != nil { // surrounding whitespace is trimmed
		t.Fatal(err)
	}
	st := c.Status()
	if !st.Authenticated || st.TokenExpiresAt.IsZero() {
		t.Errorf("status = %+v", st)
	}
	tok, err := c.AccessToken(context.Background())
	if err != nil || tok == "" {
		t.Errorf("token = %q err %v", tok, err)
	}
	if err := c.Set(context.Background(), ""); !errors.Is(err, auth.ErrNoAPIKey) {
		t.Errorf("empty key err = %v", err)
	}
}

func TestCredentials_RejectedKeyKeepsTheWorkingOne(t *testing.T) {
	t.Parallel()
	fg := newFakeGarmin(t)
	c := NewCredentials(WithAuthBaseURL(fg.srv.URL))
	if err := c.Set(context.Background(), validKey); err != nil {
		t.Fatal(err)
	}
	if err := c.Set(context.Background(), "typo"); err == nil {
		t.Fatal("expected rejection")
	}
	if !c.Status().Authenticated {
		t.Error("a failed replacement must not log you out")
	}
	if _, err := c.AccessToken(context.Background()); err != nil {
		t.Errorf("old key stopped working: %v", err)
	}
}

func TestCredentials_Clear(t *testing.T) {
	t.Parallel()
	c := NewCredentials(WithAuthBaseURL(newFakeGarmin(t).srv.URL))
	_ = c.Set(context.Background(), validKey)
	if err := c.Clear(); err != nil {
		t.Fatal(err)
	}
	if c.Status().Authenticated {
		t.Error("still authenticated after Clear")
	}
	if _, err := c.AccessToken(context.Background()); !errors.Is(err, ErrNotAuthenticated) {
		t.Errorf("err = %v", err)
	}
}

func TestCredentials_PersistenceRoundTrip(t *testing.T) {
	t.Parallel()
	fg := newFakeGarmin(t)
	path := filepath.Join(t.TempDir(), "sub", "garmin.key")

	c1 := NewCredentials(WithAuthBaseURL(fg.srv.URL), WithKeyStore(NewFileKeyStore(path)))
	if err := c1.Set(context.Background(), validKey); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("key file mode = %v, want 0600", perm)
	}
	if dirInfo, _ := os.Stat(filepath.Dir(path)); dirInfo.Mode().Perm() != 0o700 {
		t.Errorf("key dir mode = %v, want 0700", dirInfo.Mode().Perm())
	}
	if !c1.Status().Persisted {
		t.Error("Persisted should be true")
	}

	// A "restart": new Credentials, same file. No Garmin call is needed to load.
	before := fg.exchangeCount()
	c2 := NewCredentials(WithAuthBaseURL(fg.srv.URL), WithKeyStore(NewFileKeyStore(path)))
	ok, err := c2.LoadStored()
	if err != nil || !ok {
		t.Fatalf("LoadStored = %v, %v", ok, err)
	}
	if fg.exchangeCount() != before {
		t.Error("LoadStored must not contact Garmin")
	}
	if _, err := c2.AccessToken(context.Background()); err != nil {
		t.Errorf("stored key does not work: %v", err)
	}

	// Clear removes the file.
	if err := c2.Clear(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("key file still exists after Clear: %v", err)
	}
	if err := c2.Clear(); err != nil {
		t.Errorf("clearing twice must not fail: %v", err)
	}
}

func TestCredentials_RejectedKeyIsNotPersisted(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "garmin.key")
	c := NewCredentials(WithAuthBaseURL(newFakeGarmin(t).srv.URL), WithKeyStore(NewFileKeyStore(path)))
	if err := c.Set(context.Background(), "wrong"); err == nil {
		t.Fatal("expected rejection")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a rejected key was written to disk: %v", err)
	}
}

func TestCredentials_LoadStoredWithoutStoreOrFile(t *testing.T) {
	t.Parallel()
	if ok, err := NewCredentials().LoadStored(); ok || err != nil {
		t.Errorf("no store: %v %v", ok, err)
	}
	c := NewCredentials(WithKeyStore(NewFileKeyStore(filepath.Join(t.TempDir(), "absent"))))
	if ok, err := c.LoadStored(); ok || err != nil {
		t.Errorf("absent file: %v %v", ok, err)
	}
	empty := filepath.Join(t.TempDir(), "empty")
	_ = os.WriteFile(empty, []byte("  \n"), 0o600)
	if ok, err := NewCredentials(WithKeyStore(NewFileKeyStore(empty))).LoadStored(); ok || err != nil {
		t.Errorf("blank file: %v %v", ok, err)
	}
}

func TestCredentials_ConcurrentUse(t *testing.T) {
	t.Parallel()
	fg := newFakeGarmin(t)
	c := NewCredentials(WithAuthBaseURL(fg.srv.URL))
	_ = c.Set(context.Background(), validKey)
	var wg sync.WaitGroup
	for i := range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch i % 4 {
			case 0:
				_ = c.Set(context.Background(), validKey)
			case 1:
				_ = c.Status()
			case 2:
				_, _ = c.AccessToken(context.Background())
			case 3:
				if i%8 == 7 {
					_ = c.Clear()
				}
			}
		}()
	}
	wg.Wait() // the race detector is the assertion
}

// countingStore is an in-memory KeyStore that counts loads and can fail saves.
type countingStore struct {
	mu       sync.Mutex
	key      string
	loads    int
	failSave bool
}

func (s *countingStore) Load() (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loads++
	return s.key, s.key != "", nil
}

func (s *countingStore) Save(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failSave {
		return errors.New("disk full")
	}
	s.key = key
	return nil
}

func (s *countingStore) Delete() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.key = ""
	return nil
}

func TestCredentials_StatusDoesNotReadTheStoreEveryTime(t *testing.T) {
	t.Parallel()
	st := &countingStore{key: validKey}
	c := NewCredentials(WithAuthBaseURL(newFakeGarmin(t).srv.URL), WithKeyStore(st))
	for range 5 {
		if !c.Status().Persisted {
			t.Fatal("Persisted should be true")
		}
	}
	if st.loads != 1 {
		t.Errorf("store loaded %d times, want 1", st.loads)
	}
	if err := c.Clear(); err != nil {
		t.Fatal(err)
	}
	if c.Status().Persisted {
		t.Error("Persisted should be false after Clear")
	}
	if err := c.Set(context.Background(), validKey); err != nil {
		t.Fatal(err)
	}
	if !c.Status().Persisted || st.loads != 1 {
		t.Errorf("after Set: persisted=%v loads=%d", c.Status().Persisted, st.loads)
	}
}

func TestCredentials_KeyThatCannotBeSavedIsNotActivated(t *testing.T) {
	t.Parallel()
	c := NewCredentials(WithAuthBaseURL(newFakeGarmin(t).srv.URL), WithKeyStore(&countingStore{failSave: true}))
	err := c.Set(context.Background(), validKey)
	if !errors.Is(err, ErrKeyNotSaved) {
		t.Fatalf("err = %v, want ErrKeyNotSaved", err)
	}
	if c.Status().Authenticated {
		t.Error("a key that could not be saved must not be activated")
	}
}

func TestCredentials_UseUnverifiedActivatesWithoutContactingGarmin(t *testing.T) {
	t.Parallel()
	fg := newFakeGarmin(t)
	st := &countingStore{}
	c := NewCredentials(WithAuthBaseURL(fg.srv.URL), WithKeyStore(st))
	c.UseUnverified(validKey)
	if !c.Status().Authenticated || fg.exchangeCount() != 0 {
		t.Fatalf("authenticated=%v exchanges=%d", c.Status().Authenticated, fg.exchangeCount())
	}
	if st.key != "" {
		t.Error("an unverified key must not be saved")
	}
	if _, err := c.AccessToken(context.Background()); err != nil {
		t.Errorf("the key should be checked on first use and work: %v", err)
	}
}

// Set and Clear running at the same time must leave the store and the active
// key in agreement: both holding the key, or neither.
func TestCredentials_ConcurrentSetAndClearStayConsistent(t *testing.T) {
	t.Parallel()
	st := &countingStore{}
	c := NewCredentials(WithAuthBaseURL(newFakeGarmin(t).srv.URL), WithKeyStore(st))
	for round := range 20 {
		var wg sync.WaitGroup
		for i := range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if i%2 == 0 {
					_ = c.Set(context.Background(), validKey)
				} else {
					_ = c.Clear()
				}
			}()
		}
		wg.Wait()
		active := c.Status().Authenticated
		_, stored, _ := st.Load()
		if active != stored {
			t.Fatalf("round %d: active key = %v but stored key = %v", round, active, stored)
		}
	}
}
