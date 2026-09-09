// Copyright (c) 2026 Michael D Henderson.

package domain

import (
	"errors"
	"math/rand/v2"
	"strings"
	"testing"
)

// The category path tests (PLAN.md M7 acceptance 2 and 4).
//
// The invariant is one sentence -- a path begins and ends with '/', and the
// root is '/' -- and everything downstream is a string operation on it, so a
// path that broke it would not fail loudly. It would build a URI with a
// doubled slash, or match a grant against a sibling whose name happens to
// start the same way. These are the tests that make the sentence true.

func TestValidatePath(t *testing.T) {
	for _, tc := range []struct {
		path string
		ok   bool
	}{
		{"/", true},
		{"/features/", true},
		{"/features/film/", true},
		{"/a/b/c/d/e/", true},
		{"", false},
		{"features/", false},
		{"/features", false},
		{"features", false},
		{"//features/", false},
		{"/features//film/", false},
	} {
		err := ValidatePath(tc.path)
		if tc.ok && err != nil {
			t.Errorf("ValidatePath(%q) = %v, want nil", tc.path, err)
		}
		if !tc.ok {
			if err == nil {
				t.Errorf("ValidatePath(%q) = nil, want a refusal", tc.path)
			} else if !errors.Is(err, ErrInvalid) {
				t.Errorf("ValidatePath(%q) = %v, want it to wrap ErrInvalid", tc.path, err)
			}
		}
	}
}

func TestValidateDirectory(t *testing.T) {
	for _, tc := range []struct {
		dir string
		ok  bool
	}{
		{"features", true},
		{"film-and-tv", true},
		{"2026", true},
		{"a_b.c~d", true},
		{"", false},
		{"with/slash", false},
		{"with space", false},
		{"..", false},
		{".", false},
		{"café", false},
		{"per%cent", false},
		{strings.Repeat("x", MaxDirectoryLen+1), false},
	} {
		err := ValidateDirectory(tc.dir)
		if tc.ok != (err == nil) {
			t.Errorf("ValidateDirectory(%q) = %v, want ok=%t", tc.dir, err, tc.ok)
		}
	}
}

// TestJoinPathKeepsTheInvariant is PLAN.md M7 acceptance 2 as a property:
// whatever tree you build with JoinPath, every path in it begins and ends with
// '/' and contains no empty segment.
//
// It is a property test rather than a table because the invariant is about
// every path a tree can have, and a table can only be about the ones somebody
// thought of. The generator is seeded from a fixed value so a failure is
// reproducible.
func TestJoinPathKeepsTheInvariant(t *testing.T) {
	segments := []string{"a", "features", "film", "2026", "x-y", "z_1", "long-directory-name"}
	rng := rand.New(rand.NewPCG(1, 2))

	for i := 0; i < 500; i++ {
		path := RootPath
		depth := rng.IntN(8)
		for d := 0; d < depth; d++ {
			next, err := JoinPath(path, segments[rng.IntN(len(segments))])
			if err != nil {
				t.Fatalf("JoinPath(%q, ...): %v", path, err)
			}
			path = next
		}
		if err := ValidatePath(path); err != nil {
			t.Fatalf("a path built only from JoinPath is invalid: %q: %v", path, err)
		}
		if got := PathDepth(path); got != depth {
			t.Fatalf("PathDepth(%q) = %d, want %d", path, got, depth)
		}
	}
}

// TestRootIsSlash is the other half of acceptance 2: the root is exactly "/",
// and it is the only path with depth 0.
func TestRootIsSlash(t *testing.T) {
	if RootPath != "/" {
		t.Fatalf("RootPath = %q, want %q", RootPath, "/")
	}
	if err := ValidatePath(RootPath); err != nil {
		t.Errorf("the root does not satisfy the path rule: %v", err)
	}
	if got := PathDepth(RootPath); got != 0 {
		t.Errorf("PathDepth(%q) = %d, want 0", RootPath, got)
	}
	root := Category{Path: RootPath}
	if !root.IsRoot() {
		t.Error("a category with no parent does not report itself as a root")
	}
}

