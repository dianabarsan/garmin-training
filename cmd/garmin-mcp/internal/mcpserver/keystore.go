package mcpserver

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// FileKeyStore keeps the API key in one file readable only by its owner.
type FileKeyStore struct{ path string }

// NewFileKeyStore returns a store backed by path. Parent directories are
// created (mode 0700) on Save.
func NewFileKeyStore(path string) *FileKeyStore { return &FileKeyStore{path: path} }

// Load implements KeyStore.
func (f *FileKeyStore) Load() (string, bool, error) {
	b, err := os.ReadFile(f.path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading key file: %w", err)
	}
	key := strings.TrimSpace(string(b))
	return key, key != "", nil
}

// Save implements KeyStore. The write is atomic and the file is created 0600.
func (f *FileKeyStore) Save(key string) error {
	dir := filepath.Dir(f.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating key dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".key-*") // 0600
	if err != nil {
		return fmt.Errorf("creating temp key file: %w", err)
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.WriteString(key + "\n"); err != nil {
		tmp.Close()
		return fmt.Errorf("writing key: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing key file: %w", err)
	}
	if err := os.Rename(tmp.Name(), f.path); err != nil {
		return fmt.Errorf("replacing key file: %w", err)
	}
	return nil
}

// Delete implements KeyStore. A missing file is not an error.
func (f *FileKeyStore) Delete() error {
	if err := os.Remove(f.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("deleting key file: %w", err)
	}
	return nil
}
