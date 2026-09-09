// Copyright (c) 2026 Michael D Henderson.

package render

import (
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/mdhender/bricolage/internal/domain"
)

// The template cascade (PLAN.md M8 acceptance 1). A document in
// "/features/film/" finds a template at "/features/" when none exists at
// "/features/film/", and one at "/" when neither exists; with none anywhere
// the error names the element type and every path that was searched.

const site = "example.com"

// tree builds a template tree holding one file per named path, with contents
// that say which file it is.
func tree(paths ...string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for _, p := range paths {
		fsys[p] = &fstest.MapFile{Data: []byte("I am " + p)}
	}
	return fsys
}

func TestLookupWalksUpTheCategoryTree(t *testing.T) {
	const deep = "/features/film/"

	for _, tc := range []struct {
		name  string
		files []string
		want  string
	}{
		{
			name:  "the deepest category wins",
			files: []string{site + "/features/film/story.gohtml", site + "/features/story.gohtml", site + "/story.gohtml"},
			want:  site + "/features/film/story.gohtml",
		},
		{
			name:  "with none there, its parent",
			files: []string{site + "/features/story.gohtml", site + "/story.gohtml"},
			want:  site + "/features/story.gohtml",
		},
		{
			name:  "with neither, the site root",
			files: []string{site + "/story.gohtml"},
			want:  site + "/story.gohtml",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loc := Locator{FS: tree(tc.files...), Base: site}
			match, err := loc.Lookup(deep, "story")
			if err != nil {
				t.Fatalf("Lookup: %v", err)
			}
			if match.Path != tc.want {
				t.Errorf("Lookup chose %q, want %q", match.Path, tc.want)
			}
			// Searched ends with the match, so a caller can say what the
			// chosen template beat.
			if len(match.Searched) == 0 || match.Searched[len(match.Searched)-1] != match.Path {
				t.Errorf("Searched = %v, want it to end with the match %q", match.Searched, match.Path)
			}
		})
	}
}

// TestLookupWithNoTemplateAnywhere is the fourth case: a clean error naming
// the element type and the searched paths.
func TestLookupWithNoTemplateAnywhere(t *testing.T) {
	loc := Locator{FS: tree(site + "/features/film/feature.gohtml"), Base: site}

	_, err := loc.Lookup("/features/film/", "story")
	if err == nil {
		t.Fatal("Lookup found a template that does not exist")
	}
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Lookup error = %v, want it to answer to ErrNotFound", err)
	}

	var nf *NotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("Lookup error = %T, want a *NotFoundError carrying what was searched", err)
	}
	if nf.KeyName != "story" {
		t.Errorf("the error names element type %q, want %q", nf.KeyName, "story")
	}
	want := []string{
		site + "/features/film/story.gohtml",
		site + "/features/story.gohtml",
		site + "/story.gohtml",
	}
	if len(nf.Searched) != len(want) {
		t.Fatalf("searched %v, want %v", nf.Searched, want)
	}
	for i := range want {
		if nf.Searched[i] != want[i] {
			t.Fatalf("searched %v, want %v", nf.Searched, want)
		}
	}
	for _, p := range want {
		if !strings.Contains(err.Error(), p) {
			t.Errorf("the message %q does not name %q; the two mistakes that cause this are a template in the wrong directory and one with the wrong name, and neither is visible without the list", err.Error(), p)
		}
	}
}

// TestLookupIsPerSite is why the tree has a site level at all: two sites both
// have "/features/", and one site's templates are not the other's.
func TestLookupIsPerSite(t *testing.T) {
	fsys := tree("one.example/story.gohtml", "two.example/features/story.gohtml")

	if _, err := (Locator{FS: fsys, Base: "one.example"}).Lookup("/features/", "story"); err != nil {
		t.Errorf("one.example: %v", err)
	}
	match, err := (Locator{FS: fsys, Base: "two.example"}).Lookup("/features/", "story")
	if err != nil {
		t.Fatalf("two.example: %v", err)
	}
	if match.Path != "two.example/features/story.gohtml" {
		t.Errorf("two.example chose %q, want its own template", match.Path)
	}
	if _, err := (Locator{FS: fsys, Base: "three.example"}).Lookup("/features/", "story"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("a site with no templates found one belonging to another site: %v", err)
	}
}

// TestLookupSkipsADirectory covers the one thing that is a match by name and
// not a template. Walking past it is deliberate: the category above may have a
// real one.
func TestLookupSkipsADirectory(t *testing.T) {
	fsys := fstest.MapFS{
		site + "/features/story.gohtml/keep": &fstest.MapFile{Data: []byte("not a template")},
		site + "/story.gohtml":               &fstest.MapFile{Data: []byte("the real one")},
	}
	match, err := (Locator{FS: fsys, Base: site}).Lookup("/features/", "story")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if match.Path != site+"/story.gohtml" {
		t.Errorf("Lookup chose %q, want it to walk past the directory to %q", match.Path, site+"/story.gohtml")
	}
}

// TestLookupRefusesAnEscapingSegment is the traversal guard. Neither column
// these segments come from constrains its characters, so the refusal is here.
func TestLookupRefusesAnEscapingSegment(t *testing.T) {
	fsys := tree(site + "/story.gohtml")

	for _, tc := range []struct{ base, key string }{
		{site, "../../etc/passwd"},
		{site, ".."},
		{site, "."},
		{site, ""},
		{site, `a\b`},
		{"../..", "story"},
	} {
		t.Run(tc.base+" "+tc.key, func(t *testing.T) {
			if _, err := (Locator{FS: fsys, Base: tc.base}).Lookup("/", tc.key); !errors.Is(err, domain.ErrInvalid) {
				t.Errorf("Lookup(base %q, key %q) = %v, want a refusal", tc.base, tc.key, err)
			}
		})
	}
}

// TestLookupRefusesANonPath keeps the cascade honest about its input: a
// category path is '/'-terminated and absolute, and anything else is not a
// category.
func TestLookupRefusesANonPath(t *testing.T) {
	loc := Locator{FS: tree(site + "/story.gohtml"), Base: site}
	for _, p := range []string{"", "features/", "/features"} {
		if _, err := loc.Lookup(p, "story"); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("Lookup(%q) = %v, want a refusal", p, err)
		}
	}
}
