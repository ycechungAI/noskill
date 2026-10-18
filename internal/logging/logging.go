// Package logging sets up slog: text on a terminal, JSON lines in the daemon's log file.
package logging

import (
	"io"
	"log/slog"
	"strings"
)

// Level parses [general] log_level (debug | info | warn | error); unknown values are info, which
// `config check` already reports.
func Level(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}

// New returns a logger writing to w. jsonLines is for log files that tools will parse.
func New(w io.Writer, level string, jsonLines bool) *slog.Logger {
	opts := &slog.HandlerOptions{Level: Level(level)}
	if jsonLines {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}
