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
func (f *FileKeyStore) Save(key string) error { return writeFileAtomic(f.path, []byte(key+"\n")) }

// writeFileAtomic writes data to path so that a crash cannot leave a half-written
// file: it writes a private (0600) temporary file in the same directory and
// renames it into place. Missing parent directories are created 0700.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*") // created 0600
	if err != nil {
		return fmt.Errorf("creating temporary file: %w", err)
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("replacing file: %w", err)
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
