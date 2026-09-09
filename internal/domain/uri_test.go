// Copyright (c) 2026 Michael D Henderson.

package domain

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"time"
)

// The URI tests (PLAN.md M7 acceptance 3 and 4).
//
// Acceptance 3 asks for golden test vectors: root category, nested category,
// slug on and off, fixed URI format, a format containing %Y/%m/%d, and a
// document with no cover date. They are golden rather than inline because the
// output is what a reader of a review most needs to see -- a URI format change
// shows up in the diff as the addresses it produces, not as a format string
// somebody has to expand in their head.
//
// Acceptance 4 is a property: the category token consumes the following slash,
// so no URI ever contains "//", over random category depths.

// webChannel is the output channel "cmsdb seed" creates.
func webChannel() OutputChannel {
	return OutputChannel{
		UID: "OC1", SiteID: 1, Name: "Web",
		Protocol: DefaultProtocol, Filename: DefaultFilename, FileExt: DefaultFileExt,
		URIFormat:      DefaultURIFormat,
		FixedURIFormat: DefaultFixedURIFormat,
		UseSlug:        true,
		URICase:        URICaseMixed,
	}
}

// TestBuildURIVectors is PLAN.md M7 acceptance 3.
func TestBuildURIVectors(t *testing.T) {
	root := Category{ID: 1, SiteID: 1, Path: "/", Name: "Site"}
	nested := Category{ID: 3, SiteID: 1, ParentID: 2, Directory: "film", Path: "/features/film/", Name: "Film"}

	story := Document{UID: "D1", SiteID: 1, Kind: KindStory, ElementTypeKey: "story"}
	fixed := Document{UID: "D2", SiteID: 1, Kind: KindStory, ElementTypeKey: "page", ElementTypeFixedURI: true}

	dated := Version{Number: 1, Title: "A Story", Slug: "a-story", CoverDate: "2026-03-01"}
	undated := Version{Number: 1, Title: "About Us", Slug: "about-us"}

	slugOff := webChannel()
	slugOff.UseSlug = false

	// The case rules are exercised against a format with no date conversion,
	// so that the vector is about the case and not about the cover date.
	lower := webChannel()
	lower.URICase = URICaseLower
	lower.URIFormat = TokenCategories + "/" + TokenSlug

	upper := webChannel()
	upper.URICase = URICaseUpper
	upper.URIFormat = TokenCategories + "/" + TokenSlug

	plainDate := webChannel()
	plainDate.URIFormat = TokenCategories + "/%Y/%m/%d"

	noCategory := webChannel()
	noCategory.URIFormat = "/archive/%Y/" + TokenSlug

	var b strings.Builder
	for _, tc := range []struct {
		name string
		doc  Document
		ver  Version
		cat  Category
		oc   OutputChannel
	}{
		{"root category", story, dated, root, webChannel()},
		{"nested category", story, dated, nested, webChannel()},
		{"slug off", story, dated, nested, slugOff},
		{"slug off at the root", story, dated, root, slugOff},
		{"fixed uri format", fixed, undated, nested, webChannel()},
		{"fixed uri format at the root", fixed, undated, root, webChannel()},
		{"a format containing %Y/%m/%d", story, dated, nested, plainDate},
		{"a document with no cover date", story, undated, nested, webChannel()},
		{"no cover date and a format that needs none", fixed, undated, nested, webChannel()},
		{"lower case", story, Version{Slug: "A-Story"}, Category{SiteID: 1, Path: "/Features/"}, lower},
		{"upper case", story, Version{Slug: "a-story"}, Category{SiteID: 1, Path: "/features/"}, upper},
		{"a literal path with no category token", story, dated, nested, noCategory},
		{"a slug with a slash in it", story, Version{Slug: "a/b", CoverDate: "2026-03-01"}, nested, webChannel()},
	} {
		fmt.Fprintf(&b, "%s\n", tc.name)
		fmt.Fprintf(&b, "  format   %s\n", formatOf(tc.doc, tc.oc))
		fmt.Fprintf(&b, "  category %s\n", tc.cat.Path)
		fmt.Fprintf(&b, "  slug     %q (use_slug=%t)\n", tc.ver.Slug, tc.oc.UseSlug)
		fmt.Fprintf(&b, "  cover    %q\n", tc.ver.CoverDate)

		uri, err := BuildURI(tc.doc, tc.ver, tc.cat, tc.oc)
		if err != nil {
			fmt.Fprintf(&b, "  ERROR    %v\n\n", err)
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("%s: BuildURI refused with %v, which does not wrap ErrInvalid", tc.name, err)
			}
			continue
		}
		fmt.Fprintf(&b, "  uri      %s\n", uri)
		fmt.Fprintf(&b, "  file     %s\n\n", tc.oc.FileURI(uri))

		if strings.Contains(uri, "//") {
			t.Errorf("%s: the URI %q contains a doubled slash", tc.name, uri)
		}
		if !strings.HasPrefix(uri, "/") {
			t.Errorf("%s: the URI %q is not absolute", tc.name, uri)
		}
	}

	compareGolden(t, "uri_vectors.golden", b.String())
}

