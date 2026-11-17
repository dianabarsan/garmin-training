package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// newLogger builds the text logger for the given level name.
func newLogger(level string, w io.Writer) (*slog.Logger, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("invalid -log-level %q", level)
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: lvl})), nil
}

// newAuditLogger returns the logger for the audit trail and a function that
// closes it. With a path, records are appended to that file as JSON lines (the
// file is mode 0600, its directory created 0700). When the file would grow past
// maxBytes it is renamed to path.1, replacing an older one, and a new file is
// started; maxBytes 0 means no limit. Without a path, records go to the normal
// log tagged audit=true.
func newAuditLogger(path string, maxBytes int64, ops *slog.Logger) (*slog.Logger, func(), error) {
	if path == "" {
		return ops.With("audit", true), func() {}, nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, nil, fmt.Errorf("-audit-log: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return nil, nil, fmt.Errorf("-audit-log: %w", err)
	}
	restrictFile(abs, "the audit log", ops)
	w, err := openRotating(abs, maxBytes)
	if err != nil {
		return nil, nil, fmt.Errorf("-audit-log: %w", err)
	}
	logger := slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return logger, func() { _ = w.Close() }, nil
}

// rotatingFile appends to a file and, when a write would take it past max
// bytes, moves it to path.1 and starts a new one. slog handlers write each
// record in one call, so records are never split across the two files.
type rotatingFile struct {
	path string
	max  int64

	mu   sync.Mutex
	f    *os.File
	size int64
}

func openRotating(path string, max int64) (*rotatingFile, error) {
	f, size, err := openAppend(path)
	if err != nil {
		return nil, err
	}
	return &rotatingFile{path: path, max: max, f: f, size: size}, nil
}

func openAppend(path string) (*os.File, int64, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, 0, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, info.Size(), nil
}

func (r *rotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.max > 0 && r.size > 0 && r.size+int64(len(p)) > r.max {
		// If rotating fails, keep appending to the current file: losing
		// audit records would be worse than exceeding the limit.
		_ = r.rotate()
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// rotate renames the current file to path.1 and opens a fresh one. The old
// descriptor stays in use until the new file is open.
func (r *rotatingFile) rotate() error {
	if err := os.Rename(r.path, r.path+".1"); err != nil {
		return err
	}
	f, size, err := openAppend(r.path)
	if err != nil {
		return err
	}
	_ = r.f.Close()
	r.f, r.size = f, size
	return nil
}

func (r *rotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.f.Close()
}

// restrictFile makes an existing file that other users can read or write
// private (mode 0600), and warns about it. A file the server creates itself is
// already 0600; this covers one that was created by hand. A missing file is
// fine: it is created with the right mode later.
func restrictFile(path, what string, log *slog.Logger) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 == 0 {
		return
	}
	mode := fmt.Sprintf("%#o", info.Mode().Perm())
	if err := os.Chmod(path, 0o600); err != nil {
		log.Warn(what+" is accessible to other users and could not be made private; run chmod 600 on it", "path", path, "mode", mode, "err", err)
		return
	}
	log.Warn(what+" was accessible to other users; changed its mode to 0600", "path", path, "was", mode)
}
