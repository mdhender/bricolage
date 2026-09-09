// Copyright (c) 2026 Michael D Henderson.

package render

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/mdhender/bricolage/internal/domain"
)

// update regenerates the golden file (AGENTS.md, "Testing"):
//
//	go test ./internal/render/ -run TestRenderIsDeterministic -update
var update = flag.Bool("update", false, "rewrite the golden files")

// storyTemplate is the fixture every test in this file renders. It touches
// every part of the context, so a field silently dropped from Context shows up
// as a golden-file diff rather than as nothing.
const storyTemplate = `<!doctype html>
<title>{{.Version.Title}}</title>
{{if .Preview}}<p class="preview">Preview of {{.Document.UID}} ({{.Mode}})</p>{{end}}
<article>
  <h1>{{.Version.Title}}</h1>
  <p class="deck">{{.Content.deck}}</p>
  <div class="body">{{raw (printf "%s" .Content.body)}}</div>
  <footer>
    <span class="site">{{.Site.Name}} ({{.Site.Domain}})</span>
    <span class="channel">{{.Channel.Name}} {{.Channel.Filename}}.{{.Channel.FileExt}}</span>
    <span class="category">{{.Category.Name}} {{.Category.Path}} depth {{.Category.Depth}}</span>
    <span class="version">v{{.Version.Number}} {{.Version.Slug}} {{.Version.CoverDate}} draft={{.Version.Draft}}</span>
    <span class="state">{{.Document.Kind}}/{{.Document.ElementType}} {{.Document.State}}</span>
    <a href="{{.URL}}">{{.URI}}</a>
  </footer>
  <ul>{{range $k, $v := .Content}}<li>{{$k}}={{$v}}</li>{{end}}</ul>
</article>
`

// input is the render every test in this file performs, in the given mode.
func input(mode string) Input {
	return Input{
		Mode:    mode,
		Site:    domain.Site{Name: "Default", Domain: site},
		Channel: domain.OutputChannel{Name: "Web", Filename: "index", FileExt: "html"},
		Category: domain.Category{
			ID: 4, ParentID: 3, SiteID: 1,
			Directory: "film", Path: "/features/film/", Name: "Film",
		},
		Document: domain.Document{
			UID: "01JQ0000000000000000000000", Kind: domain.KindStory,
			ElementTypeKey: "story", State: "draft",
		},
		Version: domain.Version{
			ID: 7, Number: 3, Title: "A Film Piece", Slug: "a-film-piece",
			CoverDate:   "2026-03-01",
			Content:     `{"body":"<p>The quick brown <em>fox</em>.</p>","deck":"A deck & a half","rank":12}`,
			CheckedInAt: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
		},
		URI: "/features/film/2026/03/01/a-film-piece",
		URL: "https://" + site + "/features/film/2026/03/01/a-film-piece",
	}
}

func engineWith(t *testing.T, source string) *Engine {
	t.Helper()
	e, err := New(Options{FS: fstest.MapFS{
		site + "/features/film/story.gohtml": &fstest.MapFile{Data: []byte(source)},
	}})
	if err != nil {
		t.Fatalf("render.New: %v", err)
	}
	return e
}

// TestRenderIsDeterministic is PLAN.md M8 acceptance 3: same version, same
// template, same bytes. It is golden because "the same" has to mean the same
// across runs and not merely across two calls in one process -- a map ranged
// in a template is the classic way for that to stop being true.
func TestRenderIsDeterministic(t *testing.T) {
	e := engineWith(t, storyTemplate)

	first, err := e.Render(input(domain.ModePublish))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for i := range 20 {
		again, err := e.Render(input(domain.ModePublish))
		if err != nil {
			t.Fatalf("Render %d: %v", i, err)
		}
		if !bytes.Equal(first.Body, again.Body) {
			t.Fatalf("render %d differs from the first; a template that ranges a map must still produce one set of bytes", i)
		}
	}
	compareGolden(t, "story_publish.golden", string(first.Body))

	// Preview is the same template and different bytes, which is the only
	// difference between the two modes inside this package: a preview says so
	// on the page, so nobody mistakes it for what is live.
	preview, err := e.Render(input(domain.ModePreview))
	if err != nil {
		t.Fatalf("Render(preview): %v", err)
	}
	compareGolden(t, "story_preview.golden", string(preview.Body))
	if bytes.Equal(first.Body, preview.Body) {
		t.Error("publish and preview produced identical bytes; a template that asks which mode it is in got the same answer twice")
	}
}

