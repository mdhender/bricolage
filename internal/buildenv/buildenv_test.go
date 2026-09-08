// Copyright (c) 2026 Michael D Henderson.

//go:build !production

package buildenv

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestVerifyWithoutTag is the untagged half of PLAN.md M0 acceptance 14: an
// ordinary binary is fine with CMS_ENV unset and with development, and must
// refuse to run with production.
//
// The asymmetry is intentional. The ordinary binary rejects only the one value
// it must never see, because requiring developers to export anything in order
// to run "go run ./cmd/cmsd" is how you end up with a shell profile that
// exports it everywhere (DESIGN.md 14).
func TestVerifyWithoutTag(t *testing.T) {
	for _, tc := range []struct {
		name      string
		set       bool
		value     string
		wantPanic bool
	}{
		{name: "unset", set: false, wantPanic: false},
		{name: "empty", set: true, value: "", wantPanic: false},
		{name: "development", set: true, value: "development", wantPanic: false},
		{name: "staging", set: true, value: "staging", wantPanic: false},
		{name: "production", set: true, value: "production", wantPanic: true},
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

// TestNoInitFunction is PLAN.md M0 acceptance 15 and invariant 18. Verify is
// called explicitly from main, so that main keeps control of when it runs and
// can handle version or --help first. An init here would take that away
// silently.
func TestNoInitFunction(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		// Parse every file in the package, tagged or not: a build tag is
		// exactly how an init would hide from this test.
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if ok && fn.Recv == nil && fn.Name.Name == "init" {
				t.Errorf("%s declares func init; Verify is called from main, never from init (invariant 18)", name)
			}
		}
	}
}

// EnvVarForTest names the variable Verify reads. It is duplicated from
// internal/config on purpose: this package imports nothing from the repository
// and must not start now (see the package doc).
const EnvVarForTest = "CMS_ENV"

func assertPanic(t *testing.T, want bool) {
	t.Helper()
	got := func() (panicked bool) {
		defer func() { panicked = recover() != nil }()
		Verify()
		return
	}()
	if got != want {
		t.Fatalf("Verify() panicked = %v, want %v (CMS_ENV=%q, present=%v)",
			got, want, os.Getenv(EnvVarForTest), envPresent(EnvVarForTest))
	}
}

func unset(t *testing.T, key string) {
	t.Helper()
	// t.Setenv registers the restore; unset afterwards so the case really is
	// "not exported" rather than "exported as empty".
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("Unsetenv(%q): %v", key, err)
	}
}

func envPresent(key string) bool {
	_, ok := os.LookupEnv(key)
	return ok
}
