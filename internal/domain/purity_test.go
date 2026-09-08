// Copyright (c) 2026 Michael D Henderson.

package domain

import (
	"go/build"
	"strings"
	"testing"
)

// modulePath is this repository's module path. A domain import that starts
// with it is an import of this repository.
const modulePath = "github.com/mdhender/bricolage"

// forbidden are the standard-library packages that would make domain impure
// even though they are not repository imports.
//
// database/sql and net/http are I/O, and domain performs none (invariant 1).
// "time" is deliberately absent: domain may name a time.Time, it may not read
// the clock, and that is a rule about time.Now rather than about the import
// (invariant 3, enforced by the repository-wide grep in the Makefile).
var forbidden = []string{
	"database/sql",
	"net/http",
	"os",
}

// TestDomainImportsNothingLocal is PLAN.md M0 acceptance 3 and invariant 1.
// domain is the bottom of the dependency graph: it holds types, constants, and
// pure functions that decide things, and it imports nothing from this
// repository. If this test ever fails, the fix is almost always to move the
// decision into domain as a pure function that both layers call, not to add
// the import.
func TestDomainImportsNothingLocal(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatalf("ImportDir: %v", err)
	}

	for _, imp := range pkg.Imports {
		if strings.HasPrefix(imp, modulePath) {
			t.Errorf("domain imports %q; it must import nothing from this repository", imp)
		}
		for _, bad := range forbidden {
			if imp == bad || strings.HasPrefix(imp, bad+"/") {
				t.Errorf("domain imports %q; domain performs no I/O", imp)
			}
		}
	}
}
