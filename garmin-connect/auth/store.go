package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// Store persists a Token between process runs. Implementations must be safe
// for concurrent use.
type Store interface {
	// Load returns the stored token. ok is false when nothing is stored.
	Load(ctx context.Context) (t Token, ok bool, err error)
	Save(ctx context.Context, t Token) error
}

// FileStore keeps one token in a JSON file readable only by the owner.
type FileStore struct {
	path string
}

// NewFileStore returns a FileStore writing to path. Parent directories are
// created on Save.
func NewFileStore(path string) *FileStore { return &FileStore{path: path} }

// Load implements Store.
func (f *FileStore) Load(context.Context) (Token, bool, error) {
	b, err := os.ReadFile(f.path)
	if errors.Is(err, fs.ErrNotExist) {
		return Token{}, false, nil
	}
	if err != nil {
		return Token{}, false, fmt.Errorf("auth: reading token file: %w", err)
	}
	// The file holds a live access token. One that other users can read was
	// not written by Save (which creates it 0600); make it private.
	if info, err := os.Stat(f.path); err == nil && info.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(f.path, 0o600); err != nil {
			return Token{}, false, fmt.Errorf("auth: token file %s is readable by other users and could not be made private: %w", f.path, err)
		}
	}
	var t Token
	if err := json.Unmarshal(b, &t); err != nil {
		return Token{}, false, fmt.Errorf("auth: decoding token file: %w", err)
	}
	return t, true, nil
}

// Save implements Store. The write is atomic: a crash cannot leave a
// half-written file behind. The server's FileKeyStore.Save (in the root
// module) does the same; change both together.
func (f *FileStore) Save(_ context.Context, t Token) error {
	b, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("auth: encoding token: %w", err)
	}
	dir := filepath.Dir(f.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("auth: creating token dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".token-*") // created with mode 0600
	if err != nil {
		return fmt.Errorf("auth: creating temp token file: %w", err)
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("auth: writing token: %w", err)
	}
	// Flushed before the rename, so a crash cannot leave an empty file in
	// place of a good one.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("auth: writing token: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("auth: closing token file: %w", err)
	}
	if err := os.Rename(tmp.Name(), f.path); err != nil {
		return fmt.Errorf("auth: replacing token file: %w", err)
	}
	return nil
}

// MemoryStore is an in-process Store, mainly useful in tests. The zero value
// is ready to use.
type MemoryStore struct {
	mu sync.Mutex
	t  Token
	ok bool
}

// Load implements Store.
func (m *MemoryStore) Load(context.Context) (Token, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.t, m.ok, nil
}

// Save implements Store.
func (m *MemoryStore) Save(_ context.Context, t Token) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.t, m.ok = t, true
	return nil
}
