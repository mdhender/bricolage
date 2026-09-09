// Copyright (c) 2026 Michael D Henderson.

package domain

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestRenderModes pins the closed vocabulary. A mode this system does not
// implement must be refused rather than defaulted, for the reason an
// unimplemented strftime conversion is: a value nobody validates is a value
// somebody misspells.
func TestRenderModes(t *testing.T) {
	for _, m := range RenderModes {
		if !ValidRenderMode(m) {
			t.Errorf("ValidRenderMode(%q) = false, want true", m)
		}
	}
	for _, m := range []string{"", "Publish", "prevue", "render", "validate "} {
		if ValidRenderMode(m) {
			t.Errorf("ValidRenderMode(%q) = true, want false", m)
		}
	}
	if got, want := len(RenderModes), 3; got != want {
		t.Errorf("%d modes, want %d: the design names publish, preview, and validate", got, want)
	}
}

// TestAncestorPaths is the template cascade, written as arithmetic
// (PLAN.md M8 acceptance 1). Deepest first, ending at the root.
func TestAncestorPaths(t *testing.T) {
	for _, tc := range []struct {
		path string
		want []string
	}{
		{"/", []string{"/"}},
		{"/features/", []string{"/features/", "/"}},
		{"/features/film/", []string{"/features/film/", "/features/", "/"}},
		{"/a/b/c/d/", []string{"/a/b/c/d/", "/a/b/c/", "/a/b/", "/a/", "/"}},

		// Not a materialised path, so no answer. A plausible-looking list
		// would send a lookup to directories no category could produce.
		{"", nil},
		{"features/film/", nil},
		{"/features/film", nil},
		{"/features//film/", nil},
	} {
		t.Run(tc.path, func(t *testing.T) {
			got := AncestorPaths(tc.path)
			if len(got) != len(tc.want) {
				t.Fatalf("AncestorPaths(%q) = %v, want %v", tc.path, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("AncestorPaths(%q) = %v, want %v", tc.path, got, tc.want)
				}
			}
		})
	}
}

// TestAncestorPathsAreValidPaths is the property behind the table: every
// element of the walk is itself a materialised path, so the walk can be fed
// back into anything that takes one.
func TestAncestorPathsAreValidPaths(t *testing.T) {
	for _, path := range []string{"/", "/a/", "/a/b/", "/one/two/three/four/five/"} {
		got := AncestorPaths(path)
		if len(got) != PathDepth(path)+1 {
			t.Errorf("AncestorPaths(%q) has %d entries, want depth+1 = %d", path, len(got), PathDepth(path)+1)
		}
		for _, p := range got {
			if err := ValidatePath(p); err != nil {
				t.Errorf("AncestorPaths(%q) produced %q, which is not a path: %v", path, p, err)
			}
		}
		if got[len(got)-1] != RootPath {
			t.Errorf("AncestorPaths(%q) ends at %q, want the root", path, got[len(got)-1])
		}
	}
}

// TestBlobPath pins the addressing rule. Two halves of the system have to
// agree about where a file is, so the shard is two characters here and
// nowhere else.
func TestBlobPath(t *testing.T) {
	sha := strings.Repeat("ab", 32)
	got, err := BlobPath(sha)
	if err != nil {
		t.Fatalf("BlobPath: %v", err)
	}
	if want := "blobs/ab/" + sha; got != want {
		t.Errorf("BlobPath = %q, want %q", got, want)
	}

	for _, bad := range []string{
		"",
		"abc",
		strings.Repeat("ab", 31),
		strings.Repeat("AB", 32), // uppercase is a second spelling of one filename
		strings.Repeat("ag", 32), // not hex
		"../" + strings.Repeat("a", 61),
	} {
		if _, err := BlobPath(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("BlobPath(%q) = %v, want an invalid-input error", bad, err)
		}
	}
}

// TestTemplateErrorCarriesItsLocation is what PLAN.md M8 acceptance 5 asks the
// transport edge to be able to report.
func TestTemplateErrorCarriesItsLocation(t *testing.T) {
	inner := errors.New("nil pointer evaluating .Version.Title")
	err := fmt.Errorf("rendering: %w", &TemplateError{
		Template: "example.com/features/story.gohtml",
		Line:     12,
		Phase:    PhaseExecute,
		Err:      inner,
	})

	te, ok := TemplateErrorOf(err)
	if !ok {
		t.Fatal("TemplateErrorOf did not find the template failure through a wrap")
	}
	if te.Template != "example.com/features/story.gohtml" || te.Line != 12 || te.Phase != PhaseExecute {
		t.Errorf("TemplateErrorOf = %+v, want the template, the line, and the phase", te)
	}
	if !errors.Is(err, inner) {
		t.Error("a template failure does not unwrap to what html/template said")
	}

	// It answers to no sentinel, which is what makes it a 500 rather than a
	// refusal about the document or the caller's request.
	for _, sentinel := range []error{ErrInvalid, ErrNotFound, ErrConflict, ErrForbidden, ErrGuardFailed} {
		if errors.Is(err, sentinel) {
			t.Errorf("a broken template answers to %v; it is this installation's configuration failing, not a refusal", sentinel)
		}
	}

	if got := te.Error(); !strings.Contains(got, "line 12") || !strings.Contains(got, "execute") {
		t.Errorf("Error() = %q, want the line and the phase in it", got)
	}
}

// TestTemplateErrorOfFindsNothing is the negative half: an ordinary error is
// not a template failure, and the transport edge must not decorate it as one.
func TestTemplateErrorOfFindsNothing(t *testing.T) {
	if _, ok := TemplateErrorOf(fmt.Errorf("document: %w", ErrNotFound)); ok {
		t.Error("TemplateErrorOf found a template failure in an ordinary error")
	}
	if _, ok := TemplateErrorOf(nil); ok {
		t.Error("TemplateErrorOf found a template failure in nil")
	}
}
