// Copyright (c) 2026 Michael D Henderson.

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/mdhender/bricolage/internal/clock"
	"github.com/mdhender/bricolage/internal/config"
	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/ids"
	"github.com/mdhender/bricolage/internal/render"
	"github.com/mdhender/bricolage/internal/service"
	"github.com/mdhender/bricolage/internal/store"
)

// The transport half of M8. What is under test here is the status code, the
// response shape, and the two headers that decide what a browser does with a
// preview; the rendering itself is asserted in internal/render and the
// refusals in internal/service.

// newPreviewHarness is the document harness with a template tree and a scratch
// tree, and with the preview mount registered beside the API.
func newPreviewHarness(t *testing.T, env config.Environment, files map[string]string) *harness {
	t.Helper()

	db, err := store.OpenMemory(t.Context())
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

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

	c := clock.NewFake(start)
	svc, err := service.New(db, service.Options{Clock: c, Renderer: engine, Preview: scratch})
	if err != nil {
		t.Fatalf("service.New: %v", err)
	}

	mux := http.NewServeMux()
	deps := Deps{Service: svc, Environment: env}
	Register(mux, deps)
	RegisterPreview(mux, deps)

	h := &harness{mux: mux, svc: svc, clock: c, db: db}
	if _, err := h.db.CreateElementType(t.Context(), store.NewElementType{
		UID: ids.MustNew(start), KeyName: "story", Name: "Story",
		Kind: domain.KindStory, TopLevel: true, Schema: storySchema, CreatedAt: start,
	}); err != nil {
		t.Fatalf("CreateElementType: %v", err)
	}
	if _, err := h.db.CreateSite(t.Context(), store.NewSite{
		UID: ids.MustNew(start), Name: "Default", Domain: "example.com",
	}); err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	return h
}

// previewable creates a channel, a filed story with a cover date, and returns
// its uid.
func (h *harness) previewable(t *testing.T, token string) string {
	t.Helper()

	rec := h.do(t, http.MethodPost, "/api/v1/output-channels", token, map[string]any{
		"site": 1, "name": "Web", "use_slug": true,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /output-channels = %d %s", rec.Code, rec.Body.String())
	}

	rec = h.do(t, http.MethodPost, "/api/v1/documents", token, map[string]any{
		"site": 1, "kind": "story", "element_type": "story",
		"title": "A Film Piece", "slug": "a-film-piece", "cover_date": "2026-03-01",
		"content": `{"body":"Words."}`,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /documents = %d %s", rec.Code, rec.Body.String())
	}
	var doc documentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}

	rec = h.do(t, http.MethodPut, "/api/v1/documents/"+doc.UID+"/categories", token,
		map[string]any{"categories": []string{"/"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /categories = %d %s", rec.Code, rec.Body.String())
	}
	return doc.UID
}

// TestPreviewRoundTrip renders a document and fetches what was rendered, which
// is the whole of PLAN.md M8's transport.
func TestPreviewRoundTrip(t *testing.T) {
	h := newPreviewHarness(t, config.Production, map[string]string{
		"example.com/story.gohtml": `<h1>{{.Version.Title}}</h1><p>{{.URI}}</p>`,
	})
	h.user(t, "admin@example.com", domain.Publish)
	token := h.login(t, "admin@example.com")
	uid := h.previewable(t, token)

	rec := h.do(t, http.MethodPost, "/api/v1/documents/"+uid+"/preview", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /preview = %d %s", rec.Code, rec.Body.String())
	}
	var out previewResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Mode != domain.ModePreview || !out.Valid {
		t.Errorf("mode = %q valid = %v, want a valid preview", out.Mode, out.Valid)
	}
	if out.Template != "example.com/story.gohtml" {
		t.Errorf("template = %q, want the one it rendered", out.Template)
	}
	if !strings.HasPrefix(out.Path, render.PreviewPrefix) {
		t.Errorf("path = %q, want it under %q", out.Path, render.PreviewPrefix)
	}
	if out.Checksum == "" || out.Bytes == 0 {
		t.Errorf("the response is %+v, want a checksum and a byte count", out)
	}
	if out.URI != "/2026/03/01/a-film-piece" {
		t.Errorf("uri = %q, want the address the template was given", out.URI)
	}

	t.Run("and the preview is served back", func(t *testing.T) {
		rec := h.do(t, http.MethodGet, out.Path, token, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", out.Path, rec.Code, rec.Body.String())
		}
		want := "<h1>A Film Piece</h1><p>/2026/03/01/a-film-piece</p>"
		if rec.Body.String() != want {
			t.Errorf("body = %q, want %q", rec.Body.String(), want)
		}
		if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
			t.Errorf("Content-Type = %q, want text/html", got)
		}
		// The sandbox is what keeps editor-authored markup from reaching the
		// session cookie of the person previewing it. allow-same-origin would
		// undo the whole header while leaving it looking careful.
		csp := rec.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "sandbox") {
			t.Errorf("Content-Security-Policy = %q, want a sandbox", csp)
		}
		if strings.Contains(csp, "allow-same-origin") {
			t.Errorf("Content-Security-Policy = %q, which sandboxes nothing", csp)
		}
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
		}
	})

	t.Run("and it needs a session", func(t *testing.T) {
		if rec := h.do(t, http.MethodGet, out.Path, "", nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s with no credential = %d, want 401", out.Path, rec.Code)
		}
	})

	t.Run("and a name that is not a preview is refused", func(t *testing.T) {
		for _, name := range []string{"nonsense", strings.Repeat("a", 64) + "x"} {
			rec := h.do(t, http.MethodGet, render.PreviewPrefix+name, token, nil)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Errorf("GET %s = %d, want 422", render.PreviewPrefix+name, rec.Code)
			}
		}
		rec := h.do(t, http.MethodGet, render.PreviewPrefix+strings.Repeat("ab", 32)+".html", token, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET a well-formed name that is not there = %d, want 404", rec.Code)
		}
	})
}