func TestIsDescendantPath(t *testing.T) {
	for _, tc := range []struct {
		parent, child string
		want          bool
	}{
		{"/", "/", true},
		{"/", "/features/", true},
		{"/features/", "/features/", true},
		{"/features/", "/features/film/", true},
		{"/features/", "/features-and-analysis/", false},
		{"/features/film/", "/features/", false},
		{"/feat/", "/features/film/", false},
	} {
		if got := IsDescendantPath(tc.parent, tc.child); got != tc.want {
			t.Errorf("IsDescendantPath(%q, %q) = %t, want %t", tc.parent, tc.child, got, tc.want)
		}
	}
}

// TestRewritePath is the Go statement of what the store's single-statement
// subtree rewrite does. store.TestMoveCategory checks the SQL against it.
func TestRewritePath(t *testing.T) {
	for _, tc := range []struct {
		path, old, new, want string
	}{
		{"/features/film/", "/features/film/", "/culture/film/", "/culture/film/"},
		{"/features/film/reviews/", "/features/film/", "/culture/film/", "/culture/film/reviews/"},
		{"/features/books/", "/features/film/", "/culture/film/", "/features/books/"},
		{"/", "/features/", "/culture/", "/"},
	} {
		if got := RewritePath(tc.path, tc.old, tc.new); got != tc.want {
			t.Errorf("RewritePath(%q, %q, %q) = %q, want %q", tc.path, tc.old, tc.new, got, tc.want)
		}
	}
}

// TestMoveRefusals covers the two moves that cannot be made: a site's root has
// nowhere to go, and nothing may be moved inside itself.
func TestMoveRefusals(t *testing.T) {
	root := Category{ID: 1, SiteID: 1, Path: "/", Name: "Site"}
	features := Category{ID: 2, SiteID: 1, ParentID: 1, Directory: "features", Path: "/features/"}
	film := Category{ID: 3, SiteID: 1, ParentID: 2, Directory: "film", Path: "/features/film/"}
	otherSite := Category{ID: 4, SiteID: 2, Path: "/"}

	if _, err := (CategoryMove{ParentPath: Ref("/features/")}).Resolve(root, features); !errors.Is(err, ErrConflict) {
		t.Errorf("moving a root = %v, want a conflict", err)
	}
	if _, err := (CategoryMove{ParentPath: Ref("/features/film/")}).Resolve(features, film); !errors.Is(err, ErrConflict) {
		t.Errorf("moving a category into its own subtree = %v, want a conflict", err)
	}
	if _, err := (CategoryMove{}).Resolve(features, root); !errors.Is(err, ErrInvalid) {
		t.Errorf("a move that changes nothing = %v, want invalid", err)
	}
	if _, err := (CategoryMove{ParentPath: Ref("/")}).Resolve(features, otherSite); !errors.Is(err, ErrInvalid) {
		t.Errorf("moving between sites = %v, want invalid", err)
	}

	got, err := (CategoryMove{ParentPath: Ref("/")}).Resolve(film, root)
	if err != nil {
		t.Fatalf("moving /features/film/ to the root: %v", err)
	}
	if got != "/film/" {
		t.Errorf("the moved path = %q, want %q", got, "/film/")
	}

	renamed, err := (CategoryMove{Directory: Ref("cinema")}).Resolve(film, features)
	if err != nil {
		t.Fatalf("renaming: %v", err)
	}
	if renamed != "/features/cinema/" {
		t.Errorf("the renamed path = %q, want %q", renamed, "/features/cinema/")
	}
}

func TestPrimaryOf(t *testing.T) {
	filings := []Filing{
		{Category: Category{Path: "/features/"}, Primary: false},
		{Category: Category{Path: "/features/film/"}, Primary: true},
	}
	got, ok := PrimaryOf(filings)
	if !ok || got.Path != "/features/film/" {
		t.Errorf("PrimaryOf = %q, %t; want /features/film/, true", got.Path, ok)
	}
	if _, ok := PrimaryOf(nil); ok {
		t.Error("a document filed nowhere reports a primary category")
	}
}
