// Copyright (c) 2026 Michael D Henderson.

package workflow

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// PLAN.md M4 acceptance 4, enforced over the source tree: a grep for writes to
// documents.state finds them only where they belong.
//
// Two rules meet here and the tests below keep both. Invariant 2 says no SQL
// string appears outside internal/store, ever. Invariant 4 says
// internal/workflow is the only writer of documents.state, and DESIGN.md 6.3
// adds that the store must expose no method setting state alone. Neither is
// weakened: the statement lives in one file of internal/store, it cannot be
// reached without the engine's decision function, and the engine is its only
// caller. "make lint" runs the same two greps, so a change that breaks either
// fails before the tests do.

// stateUpdate matches an UPDATE of documents that assigns state.
//
// It is deliberately loose about whitespace and about what else the statement
// touches, because what is being caught is somebody writing a second one --
// and somebody writing a second one will not format it like the first.
var stateUpdate = regexp.MustCompile(`(?is)UPDATE\s+documents\b[^;` + "`" + `]*\bSET\b[^;` + "`" + `]*\bstate\s*=`)

// stateInsert matches an INSERT INTO documents naming the state column.
var stateInsert = regexp.MustCompile(`(?is)INSERT\s+INTO\s+documents\s*\([^)]*\bstate\b`)

// TestOnlyOneStatementWritesDocumentState is invariant 4 and invariant 2 at
// once.
func TestOnlyOneStatementWritesDocumentState(t *testing.T) {
	var updates, inserts []string
	for path, src := range goSources(t) {
		if stateUpdate.MatchString(src) {
			updates = append(updates, path)
		}
		if stateInsert.MatchString(src) {
			inserts = append(inserts, path)
		}
	}
	// Sorted, so that a failure names the offenders in a stable order rather
	// than in whatever order the map came out in.
	slices.Sort(updates)
	slices.Sort(inserts)

	// One UPDATE, in the file whose whole subject is the transition. It is
	// reachable only through ApplyTransition, which cannot run without the
	// decision function the engine supplies.
	if want := []string{"internal/store/workflow.go"}; !slices.Equal(updates, want) {
		t.Errorf("documents.state is updated in %v, want only %v\n"+
			"internal/workflow is the only writer of documents.state (invariant 4), and all SQL\n"+
			"lives in internal/store (invariant 2). The one statement satisfying both is the one\n"+
			"inside ApplyTransition, which the engine reaches by handing over its check.", updates, want)
	}

	// One INSERT, in the file that creates documents. Creating a document is
	// not a transition -- a document does not move into its initial state, it
	// starts there -- and the composite foreign key refuses any state its
	// workflow does not declare.
	if want := []string{"internal/store/documents.go"}; !slices.Equal(inserts, want) {
		t.Errorf("documents.state is inserted in %v, want only %v", inserts, want)
	}
}

// TestApplyTransitionHasOneCaller is the other half. The statement being in
// one place is worth nothing if anything may reach it.
func TestApplyTransitionHasOneCaller(t *testing.T) {
	var callers []string
	for path, src := range goSources(t) {
		if strings.HasPrefix(path, "internal/store/") {
			// The store declares it; declaring is not calling.
			continue
		}
		if strings.Contains(src, "ApplyTransition") {
			callers = append(callers, path)
		}
	}
	slices.Sort(callers)

	if want := []string{"internal/workflow/engine.go"}; !slices.Equal(callers, want) {
		t.Errorf("ApplyTransition is called from %v, want only %v\n"+
			"If something needs to move a document, it calls the engine (invariant 4, DESIGN.md 6.3).", callers, want)
	}
}

// goSources returns every non-test Go file under cmd/ and internal/, keyed by
// its repository-relative path with forward slashes, in sorted order.
func goSources(t *testing.T) map[string]string {
	t.Helper()

	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			out[filepath.ToSlash(rel)] = string(b)
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", dir, err)
		}
	}
	if len(out) == 0 {
		t.Fatal("no Go sources were found; this test would pass vacuously")
	}
	return out
}
