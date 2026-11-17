package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuditLog_RotatesPastTheSizeLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	audit, closeAudit, err := newAuditLogger(path, 1000, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	for i := range 40 {
		audit.Info("tool_call", "n", i, "pad", strings.Repeat("x", 50))
	}
	closeAudit()

	cur, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	old, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("no rotated file: %v", err)
	}
	if len(cur) > 1000 || len(old) > 1000 {
		t.Errorf("files exceed the limit: current %d bytes, previous %d bytes", len(cur), len(old))
	}
	// Every record is whole: rotation never splits one across files.
	for name, b := range map[string][]byte{"current": cur, "previous": old} {
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if !strings.HasPrefix(line, "{") || !strings.HasSuffix(line, "}") {
				t.Errorf("%s file has a broken record: %q", name, line)
			}
		}
	}
	if !strings.Contains(string(cur), `"n":39`) {
		t.Error("the newest record should be in the current file")
	}
	for _, p := range []string{path, path + ".1"} {
		if info, _ := os.Stat(p); info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, want 0600", p, info.Mode().Perm())
		}
	}
}

func TestAuditLog_NoLimitMeansNoRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	audit, closeAudit, err := newAuditLogger(path, 0, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	for range 50 {
		audit.Info("tool_call", "pad", strings.Repeat("x", 100))
	}
	closeAudit()
	if _, err := os.Stat(path + ".1"); err == nil {
		t.Error("the file was rotated although the limit is 0")
	}
}

func TestRestrictFile_MakesAnExistingFilePrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "garmin.key")
	if err := os.WriteFile(path, []byte("k\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil { // in case of a restrictive umask
		t.Fatal(err)
	}
	var logs strings.Builder
	restrictFile(path, "the key store", slog.New(slog.NewTextHandler(&logs, nil)))
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
	if !strings.Contains(logs.String(), "changed its mode to 0600") {
		t.Errorf("the change should be reported:\n%s", logs.String())
	}

	// A private or missing file is left alone, silently.
	logs.Reset()
	restrictFile(path, "the key store", slog.New(slog.NewTextHandler(&logs, nil)))
	restrictFile(filepath.Join(t.TempDir(), "absent"), "the key store", slog.New(slog.NewTextHandler(&logs, nil)))
	if logs.Len() != 0 {
		t.Errorf("nothing should be logged:\n%s", logs.String())
	}
}

func TestRun_TightensALooseKeyStoreFile(t *testing.T) {
	garminSrv, _ := fakeGarmin(t)
	store := filepath.Join(t.TempDir(), "garmin.key")
	if err := os.WriteFile(store, []byte(goodKey+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(store, 0o644)
	_, logs, _ := start(t, nil, "-garmin-url", garminSrv.URL, "-key-store", store)
	if info, _ := os.Stat(store); info.Mode().Perm() != 0o600 {
		t.Errorf("key store mode = %v, want 0600\n%s", info.Mode().Perm(), logs.String())
	}
}
