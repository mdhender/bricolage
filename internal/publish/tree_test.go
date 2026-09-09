// Copyright (c) 2026 Michael D Henderson.

package publish

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mdhender/bricolage/internal/domain"
)

// The output tree's own tests. Two things are being asserted and both are
// about the exception this package makes to invariant 19: the root is never
// created, and nothing that is not a path within the tree is ever written.

func newTree(t *testing.T) (*Tree, string) {
	t.Helper()
	// t.TempDir already exists, which is the only reason this helper needs no
	// mkdir: a helper that created what NewTree refuses to create would be a
	// hole in invariant 19 wide enough for the production code.
	dir := t.TempDir()
	tree, err := NewTree(dir)
	if err != nil {
		t.Fatalf("NewTree: %v", err)
	}
	t.Cleanup(func() { _ = tree.Close() })
	return tree, dir
}

// TestNewTreeNeverCreatesItsRoot is the half of invariant 19 that does not
// bend. A mistyped --output is a refusal at startup, not a site published into
// a directory nobody can find.
func TestNewTreeNeverCreatesItsRoot(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-there")
	if _, err := NewTree(missing); err == nil {
		t.Fatal("NewTree created its root")
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Stat(%q) = %v, want it still not to exist", missing, err)
	}

	// A file is not a directory, and saying so beats a write that fails later
	// with a message about a path.
	file := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := NewTree(file); err == nil {
		t.Error("NewTree accepted a file as an output tree")
	}

	if _, err := NewTree("  "); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("NewTree(\"\") = %v, want unavailable", err)
	}
}

// TestBatchCreatesTheInteriorAndFindsItAgain is the exception working: the
// directories below the root are computed, so they are made.
func TestBatchCreatesTheInteriorAndFindsItAgain(t *testing.T) {
	tree, dir := newTree(t)

	b := tree.Begin()
	written, err := b.Put("features/film/2026/03/01/a-piece/index.html", []byte("<p>page</p>"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if written.Bytes != 11 {
		t.Errorf("bytes = %d, want 11", written.Bytes)
	}
	if len(written.Checksum) != domain.ChecksumLen {
		t.Errorf("checksum = %q, want a hex sha256", written.Checksum)
	}

	got, err := tree.Files()
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	if len(got) != 1 || got[0] != "features/film/2026/03/01/a-piece/index.html" {
		t.Fatalf("Files = %v, want the one page", got)
	}

	// No temporary file survives a successful write, and none is reported by
	// the walk: a stray ".publish-" left behind would be reported as an
	// orphan by "cmsdb check" forever.
	for _, p := range got {
		if strings.Contains(p, ".publish-") {
			t.Errorf("the walk reports a temporary file: %s", p)
		}
	}

	body, err := tree.Read("features/film/2026/03/01/a-piece/index.html")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(body) != "<p>page</p>" {
		t.Errorf("read %q", body)
	}

	// The directories are real on disk, which is what a web server needs.
	if info, err := os.Stat(filepath.Join(dir, "features", "film")); err != nil || !info.IsDir() {
		t.Errorf("Stat(features/film) = %v, %v; want a directory", info, err)
	}
}

// TestRollbackPutsTheTreeBack is PLAN.md M9 acceptance 6 at the layer that
// holds a file descriptor: a failed publish leaves no partial output.
func TestRollbackPutsTheTreeBack(t *testing.T) {
	tree, _ := newTree(t)

	// One page already published.
	first := tree.Begin()
	if _, err := first.Put("a/index.html", []byte("original")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// A republish that replaces it and adds another, then fails.
	second := tree.Begin()
	if _, err := second.Put("a/index.html", []byte("replacement")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := second.Put("b/index.html", []byte("new")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := second.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	files, err := tree.Files()
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	if len(files) != 1 || files[0] != "a/index.html" {
		t.Fatalf("Files = %v after the rollback, want only the page that was there before", files)
	}
	body, err := tree.Read("a/index.html")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(body) != "original" {
		t.Errorf("the replaced page reads %q, want the bytes that were there before the failed publish", body)
	}
}

// TestPutRefusesAPathThatWouldLeaveTheTree is the second of the two answers.
// domain.OutputPath is the first; this is the one that runs even if the first
// were wrong.
func TestPutRefusesAPathThatWouldLeaveTheTree(t *testing.T) {
	tree, dir := newTree(t)

	for _, p := range []string{
		"../escaped.html",
		"a/../../escaped.html",
		"/absolute.html",
		"a//b.html",
		"a/./b.html",
		".publish-sneaky",
		"a/.publish-sneaky",
		"",
	} {
		t.Run(p, func(t *testing.T) {
			b := tree.Begin()
			if _, err := b.Put(p, []byte("x")); err == nil {
				t.Fatalf("Put(%q) was accepted", p)
			}
		})
	}

	// Nothing escaped, and nothing was created on the way to finding out.
	entries, err := os.ReadDir(filepath.Dir(dir))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if e.Name() == "escaped.html" || e.Name() == "absolute.html" {
			t.Errorf("a write escaped the tree: %s", e.Name())
		}
	}
	files, err := tree.Files()
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("the tree holds %v after only refusals", files)
	}
}

// TestPutRefusesTheSamePathTwiceInOneBatch. Two channels resolving to one file
// is a configuration mistake, and writing one over the other inside a single
// publish would leave the resource rows describing bytes that are not there.
func TestPutRefusesTheSamePathTwiceInOneBatch(t *testing.T) {
	tree, _ := newTree(t)
	b := tree.Begin()
	if _, err := b.Put("a/index.html", []byte("one")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := b.Put("a/index.html", []byte("two")); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("the second Put = %v, want a conflict", err)
	}
}

// TestRemoveIsIdempotent. An expire job may be retried after the delete
// succeeded and the process died before it could say so.
func TestRemoveIsIdempotent(t *testing.T) {
	tree, _ := newTree(t)
	b := tree.Begin()
	if _, err := b.Put("a/index.html", []byte("page")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	for range 2 {
		if err := tree.Remove("a/index.html"); err != nil {
			t.Fatalf("Remove: %v", err)
		}
	}
	files, err := tree.Files()
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("Files = %v after the removal", files)
	}

	// The directory is left behind, and deliberately: an empty directory is
	// not damage, and pruning it would race a publish writing into it.
	if _, err := os.Stat(filepath.Join(tree.Dir(), "a")); err != nil {
		t.Errorf("the directory was pruned: %v", err)
	}
}

// TestCompareIsTheReconciliation is the pure half of PLAN.md M9 acceptance 7.
func TestCompareIsTheReconciliation(t *testing.T) {
	resources := []domain.Resource{
		{Path: "a/index.html"},
		{Path: "b/index.html"},
	}
	got := Compare(resources, []string{"b/index.html", "stray.html"})

	if !slices.Equal(got.Missing, []string{"a/index.html"}) {
		t.Errorf("missing = %v, want the row whose file is gone", got.Missing)
	}
	if !slices.Equal(got.Unknown, []string{"stray.html"}) {
		t.Errorf("unknown = %v, want the file no row claims", got.Unknown)
	}
	if got.Empty() || got.Count() != 2 {
		t.Errorf("count = %d, empty = %v", got.Count(), got.Empty())
	}

	if agreed := Compare(resources, []string{"a/index.html", "b/index.html"}); !agreed.Empty() {
		t.Errorf("a tree that agrees reports %+v", agreed)
	}
	if empty := Compare(nil, nil); !empty.Empty() {
		t.Errorf("two empty sets disagree: %+v", empty)
	}
}
