// Copyright (c) 2026 Michael D Henderson.

package service

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/render"
)

// The service half of M8. A preview is a read that writes a file, so what
// these tests assert is which version was rendered, what reached the scratch
// tree, and what did not reach it when the render failed.

// previewHarness is a category tree, one output channel, one filed document,
// and a template tree that can be rewritten between renders.
type previewHarness struct {
	*catHarness
	templates fstest.MapFS
	scratch   *render.Scratch
	doc       DocumentView
}

// siteDomain is the domain newHarness gives its site, and therefore the
// directory the template tree hangs the category tree off.
const siteDomain = "example.com"

func newPreviewHarness(t *testing.T, files map[string]string) *previewHarness {
	t.Helper()

	templates := fstest.MapFS{}
	for name, body := range files {
		templates[name] = &fstest.MapFile{Data: []byte(body)}
	}
	engine, err := render.New(render.Options{FS: templates, Reload: true})
	if err != nil {
		t.Fatalf("render.New: %v", err)
	}
	scratch, err := render.NewScratch(t.TempDir())
	if err != nil {
		t.Fatalf("render.NewScratch: %v", err)
	}

	// The category tree and the element type come from the shared M7
	// harness; the service is rebuilt over the same store so that it carries
	// the renderer.
	h := newCatHarness(t)
	svc, err := New(h.db, Options{Clock: h.clock, Renderer: engine, Preview: scratch})
	if err != nil {
		t.Fatalf("service.New: %v", err)
	}
	h.Service = svc

	if _, err := h.CreateOutputChannel(t.Context(), h.owner, domain.OutputChannel{
		SiteID: h.siteID, Name: "Web", UseSlug: true,
	}); err != nil {
		t.Fatalf("CreateOutputChannel: %v", err)
	}

	view, err := h.CreateDocument(t.Context(), h.owner, domain.NewDocument{
		SiteID:         h.siteID,
		Kind:           domain.KindStory,
		ElementTypeKey: "story",
		Title:          "A Film Piece",
		Slug:           "a-film-piece",
		CoverDate:      "2026-03-01",
		Content:        `{"body":"draft one"}`,
	})
	if err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}
	if _, _, err := h.FileDocument(t.Context(), h.owner, view.Document.UID, []string{"/features/film/"}); err != nil {
		t.Fatalf("FileDocument: %v", err)
	}

	return &previewHarness{catHarness: h, templates: templates, scratch: scratch, doc: view}
}

// preview renders the harness's document and fails the test if it cannot.
func (h *previewHarness) preview(t *testing.T) PreviewResult {
	t.Helper()
	got, err := h.Preview(t.Context(), h.owner, PreviewRequest{UID: h.doc.Document.UID})
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	return got
}

// body reads a written preview back off the scratch tree.
func (h *previewHarness) body(t *testing.T, result PreviewResult) string {
	t.Helper()
	f, err := h.PreviewOpen(result.Entry.Name)
	if err != nil {
		t.Fatalf("PreviewOpen(%q): %v", result.Entry.Name, err)
	}
	defer func() { _ = f.Close() }()
	body, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("reading the preview: %v", err)
	}
	if len(body) != result.Entry.Bytes {
		t.Errorf("the preview holds %d bytes, want the %d the result reported", len(body), result.Entry.Bytes)
	}
	return string(body)
}

