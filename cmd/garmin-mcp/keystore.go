package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// keyFile keeps the Garmin API key in one file readable only by its owner
// (-key-store). Parent directories are created, mode 0700, on save.
type keyFile string

// load returns the stored key; ok is false when none is stored.
func (f keyFile) load() (key string, ok bool, err error) {
	b, err := os.ReadFile(string(f))
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading key file: %w", err)
	}
	key = strings.TrimSpace(string(b))
	return key, key != "", nil
}

// save stores key. The write is atomic and the file is created 0600.
func (f keyFile) save(key string) error {
	dir := filepath.Dir(string(f))
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
	// Flushed before the rename, so a crash cannot leave an empty key file in
	// place of the old one.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("writing key: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing key file: %w", err)
	}
	if err := os.Rename(tmp.Name(), string(f)); err != nil {
		return fmt.Errorf("replacing key file: %w", err)
	}
	return nil
}
