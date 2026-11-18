package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestKeyFile_SaveAndLoad(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "nested", "dir", "key")
	st := keyFile(path)

	if key, ok, err := st.load(); ok || err != nil || key != "" {
		t.Fatalf("missing file: key=%q ok=%v err=%v, want nothing stored", key, ok, err)
	}
	if err := st.save("first"); err != nil {
		t.Fatal(err)
	}
	if err := st.save("second"); err != nil { // replaces the first
		t.Fatal(err)
	}
	if key, ok, err := st.load(); !ok || err != nil || key != "second" {
		t.Fatalf("after Save: key=%q ok=%v err=%v", key, ok, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("key file mode = %v (%v), want 0600", info.Mode().Perm(), err)
	}
	if dir, _ := os.Stat(filepath.Dir(path)); dir.Mode().Perm() != 0o700 {
		t.Errorf("created directory mode = %v, want 0700", dir.Mode().Perm())
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("directory holds %d entries, want only the key file (no temp files left)", len(entries))
	}
}

func TestKeyFile_BlankFileHoldsNoKey(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if key, ok, err := keyFile(path).load(); ok || err != nil || key != "" {
		t.Errorf("key=%q ok=%v err=%v, want nothing stored", key, ok, err)
	}
}

func TestKeyFile_SaveFailsWhenTheDirectoryIsAFile(t *testing.T) {
	t.Parallel()
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := keyFile(filepath.Join(file, "key")).save("k"); err == nil {
		t.Error("Save under a regular file should fail")
	}
}