// TestRenderIsDeterministicConcurrently is the same property under -race: the
// cache is shared and a parsed template is executed, never modified.
func TestRenderIsDeterministicConcurrently(t *testing.T) {
	e := engineWith(t, storyTemplate)
	want, err := e.Render(input(domain.ModePublish))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 10 {
				got, err := e.Render(input(domain.ModePublish))
				if err != nil {
					t.Errorf("Render: %v", err)
					return
				}
				if !bytes.Equal(got.Body, want.Body) {
					t.Error("a concurrent render produced different bytes")
					return
				}
			}
		})
	}
	wg.Wait()
}

// TestValidateModeWritesNothingAndReportsParseErrors is PLAN.md M8
// acceptance 2.
func TestValidateModeWritesNothingAndReportsParseErrors(t *testing.T) {
	t.Run("a template that parses", func(t *testing.T) {
		e := engineWith(t, storyTemplate)
		got, err := e.Render(input(domain.ModeValidate))
		if err != nil {
			t.Fatalf("Render(validate): %v", err)
		}
		if got.Body != nil {
			t.Errorf("validate produced %d bytes, want none", len(got.Body))
		}
		if got.Template != site+"/features/film/story.gohtml" {
			t.Errorf("validate chose %q, want the template it would have run", got.Template)
		}
	})

	t.Run("a template that does not", func(t *testing.T) {
		e := engineWith(t, "line one\nline two\n{{if .Version.Title}}unclosed\n")
		got, err := e.Render(input(domain.ModeValidate))
		if err == nil {
			t.Fatal("validate accepted a template that will not parse")
		}
		if got.Body != nil {
			t.Error("validate produced bytes for a template that will not parse")
		}

		te, ok := domain.TemplateErrorOf(err)
		if !ok {
			t.Fatalf("Render(validate) = %T, want a *domain.TemplateError", err)
		}
		if te.Phase != domain.PhaseParse {
			t.Errorf("phase = %q, want %q", te.Phase, domain.PhaseParse)
		}
		if te.Template != site+"/features/film/story.gohtml" {
			t.Errorf("template = %q, want the file that would not parse", te.Template)
		}
		if te.Line == 0 {
			t.Error("the parse failure names no line; the person who has to fix it is reading the whole file without one")
		}
	})
}

// TestExecutionFailureNamesTheTemplateAndLine is half of PLAN.md M8
// acceptance 5. The other half -- that no partial file is written -- is a
// property of the shape and is asserted in the service tests, where there is a
// directory to look in.
func TestExecutionFailureNamesTheTemplateAndLine(t *testing.T) {
	// Line 3 calls a method the context does not have, which text/template
	// only discovers while executing.
	e := engineWith(t, "one\ntwo\n{{.Version.NoSuchThing}}\nfour\n")

	got, err := e.Render(input(domain.ModePublish))
	if err == nil {
		t.Fatal("Render succeeded against a template that cannot execute")
	}
	if got.Body != nil {
		t.Error("a failed execution returned bytes; there must be nothing for a caller to write")
	}

	te, ok := domain.TemplateErrorOf(err)
	if !ok {
		t.Fatalf("Render = %T, want a *domain.TemplateError", err)
	}
	if te.Phase != domain.PhaseExecute {
		t.Errorf("phase = %q, want %q", te.Phase, domain.PhaseExecute)
	}
	if !strings.HasSuffix(te.Template, "story.gohtml") {
		t.Errorf("template = %q, want the template that failed", te.Template)
	}
	if te.Line != 3 {
		t.Errorf("line = %d, want 3", te.Line)
	}
}

