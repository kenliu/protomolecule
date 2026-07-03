package main

import (
	"io"
	"log/slog"
)

// NewJSONLogger creates a structured JSON logger writing to w.
// If verbose is true, the log level is set to Debug.
func NewJSONLogger(w io.Writer, verbose bool) *slog.Logger {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: level,
	}))
}

// DiscardLogger returns a logger that discards all output (for tests).
func DiscardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}
