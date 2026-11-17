package mcpserver

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// Errors returned by the user store.
var (
	ErrUserNotFound = errors.New("user not found")
	ErrUserExists   = errors.New("a user with that name already exists")
	ErrInvalidName  = errors.New("name must be 1 to 64 characters: letters, digits, spaces and . _ @ -, starting with a letter or digit")
	ErrNoKey        = errors.New("api_key is required")
)

const (
	// tokenPrefix marks the tokens this server issues, so a leaked one is easy
	// to recognise and to find with secret scanners.
	tokenPrefix      = "gmcp_"
	usersFileVersion = 1
)

var nameRE = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N} ._@-]{0,63}$`)

// User is a registered user as the rest of the program sees them. It never
// carries the Garmin key or the token.
type User struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// userRecord is what is written to disk. The token is stored only as a hash,
// so a copy of the file cannot be used to log in, and the Garmin key is
// encrypted, so it is useless without the master key.
type userRecord struct {
	User
	TokenHash string `json:"token_hash"` // hex SHA-256 of the token
	KeyEnc    string `json:"key_enc"`    // base64(nonce || AES-256-GCM ciphertext)
}

type usersFile struct {
	Version int          `json:"version"`
	Users   []userRecord `json:"users"`
}

// UserStore keeps the registered users in one JSON file. It is safe for
// concurrent use; every change is written to disk before it takes effect.
type UserStore struct {
	path string
	aead cipher.AEAD
	now  func() time.Time

	mu      sync.RWMutex
	byID    map[string]userRecord
	byToken map[string]string // token hash -> user id
}

// ParseMasterKey reads a 32-byte key written as 64 hex characters or as base64.
func ParseMasterKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("no master key given")
	}
	if b, err := hex.DecodeString(s); err == nil && len(b) == 32 {
		return b, nil
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil && len(b) == 32 {
			return b, nil
		}
	}
	return nil, errors.New("the master key must be 32 random bytes written as 64 hex characters or as base64 (generate one with: openssl rand -hex 32)")
}

// OpenUserStore opens the users file at path, creating an empty store when the
// file does not exist. It fails when the file was written with a different
// master key, rather than discovering that on a user's first request.
func OpenUserStore(path string, masterKey []byte) (*UserStore, error) {
	if len(masterKey) != 32 {
		return nil, errors.New("mcpserver: the master key must be 32 bytes")
	}
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	s := &UserStore{path: path, aead: aead, now: time.Now, byID: map[string]userRecord{}}

	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		s.reindex()
		return s, nil
	case err != nil:
		return nil, fmt.Errorf("reading users file: %w", err)
	}
	var f usersFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("users file %s is not valid: %w", path, err)
	}
	if f.Version != usersFileVersion {
		return nil, fmt.Errorf("users file %s has version %d; this build understands version %d", path, f.Version, usersFileVersion)
	}
	names := map[string]bool{}
	for _, r := range f.Users {
		if _, err := s.decrypt(r.ID, r.KeyEnc); err != nil {
			return nil, fmt.Errorf("users file %s: cannot decrypt the key of user %q; the master key does not match the one it was written with", path, r.Name)
		}
		lower := strings.ToLower(r.Name)
		if r.ID == "" || r.TokenHash == "" || names[lower] || s.byID[r.ID].ID != "" {
			return nil, fmt.Errorf("users file %s is inconsistent (duplicate or empty entries)", path)
		}
		names[lower] = true
		s.byID[r.ID] = r
	}
	s.reindex()
	return s, nil
}

func (s *UserStore) reindex() {
	s.byToken = make(map[string]string, len(s.byID))
	for id, r := range s.byID {
		s.byToken[r.TokenHash] = id
	}
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("mcpserver: no randomness available: " + err.Error()) // cannot continue safely
	}
	return hex.EncodeToString(b)
}

func newToken() string { return tokenPrefix + randomHex(32) }

// encrypt seals plaintext, binding it to the user id so a ciphertext cannot be
// moved to another user's record.
func (s *UserStore) encrypt(id, plaintext string) string {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic("mcpserver: no randomness available: " + err.Error())
	}
	return base64.StdEncoding.EncodeToString(s.aead.Seal(nonce, nonce, []byte(plaintext), []byte(id)))
}

func (s *UserStore) decrypt(id, enc string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil || len(raw) < s.aead.NonceSize() {
		return "", errors.New("malformed ciphertext")
	}
	plain, err := s.aead.Open(nil, raw[:s.aead.NonceSize()], raw[s.aead.NonceSize():], []byte(id))
	if err != nil {
		return "", errors.New("cannot decrypt")
	}
	return string(plain), nil
}

// CheckName reports whether name could be registered now.
func (s *UserStore) CheckName(name string) error {
	name = strings.TrimSpace(name)
	if !nameRE.MatchString(name) {
		return ErrInvalidName
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.nameFreeLocked(name)
}

func (s *UserStore) nameFreeLocked(name string) error {
	for _, r := range s.byID {
		if strings.EqualFold(r.Name, name) {
			return ErrUserExists
		}
	}
	return nil
}

// mutate applies change to a copy of the users, writes the copy to disk, and
// only then makes it current: a failed write leaves the store as it was.
func (s *UserStore) mutate(change func(users map[string]userRecord) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make(map[string]userRecord, len(s.byID)+1)
	for k, v := range s.byID {
		next[k] = v
	}
	if err := change(next); err != nil {
		return err
	}
	if err := s.persist(next); err != nil {
		return err
	}
	s.byID = next
	s.reindex()
	return nil
}

func (s *UserStore) persist(users map[string]userRecord) error {
	f := usersFile{Version: usersFileVersion, Users: make([]userRecord, 0, len(users))}
	for _, r := range users {
		f.Users = append(f.Users, r)
	}
	slices.SortFunc(f.Users, func(a, b userRecord) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.path, append(b, '\n'))
}

// Add registers a user with their Garmin key and returns the token they will
// authenticate with. The token is returned only here and cannot be recovered
// later, only replaced with RotateToken.
func (s *UserStore) Add(name, apiKey string) (User, string, error) {
	name = strings.TrimSpace(name)
	if !nameRE.MatchString(name) {
		return User{}, "", ErrInvalidName
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return User{}, "", ErrNoKey
	}
	id := "u_" + randomHex(8)
	token := newToken()
	rec := userRecord{
		User:      User{ID: id, Name: name, CreatedAt: s.now().UTC().Truncate(time.Second)},
		TokenHash: hashToken(token),
		KeyEnc:    s.encrypt(id, apiKey),
	}
	err := s.mutate(func(users map[string]userRecord) error {
		for _, r := range users {
			if strings.EqualFold(r.Name, name) {
				return ErrUserExists
			}
		}
		users[id] = rec
		return nil
	})
	if err != nil {
		return User{}, "", err
	}
	return rec.User, token, nil
}

// Lookup returns the user a token belongs to.
func (s *UserStore) Lookup(token string) (User, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.byToken[hashToken(token)]
	if !ok {
		return User{}, false
	}
	return s.byID[id].User, true
}

// APIKey returns a user's decrypted Garmin key.
func (s *UserStore) APIKey(id string) (string, error) {
	s.mu.RLock()
	r, ok := s.byID[id]
	s.mu.RUnlock()
	if !ok {
		return "", ErrUserNotFound
	}
	return s.decrypt(r.ID, r.KeyEnc)
}

// List returns every user, ordered by name.
func (s *UserStore) List() []User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]User, 0, len(s.byID))
	for _, r := range s.byID {
		out = append(out, r.User)
	}
	slices.SortFunc(out, func(a, b User) int { return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)) })
	return out
}

// Get returns one user.
func (s *UserStore) Get(id string) (User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.byID[id]
	if !ok {
		return User{}, ErrUserNotFound
	}
	return r.User, nil
}

// Remove deletes a user; their token stops working immediately.
func (s *UserStore) Remove(id string) error {
	return s.mutate(func(users map[string]userRecord) error {
		if _, ok := users[id]; !ok {
			return ErrUserNotFound
		}
		delete(users, id)
		return nil
	})
}

// RotateToken issues a new token for a user and invalidates the old one.
func (s *UserStore) RotateToken(id string) (string, error) {
	token := newToken()
	err := s.mutate(func(users map[string]userRecord) error {
		r, ok := users[id]
		if !ok {
			return ErrUserNotFound
		}
		r.TokenHash = hashToken(token)
		users[id] = r
		return nil
	})
	if err != nil {
		return "", err
	}
	return token, nil
}

// SetAPIKey replaces a user's Garmin key, for example when the old one expires.
func (s *UserStore) SetAPIKey(id, apiKey string) error {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return ErrNoKey
	}
	return s.mutate(func(users map[string]userRecord) error {
		r, ok := users[id]
		if !ok {
			return ErrUserNotFound
		}
		r.KeyEnc = s.encrypt(id, apiKey)
		users[id] = r
		return nil
	})
}