// files lists what is in the scratch tree.
func (h *previewHarness) files(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(h.scratch.Dir())
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// TestPreviewRendersTheDraftAndThenTheVersion is PLAN.md M8 acceptance 4:
// preview of a checked-out draft renders the draft, and preview of a
// checked-in document renders the current version.
func TestPreviewRendersTheDraftAndThenTheVersion(t *testing.T) {
	h := newPreviewHarness(t, map[string]string{
		siteDomain + "/story.gohtml": `[{{.Version.Number}} draft={{.Version.Draft}}] {{.Content.body}}`,
	})
	uid := h.doc.Document.UID

	// A freshly created document has an open working draft, so the first
	// preview is of the draft.
	first := h.preview(t)
	if got, want := h.body(t, first), "[1 draft=true] draft one"; got != want {
		t.Errorf("preview of a new document = %q, want %q", got, want)
	}

	if _, err := h.Checkout(t.Context(), h.owner, uid); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if _, err := h.Checkin(t.Context(), h.owner, uid, "first cut"); err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	checkedIn := h.preview(t)
	if got, want := h.body(t, checkedIn), "[1 draft=false] draft one"; got != want {
		t.Errorf("preview after check-in = %q, want %q", got, want)
	}

	// Check out and edit. The draft is now version 2 and the checked-in
	// version 1 is what a publish would still pin; a preview shows the draft,
	// because the person previewing is the person editing it.
	if _, err := h.Checkout(t.Context(), h.owner, uid); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	body := `{"body":"draft two"}`
	if _, err := h.UpdateDraft(t.Context(), h.owner, uid, domain.DraftUpdate{Content: &body}); err != nil {
		t.Fatalf("UpdateDraft: %v", err)
	}
	editing := h.preview(t)
	if got, want := h.body(t, editing), "[2 draft=true] draft two"; got != want {
		t.Errorf("preview of a checked-out draft = %q, want %q", got, want)
	}
	if editing.Entry.Name == checkedIn.Entry.Name {
		t.Error("the draft and the checked-in version produced one file; content addressing has stopped distinguishing them")
	}
}

// TestPreviewWalksUpTheCategoryTree is the cascade seen through the service:
// the same document finds three different templates as they are removed.
func TestPreviewWalksUpTheCategoryTree(t *testing.T) {
	h := newPreviewHarness(t, map[string]string{
		siteDomain + "/features/film/story.gohtml": "deepest",
		siteDomain + "/features/story.gohtml":      "middle",
		siteDomain + "/story.gohtml":               "root",
	})

	for _, tc := range []struct{ remove, want, template string }{
		{"", "deepest", siteDomain + "/features/film/story.gohtml"},
		{siteDomain + "/features/film/story.gohtml", "middle", siteDomain + "/features/story.gohtml"},
		{siteDomain + "/features/story.gohtml", "root", siteDomain + "/story.gohtml"},
	} {
		if tc.remove != "" {
			delete(h.templates, tc.remove)
		}
		got := h.preview(t)
		if body := h.body(t, got); body != tc.want {
			t.Errorf("after removing %q the preview rendered %q, want %q", tc.remove, body, tc.want)
		}
		if got.Template != tc.template {
			t.Errorf("the result names template %q, want %q", got.Template, tc.template)
		}
	}

	// With none anywhere, a 404 naming the element type and every path
	// searched (PLAN.md M8 acceptance 1).
	delete(h.templates, siteDomain+"/story.gohtml")
	_, err := h.Preview(t.Context(), h.owner, PreviewRequest{UID: h.doc.Document.UID})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Preview with no template anywhere = %v, want not found", err)
	}
	for _, want := range []string{"story", siteDomain + "/features/film/story.gohtml", siteDomain + "/story.gohtml"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not name %q", err, want)
		}
	}
}

// TestPreviewOfABrokenTemplateWritesNothing is the other half of PLAN.md M8
// acceptance 5: an execution failure leaves no partial file.
func TestPreviewOfABrokenTemplateWritesNothing(t *testing.T) {
	h := newPreviewHarness(t, map[string]string{
		siteDomain + "/story.gohtml": "one\ntwo\n{{.Version.NoSuchThing}}\n",
	})

	_, err := h.Preview(t.Context(), h.owner, PreviewRequest{UID: h.doc.Document.UID})
	if err == nil {
		t.Fatal("Preview succeeded against a template that cannot execute")
	}
	te, ok := domain.TemplateErrorOf(err)
	if !ok {
		t.Fatalf("Preview = %T, want a *domain.TemplateError naming the template and the line", err)
	}
	if te.Line != 3 {
		t.Errorf("line = %d, want 3", te.Line)
	}
	if names := h.files(t); len(names) != 0 {
		t.Errorf("the scratch tree holds %v after a failed render, want nothing -- not even a partial file", names)
	}
}

