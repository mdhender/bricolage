// Copyright (c) 2026 Michael D Henderson.

//go:build production

package buildenv

import (
	"os"
	"testing"
)

// TestVerifyWithTag is the tagged half of PLAN.md M0 acceptance 14, run in CI
// with "go test -tags production ./...".
//
// A release binary requires CMS_ENV to be set explicitly, because a server
// should say what it is (DESIGN.md 14).
func TestVerifyWithTag(t *testing.T) {
	for _, tc := range []struct {
		name      string
		set       bool
		value     string
		wantPanic bool
	}{
		{name: "unset", set: false, wantPanic: true},
		{name: "empty", set: true, value: "", wantPanic: true},
		{name: "development", set: true, value: "development", wantPanic: true},
		{name: "staging", set: true, value: "staging", wantPanic: true},
		{name: "production", set: true, value: "production", wantPanic: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv(EnvVarForTest, tc.value)
			} else {
				unset(t, EnvVarForTest)
			}
			assertPanic(t, tc.wantPanic)
		})
	}
}

// EnvVarForTest names the variable Verify reads.
const EnvVarForTest = "CMS_ENV"

func assertPanic(t *testing.T, want bool) {
	t.Helper()
	got := func() (panicked bool) {
		defer func() { panicked = recover() != nil }()
		Verify()
		return
	}()
	if got != want {
		t.Fatalf("Verify() panicked = %v, want %v (CMS_ENV=%q)", got, want, os.Getenv(EnvVarForTest))
	}
}

func unset(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("Unsetenv(%q): %v", key, err)
	}
}
