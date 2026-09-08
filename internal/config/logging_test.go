// Copyright (c) 2026 Michael D Henderson.

package config

import (
	"bytes"
	"strings"
	"testing"
)

// TestNewLoggerFormatPerEnvironment covers the row of the DESIGN.md 14 table
// that says console in development, JSON everywhere else.
func TestNewLoggerFormatPerEnvironment(t *testing.T) {
	var dev, prod bytes.Buffer
	NewLogger(Development, &dev).Info("hello", "environment", "development")
	NewLogger(Production, &prod).Info("hello", "environment", "production")

	if got := dev.String(); !strings.Contains(got, "environment=development") {
		t.Errorf("development log = %q, want key=value text", got)
	}
	if got := prod.String(); !strings.Contains(got, `"environment":"production"`) {
		t.Errorf("production log = %q, want JSON", got)
	}
	if got := prod.String(); strings.Contains(got, "environment=production") {
		t.Errorf("production log = %q, unexpectedly in text format", got)
	}
}