// TestValidateReportsABrokenTemplate is PLAN.md M8 acceptance 2 through the
// service: the report comes back as a result rather than as an error, and
// nothing is written.
func TestValidateReportsABrokenTemplate(t *testing.T) {
	h := newPreviewHarness(t, map[string]string{
		siteDomain + "/story.gohtml": "line one\n{{if .Version.Title}}unclosed\n",
	})

	got, err := h.Preview(t.Context(), h.owner, PreviewRequest{UID: h.doc.Document.UID, Validate: true})
	if err != nil {
		t.Fatalf("Preview(validate) = %v, want the report rather than a failure", err)
	}
	if got.Valid {
		t.Error("validate called a template that will not parse valid")
	}
	if got.Problem == nil || got.Problem.Phase != domain.PhaseParse || got.Problem.Line == 0 {
		t.Errorf("problem = %+v, want a parse failure naming a line", got.Problem)
	}
	if got.Entry.Name != "" {
		t.Error("validate wrote a preview; it writes nothing")
	}
	if names := h.files(t); len(names) != 0 {
		t.Errorf("the scratch tree holds %v after a validation, want nothing", names)
	}

	// A missing template is not a broken one, and validate still refuses it:
	// "there is no template" and "the template does not compile" send
	// different people looking.
	delete(h.templates, siteDomain+"/story.gohtml")
	if _, err := h.Preview(t.Context(), h.owner, PreviewRequest{UID: h.doc.Document.UID, Validate: true}); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("validate with no template = %v, want not found", err)
	}

	t.Run("and a template that parses is reported valid", func(t *testing.T) {
		h.templates[siteDomain+"/story.gohtml"] = &fstest.MapFile{Data: []byte("fine")}
		got, err := h.Preview(t.Context(), h.owner, PreviewRequest{UID: h.doc.Document.UID, Validate: true})
		if err != nil {
			t.Fatalf("Preview(validate): %v", err)
		}
		if !got.Valid || got.Problem != nil {
			t.Errorf("valid = %v, problem = %+v, want valid with no problem", got.Valid, got.Problem)
		}
		if got.Mode != domain.ModeValidate {
			t.Errorf("mode = %q, want %q", got.Mode, domain.ModeValidate)
		}
	})
}

// TestPreviewNeedsReadAndNothingMore is the privilege decision: previewing is
// a read, so a viewer may do it and somebody with no grant may not.
func TestPreviewNeedsReadAndNothingMore(t *testing.T) {
	h := newPreviewHarness(t, map[string]string{siteDomain + "/story.gohtml": "ok"})

	viewer := h.userWithGrant(t, "viewer@example.com", "correct horse battery",
		domain.Grant{Privilege: domain.Read})
	if _, err := h.Preview(t.Context(), viewer, PreviewRequest{UID: h.doc.Document.UID}); err != nil {
		t.Errorf("a reader could not preview: %v", err)
	}

	stranger := h.userWithGrant(t, "stranger@example.com", "correct horse battery", domain.Grant{})
	_, err := h.Preview(t.Context(), stranger, PreviewRequest{UID: h.doc.Document.UID})
	if !errors.Is(err, domain.ErrNotFound) && !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("a stranger previewed the document: %v", err)
	}
}

// TestPreviewRefusesWhatItCannotAddress covers the two states a document can
// be in that have no address, and the channel question a second output channel
// makes the caller answer.
func TestPreviewRefusesWhatItCannotAddress(t *testing.T) {
	h := newPreviewHarness(t, map[string]string{siteDomain + "/story.gohtml": "ok"})

	t.Run("filed nowhere", func(t *testing.T) {
		view, err := h.CreateDocument(t.Context(), h.owner, domain.NewDocument{
			SiteID: h.siteID, Kind: domain.KindStory, ElementTypeKey: "story",
			Title: "Unfiled", Slug: "unfiled", CoverDate: "2026-03-01",
		})
		if err != nil {
			t.Fatalf("CreateDocument: %v", err)
		}
		if _, err := h.Preview(t.Context(), h.owner, PreviewRequest{UID: view.Document.UID}); !errors.Is(err, domain.ErrConflict) {
			t.Errorf("preview of an unfiled document = %v, want a conflict saying it has no address", err)
		}
	})

	t.Run("two channels and no choice", func(t *testing.T) {
		if _, err := h.CreateOutputChannel(t.Context(), h.owner, domain.OutputChannel{
			SiteID: h.siteID, Name: "Print", UseSlug: true,
		}); err != nil {
			t.Fatalf("CreateOutputChannel: %v", err)
		}
		_, err := h.Preview(t.Context(), h.owner, PreviewRequest{UID: h.doc.Document.UID})
		if !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("preview with two channels = %v, want a refusal asking which", err)
		}
		if !strings.Contains(err.Error(), "Print") || !strings.Contains(err.Error(), "Web") {
			t.Errorf("the refusal %q does not name the channels to choose between", err)
		}

		// Naming one answers it.
		channels, err := h.OutputChannels(t.Context(), h.owner, h.siteID)
		if err != nil {
			t.Fatalf("OutputChannels: %v", err)
		}
		if _, err := h.Preview(t.Context(), h.owner, PreviewRequest{
			UID: h.doc.Document.UID, ChannelUID: channels[0].UID,
		}); err != nil {
			t.Errorf("preview naming a channel: %v", err)
		}
	})
}

