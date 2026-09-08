// Copyright (c) 2026 Michael D Henderson.

package config

import (
	"io"
	"log/slog"
)

// NewLogger builds the logger for an environment.
//
// The format is one of the things the environment governs (DESIGN.md 14):
// console and human-readable on a developer's machine, JSON everywhere else,
// because everywhere else something is collecting it.
//
// It lives in this package rather than in a logging package of its own because
// there is nothing to decide here beyond the environment, and the environment
// is what this package resolves.
func NewLogger(env Environment, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	if env.IsDevelopment() {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}