// TestPreviewExecutionFailureIsA500NamingTheTemplate is PLAN.md M8
// acceptance 5 at the edge, in production -- where the detail is generic and
// the template and the line still have to reach the person who must fix them.
func TestPreviewExecutionFailureIsA500NamingTheTemplate(t *testing.T) {
	h := newPreviewHarness(t, config.Production, map[string]string{
		"example.com/story.gohtml": "one\ntwo\n{{.Version.NoSuchThing}}\n",
	})
	h.user(t, "admin@example.com", domain.Publish)
	token := h.login(t, "admin@example.com")
	uid := h.previewable(t, token)

	rec := h.do(t, http.MethodPost, "/api/v1/documents/"+uid+"/preview", token, nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("POST /preview against a broken template = %d %s, want 500", rec.Code, rec.Body.String())
	}
	var p Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Template != "example.com/story.gohtml" {
		t.Errorf("template = %q, want the template that failed", p.Template)
	}
	if p.Line != 3 {
		t.Errorf("line = %d, want 3", p.Line)
	}
	if strings.Contains(p.Detail, "NoSuchThing") {
		t.Errorf("detail = %q; production discloses no underlying detail, which is why the template and line are extension members", p.Detail)
	}
}

// TestPreviewValidate is PLAN.md M8 acceptance 2 at the edge: a template that
// will not parse is a 200 carrying the report, because the question asked was
// whether it compiles.
func TestPreviewValidate(t *testing.T) {
	h := newPreviewHarness(t, config.Production, map[string]string{
		"example.com/story.gohtml": "line one\n{{if .Version.Title}}unclosed\n",
	})
	h.user(t, "admin@example.com", domain.Publish)
	token := h.login(t, "admin@example.com")
	uid := h.previewable(t, token)

	rec := h.do(t, http.MethodPost, "/api/v1/documents/"+uid+"/preview", token,
		map[string]any{"validate": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /preview validate = %d %s, want 200 with the report", rec.Code, rec.Body.String())
	}
	var out previewResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Valid {
		t.Error("the response calls a template that will not parse valid")
	}
	if out.Error == nil || out.Error.Line == 0 || out.Error.Phase != domain.PhaseParse {
		t.Errorf("error = %+v, want a parse failure naming a line", out.Error)
	}
	if out.Path != "" || out.Checksum != "" {
		t.Errorf("validate reported a written file (%+v); it writes nothing", out)
	}
}

// TestPreviewWithNoTemplateAnywhere is PLAN.md M8 acceptance 1 at the edge: a
// 404 naming the element type and the searched paths.
func TestPreviewWithNoTemplateAnywhere(t *testing.T) {
	h := newPreviewHarness(t, config.Development, map[string]string{})
	h.user(t, "admin@example.com", domain.Publish)
	token := h.login(t, "admin@example.com")
	uid := h.previewable(t, token)

	rec := h.do(t, http.MethodPost, "/api/v1/documents/"+uid+"/preview", token, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("POST /preview with no template = %d %s, want 404", rec.Code, rec.Body.String())
	}
	var p Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"story", "example.com/story.gohtml"} {
		if !strings.Contains(p.Detail, want) {
			t.Errorf("detail = %q, want it to name %q", p.Detail, want)
		}
	}
}

// TestPreviewOnAServerWithoutOne is the 503 a half-configured server answers,
// and it is registered so that the route table tells the truth.
func TestPreviewOnAServerWithoutOne(t *testing.T) {
	h := newDocAPIHarness(t) // no renderer, no scratch tree
	mux := http.NewServeMux()
	RegisterPreview(mux, Deps{Service: h.svc, Environment: config.Development})
	h.mux.Handle("GET "+render.PreviewPrefix+"{name}", mux)

	h.user(t, "admin@example.com", domain.Publish)
	token := h.login(t, "admin@example.com")
	doc := h.createDoc(t, token, "A Film Piece")

	rec := h.do(t, http.MethodPost, "/api/v1/documents/"+doc.UID+"/preview", token, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST /preview on a server with no template tree = %d %s, want 503", rec.Code, rec.Body.String())
	}
	var p Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Detail, "--templates") {
		t.Errorf("detail = %q, want it to name the flag that was not given", p.Detail)
	}
}