// TestPreviewOnAServerWithNoTemplateTree is the 503: a request this server was
// not configured to answer, naming the flag it was not given.
func TestPreviewOnAServerWithNoTemplateTree(t *testing.T) {
	h := newCatHarness(t)
	if h.PreviewConfigured() {
		t.Fatal("a service built without a renderer reports previews configured")
	}
	view := filedWithADate(t, h)

	_, err := h.Preview(t.Context(), h.owner, PreviewRequest{UID: view.Document.UID})
	if !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("Preview with no template tree = %v, want unavailable", err)
	}
	if !strings.Contains(err.Error(), "--templates") {
		t.Errorf("the refusal %q does not name the flag that was not given", err)
	}
	if _, err := h.PreviewOpen(strings.Repeat("ab", 32)); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("PreviewOpen with no scratch tree = %v, want unavailable", err)
	}
}

// TestPreviewWithNoScratchTree is the same, half configured: the render works
// and there is nowhere to put it.
func TestPreviewWithNoScratchTree(t *testing.T) {
	engine, err := render.New(render.Options{FS: fstest.MapFS{
		siteDomain + "/story.gohtml": &fstest.MapFile{Data: []byte("ok")},
	}})
	if err != nil {
		t.Fatalf("render.New: %v", err)
	}
	h := newCatHarness(t)
	svc, err := New(h.db, Options{Clock: h.clock, Renderer: engine})
	if err != nil {
		t.Fatalf("service.New: %v", err)
	}
	h.Service = svc
	if _, err := h.CreateOutputChannel(t.Context(), h.owner, domain.OutputChannel{
		SiteID: h.siteID, Name: "Web", UseSlug: true,
	}); err != nil {
		t.Fatalf("CreateOutputChannel: %v", err)
	}
	view := filedWithADate(t, h)

	_, err = h.Preview(t.Context(), h.owner, PreviewRequest{UID: view.Document.UID})
	if !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("Preview with no scratch tree = %v, want unavailable", err)
	}
	if !strings.Contains(err.Error(), "--preview") {
		t.Errorf("the refusal %q does not name the flag that was not given", err)
	}

	// Validate needs no scratch tree, so it still answers.
	if _, err := h.Preview(t.Context(), h.owner, PreviewRequest{UID: view.Document.UID, Validate: true}); err != nil {
		t.Errorf("validate on a server with no scratch tree: %v", err)
	}
}

// TestPreviewWritesNoEvent is the deliberate omission. Invariant 7 asks that
// every state-changing operation write one; a preview changes no state, and an
// event per preview would be an audit log of people looking at things.
func TestPreviewWritesNoEvent(t *testing.T) {
	h := newPreviewHarness(t, map[string]string{siteDomain + "/story.gohtml": "ok"})

	before, err := h.db.EventsForSubject(t.Context(), domain.SubjectDocument, h.doc.Document.ID, 100)
	if err != nil {
		t.Fatalf("EventsForSubject: %v", err)
	}
	h.preview(t)
	after, err := h.db.EventsForSubject(t.Context(), domain.SubjectDocument, h.doc.Document.ID, 100)
	if err != nil {
		t.Fatalf("EventsForSubject: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("a preview wrote %d events, want none", len(after)-len(before))
	}
}

// filedWithADate creates a story with a cover date and a slug, filed in
// /features/film/.
//
// The shared M7 helper creates one with neither, which is a document the
// seeded URI format -- %{categories}/%Y/%m/%d/%{slug} -- can build no address
// for. A preview of it would fail for that reason rather than the one the test
// is about.
func filedWithADate(t *testing.T, h *catHarness) DocumentView {
	t.Helper()
	view, err := h.CreateDocument(t.Context(), h.owner, domain.NewDocument{
		SiteID:         h.siteID,
		Kind:           domain.KindStory,
		ElementTypeKey: "story",
		Title:          "A Film Piece",
		Slug:           "a-film-piece",
		CoverDate:      "2026-03-01",
		Content:        `{"body":"Words."}`,
	})
	if err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}
	if _, _, err := h.FileDocument(t.Context(), h.owner, view.Document.UID, []string{"/features/film/"}); err != nil {
		t.Fatalf("FileDocument: %v", err)
	}
	return view
}