// TestRenderRefusesAnUnknownMode keeps the closed vocabulary closed at the one
// place a caller could widen it.
func TestRenderRefusesAnUnknownMode(t *testing.T) {
	e := engineWith(t, storyTemplate)
	in := input("prevue")
	if _, err := e.Render(in); !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("Render(mode %q) = %v, want a refusal naming the three modes", in.Mode, err)
	}
}

// TestRenderRefusesContentThatIsNotAnObject covers previewing a working draft
// that somebody has broken. It may be invalid against its element type's
// schema; it may not be something other than JSON.
func TestRenderRefusesContentThatIsNotAnObject(t *testing.T) {
	e := engineWith(t, storyTemplate)
	in := input(domain.ModePublish)
	in.Version.Content = `["not", "an", "object"]`
	if _, err := e.Render(in); !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("Render with array content = %v, want a refusal", err)
	}
}

// TestRenderWithNoContent is the other end of it: a document nobody has
// written yet renders, because {{.Content.body}} on an empty map is empty and
// not a failure.
func TestRenderWithNoContent(t *testing.T) {
	e := engineWith(t, `<title>{{.Version.Title}}</title>{{.Content.body}}`)
	in := input(domain.ModePublish)
	in.Version.Content = ""
	got, err := e.Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if want := "<title>A Film Piece</title>"; string(got.Body) != want {
		t.Errorf("Render = %q, want %q", got.Body, want)
	}
}

// TestReloadPicksUpAnEdit is the row DESIGN.md 14 gives: development re-reads
// a template on every render, production parses it once and keeps it.
//
// The tree here has no site directory -- the template sits at the root of the
// FS and the render is given an empty site domain -- because t.TempDir already
// exists and nothing in this system creates a directory (invariant 19), test
// helper included.
func TestReloadPicksUpAnEdit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "story.gohtml")
	if err := os.WriteFile(path, []byte("first"), 0o600); err != nil {
		t.Fatalf("writing the template: %v", err)
	}

	renderRoot := func(e *Engine) string {
		t.Helper()
		in := input(domain.ModePublish)
		in.Site.Domain = ""
		in.Category.Path = domain.RootPath
		got, err := e.Render(in)
		if err != nil {
			t.Fatalf("Render: %v", err)
		}
		return string(got.Body)
	}

	cached, err := New(Options{Root: dir})
	if err != nil {
		t.Fatalf("render.New: %v", err)
	}
	reloading, err := New(Options{Root: dir, Reload: true})
	if err != nil {
		t.Fatalf("render.New: %v", err)
	}

	if got := renderRoot(cached); got != "first" {
		t.Fatalf("cached render = %q, want %q", got, "first")
	}
	if got := renderRoot(reloading); got != "first" {
		t.Fatalf("reloading render = %q, want %q", got, "first")
	}

	if err := os.WriteFile(path, []byte("second"), 0o600); err != nil {
		t.Fatalf("rewriting the template: %v", err)
	}
	if got := renderRoot(reloading); got != "second" {
		t.Errorf("after an edit, development render = %q, want %q", got, "second")
	}
	if got := renderRoot(cached); got != "first" {
		t.Errorf("after an edit, production render = %q, want the parse it kept, %q", got, "first")
	}
}

// TestNewRefusesAMissingRoot is invariant 19 seen from here: a renderer that
// made an empty tree when it could not find one would report "no template" for
// every document on the site rather than "there is no template tree here".
func TestNewRefusesAMissingRoot(t *testing.T) {
	if _, err := New(Options{Root: filepath.Join(t.TempDir(), "nope")}); err == nil {
		t.Error("render.New accepted a directory that does not exist")
	}
	if _, err := New(Options{}); !errors.Is(err, domain.ErrUnavailable) {
		t.Error("render.New with no root did not say the server has no template tree")
	}

	file := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("writing: %v", err)
	}
	if _, err := New(Options{Root: file}); err == nil {
		t.Error("render.New accepted a file as a template tree")
	}
}

// compareGolden asserts got against testdata/name, rewriting it under -update.
func compareGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v (regenerate with -update)", path, err)
	}
	if got != string(want) {
		t.Errorf("%s differs.\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}
