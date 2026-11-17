package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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
// file is created mode 0600, its directory 0700). Without one they go to the
// normal log tagged audit=true.
func newAuditLogger(path string, ops *slog.Logger) (*slog.Logger, func(), error) {
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
	f, err := os.OpenFile(abs, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("-audit-log: %w", err)
	}
	logger := slog.New(slog.NewJSONHandler(f, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return logger, func() { _ = f.Close() }, nil
}
