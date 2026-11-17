// SPDX-License-Identifier: AGPL-3.0-only

// Package logging builds the structured logger.
//
// PlainRoute logs routing facts, never content: no API key, no Authorization
// header, no prompt, no response body. There is no field in this package that
// could carry a secret, so a log line cannot leak a key by accident.
package logging

import (
	"io"
	"log"
	"log/slog"
	"strings"

	"github.com/40960/plainroute/internal/config"
)

// New returns a logger for the given config.
func New(cfg config.LoggingConfig, w io.Writer) *slog.Logger {
	level := slog.LevelInfo
	switch cfg.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	opts := &slog.HandlerOptions{Level: level}
	if cfg.Format == "text" {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

// StdLogger adapts the standard library logger that http.Server expects, so the
// server's own error lines go through the same structured output.
func StdLogger(l *slog.Logger) *log.Logger {
	return log.New(&slogWriter{l}, "", 0)
}

type slogWriter struct{ l *slog.Logger }

func (w *slogWriter) Write(p []byte) (int, error) {
	w.l.Warn("http_server", "message", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
