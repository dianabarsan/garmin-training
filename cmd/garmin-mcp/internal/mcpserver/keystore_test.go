package mcpserver

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFileKeyStore_SaveLoadDelete(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "nested", "dir", "key")
	st := NewFileKeyStore(path)

	if key, ok, err := st.Load(); ok || err != nil || key != "" {
		t.Fatalf("missing file: key=%q ok=%v err=%v, want nothing stored", key, ok, err)
	}
	if err := st.Save("first"); err != nil {
		t.Fatal(err)
	}
	if err := st.Save("second"); err != nil { // replaces the first
		t.Fatal(err)
	}
	if key, ok, err := st.Load(); !ok || err != nil || key != "second" {
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

	if err := st.Delete(); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(); err != nil {
		t.Errorf("deleting a missing file: %v", err)
	}
	if _, ok, _ := st.Load(); ok {
		t.Error("key still loads after Delete")
	}
}

func TestFileKeyStore_BlankFileHoldsNoKey(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if key, ok, err := NewFileKeyStore(path).Load(); ok || err != nil || key != "" {
		t.Errorf("key=%q ok=%v err=%v, want nothing stored", key, ok, err)
	}
}

// A key file other users can read was not written by Save; Load makes it
// private.
func TestFileKeyStore_LoadMakesALooseFilePrivate(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte("k\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil { // the umask may have masked it
		t.Fatal(err)
	}
	if key, ok, err := NewFileKeyStore(path).Load(); !ok || err != nil || key != "k" {
		t.Fatalf("key=%q ok=%v err=%v", key, ok, err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestFileKeyStore_SaveFailsWhenTheDirectoryIsAFile(t *testing.T) {
	t.Parallel()
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := NewFileKeyStore(filepath.Join(file, "key")).Save("k"); err == nil {
		t.Error("Save under a regular file should fail")
	}
}