func formatOf(d Document, oc OutputChannel) string {
	if d.ElementTypeFixedURI {
		return oc.FixedURIFormat
	}
	return oc.URIFormat
}

// TestNoURIContainsADoubledSlash is PLAN.md M7 acceptance 4.
//
// The category token consumes the slash that follows it, and category paths
// already end in one, so the two never produce an empty segment. It is a
// property over random depths because the failure is a function of how many
// segments the path has: a format that works at depth 1 works at depth 5, and
// a bug in the consumption rule shows at every depth including 0.
func TestNoURIContainsADoubledSlash(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 11))
	segments := []string{"a", "features", "film", "reviews", "2026", "x-y"}

	formats := []string{
		TokenCategories + "/%Y/%m/%d/" + TokenSlug,
		TokenCategories + "/" + TokenSlug,
		TokenCategories + TokenSlug,
		TokenCategories + "/%Y",
		TokenCategories,
		TokenCategories + "/archive/" + TokenSlug,
	}

	for i := 0; i < 500; i++ {
		path := RootPath
		depth := rng.IntN(6)
		for d := 0; d < depth; d++ {
			next, err := JoinPath(path, segments[rng.IntN(len(segments))])
			if err != nil {
				t.Fatal(err)
			}
			path = next
		}

		oc := webChannel()
		oc.URIFormat = formats[rng.IntN(len(formats))]
		oc.UseSlug = rng.IntN(2) == 0

		uri, err := BuildURI(
			Document{SiteID: 1},
			Version{Slug: "the-slug", CoverDate: "2026-03-01"},
			Category{SiteID: 1, Path: path},
			oc,
		)
		if err != nil {
			t.Fatalf("BuildURI(%q, %q): %v", path, oc.URIFormat, err)
		}
		if strings.Contains(uri, "//") {
			t.Fatalf("format %q at %q produced %q, which contains a doubled slash",
				oc.URIFormat, path, uri)
		}
		if !strings.HasPrefix(uri, "/") {
			t.Fatalf("format %q at %q produced %q, which is not absolute",
				oc.URIFormat, path, uri)
		}
		if strings.HasSuffix(uri, "/") && uri != "/" {
			t.Fatalf("format %q at %q produced %q, which ends in a slash",
				oc.URIFormat, path, uri)
		}
	}
}

func TestOutputChannelValidate(t *testing.T) {
	base := webChannel()
	for _, tc := range []struct {
		name   string
		mutate func(*OutputChannel)
		ok     bool
	}{
		{"the seeded channel", func(*OutputChannel) {}, true},
		{"no site", func(oc *OutputChannel) { oc.SiteID = 0 }, false},
		{"no name", func(oc *OutputChannel) { oc.Name = "" }, false},
		{"no uri format", func(oc *OutputChannel) { oc.URIFormat = "" }, false},
		{"no fixed uri format", func(oc *OutputChannel) { oc.FixedURIFormat = "" }, false},
		{"a case rule nobody implements", func(oc *OutputChannel) { oc.URICase = "title" }, false},
		{"a strftime conversion nobody implements", func(oc *OutputChannel) {
			oc.URIFormat = TokenCategories + "/%Q"
		}, false},
		{"a trailing percent", func(oc *OutputChannel) { oc.URIFormat = TokenCategories + "/%" }, false},
	} {
		oc := base
		tc.mutate(&oc)
		err := oc.Validate()
		if tc.ok != (err == nil) {
			t.Errorf("%s: Validate = %v, want ok=%t", tc.name, err, tc.ok)
		}
		if err != nil && !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Validate = %v, want it to wrap ErrInvalid", tc.name, err)
		}
	}
}

func TestParseCoverDate(t *testing.T) {
	want := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for _, s := range []string{"2026-03-01", "2026-03-01T00:00:00Z", "2026-03-01T00:00:00.000Z"} {
		got, err := ParseCoverDate(s)
		if err != nil {
			t.Errorf("ParseCoverDate(%q): %v", s, err)
			continue
		}
		if !got.Equal(want) {
			t.Errorf("ParseCoverDate(%q) = %v, want %v", s, got, want)
		}
	}
	for _, s := range []string{"", "yesterday", "01/03/2026"} {
		if _, err := ParseCoverDate(s); !errors.Is(err, ErrInvalid) {
			t.Errorf("ParseCoverDate(%q) = %v, want ErrInvalid", s, err)
		}
	}
}

func TestFileURIAndURL(t *testing.T) {
	oc := webChannel()
	if got := oc.FileURI("/features/film/a-story"); got != "/features/film/a-story/index.html" {
		t.Errorf("FileURI = %q", got)
	}
	if got := oc.FileURI("/"); got != "/index.html" {
		t.Errorf("FileURI at the root = %q, want %q", got, "/index.html")
	}
	if got := oc.URL("example.com", "/a"); got != "https://example.com/a" {
		t.Errorf("URL = %q", got)
	}
}
