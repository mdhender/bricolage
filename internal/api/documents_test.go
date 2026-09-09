// Copyright (c) 2026 Michael D Henderson.

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/ids"
	"github.com/mdhender/bricolage/internal/store"
)

// The transport half of M3. What is under test here is the status code and the
// document shape: the refusals themselves are asserted in internal/service,
// where the rows written can be counted.

// docHarness is the API harness with one element type, and the site every
// document is created on.
func newDocAPIHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	if _, err := h.db.CreateElementType(t.Context(), store.NewElementType{
		UID: ids.MustNew(start), KeyName: "story", Name: "Story",
		Kind: domain.KindStory, TopLevel: true, Schema: "{}", CreatedAt: start,
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

// createDoc posts a document and returns the decoded response.
func (h *harness) createDoc(t *testing.T, token, title string) documentResponse {
	t.Helper()
	w := h.do(t, http.MethodPost, "/api/v1/documents", token, map[string]any{
		"site": 1, "kind": "story", "element_type": "story",
		"title": title, "content": `{"body":"The quick brown fox."}`,
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("POST /api/v1/documents = %d %s", w.Code, w.Body)
	}
	var out documentResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestDocumentRoundTrip drives the whole cycle over HTTP, which is what
// "if earl cannot do it, the API is incomplete" means for this milestone.
func TestDocumentRoundTrip(t *testing.T) {
	h := newDocAPIHarness(t)
	h.user(t, "editor@example.com", domain.Create)
	token := h.login(t, "editor@example.com")

	doc := h.createDoc(t, token, "The Quick Brown Fox")
	if doc.UID == "" || doc.Version == nil {
		t.Fatalf("the created document is %+v", doc)
	}
	if !doc.Version.Draft || doc.Version.Version != 1 {
		t.Errorf("the first version is %+v, want an open draft numbered 1", doc.Version)
	}
	if doc.CheckedOutBy != "" {
		t.Errorf("a new document reports itself checked out by %q", doc.CheckedOutBy)
	}

	// The integer primary key never appears in a response (invariant 10).
	if !ids.Valid(doc.UID) {
		t.Errorf("uid = %q, want a ULID", doc.UID)
	}

	base := "/api/v1/documents/" + doc.UID

	if w := h.do(t, http.MethodPost, base+"/checkout", token, nil); w.Code != http.StatusOK {
		t.Fatalf("checkout = %d %s", w.Code, w.Body)
	}

	// A held lease is reported with the holder's uid, never their id.
	w := h.do(t, http.MethodGet, base, token, nil)
	var held documentResponse
	if err := json.Unmarshal(w.Body.Bytes(), &held); err != nil {
		t.Fatal(err)
	}
	if held.CheckedOutBy == "" || held.LockExpiresAt == nil {
		t.Errorf("the checked-out document is %+v", held)
	}

	if w := h.do(t, http.MethodPatch, base, token, map[string]any{
		"title": "The Slow Brown Fox", "content": `{"body":"The slow brown fox."}`,
	}); w.Code != http.StatusOK {
		t.Fatalf("patch = %d %s", w.Code, w.Body)
	}
	if w := h.do(t, http.MethodPost, base+"/checkin", token,
		map[string]any{"note": "first pass"}); w.Code != http.StatusOK {
		t.Fatalf("checkin = %d %s", w.Code, w.Body)
	}

	// Version 1 is now checked in and readable by number.
	w = h.do(t, http.MethodGet, base+"/versions/1", token, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET versions/1 = %d %s", w.Code, w.Body)
	}
	var v1 documentResponse
	if err := json.Unmarshal(w.Body.Bytes(), &v1); err != nil {
		t.Fatal(err)
	}
	if v1.Version.Draft || v1.Version.CheckedInAt == nil {
		t.Errorf("version 1 is %+v, want it checked in", v1.Version)
	}
	if v1.Version.Title != "The Slow Brown Fox" || v1.Version.Note != "first pass" {
		t.Errorf("version 1 = %+v", v1.Version)
	}

	// A second cycle, then a diff.
	if w := h.do(t, http.MethodPost, base+"/checkout", token, nil); w.Code != http.StatusOK {
		t.Fatalf("the second checkout = %d %s", w.Code, w.Body)
	}
	if w := h.do(t, http.MethodPatch, base, token,
		map[string]any{"title": "The Slow Grey Fox"}); w.Code != http.StatusOK {
		t.Fatalf("the second patch = %d %s", w.Code, w.Body)
	}
	if w := h.do(t, http.MethodPost, base+"/checkin", token, nil); w.Code != http.StatusOK {
		t.Fatalf("the second checkin = %d %s", w.Code, w.Body)
	}

	w = h.do(t, http.MethodGet, base+"/diff?from=1&to=2", token, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("diff = %d %s", w.Code, w.Body)
	}
	var diff diffResponse
	if err := json.Unmarshal(w.Body.Bytes(), &diff); err != nil {
		t.Fatal(err)
	}
	if diff.From != 1 || diff.To != 2 {
		t.Errorf("diff = %+v", diff)
	}
	if diff.Text == "" {
		t.Error("the diff carries no rendered text")
	}
	var titleDiff *fieldDiffJSON
	for i := range diff.Fields {
		if diff.Fields[i].Field == "title" {
			titleDiff = &diff.Fields[i]
		}
	}
	if titleDiff == nil || !titleDiff.Changed {
		t.Fatalf("the title diff is %+v", titleDiff)
	}
	var saw = map[string]bool{}
	for _, op := range titleDiff.Ops {
		saw[op.Op] = true
	}
	if !saw["-"] || !saw["+"] {
		t.Errorf("the title diff has ops %v, want a deletion and an insertion", titleDiff.Ops)
	}

	// The versions list, newest first.
	w = h.do(t, http.MethodGet, base+"/versions", token, nil)
	var versions struct {
		Versions []versionResponse `json:"versions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &versions); err != nil {
		t.Fatal(err)
	}
	if len(versions.Versions) != 2 || versions.Versions[0].Version != 2 {
		t.Errorf("versions = %+v, want two, newest first", versions.Versions)
	}

	// And the history, which is a query rather than a log grep.
	w = h.do(t, http.MethodGet, base+"/events", token, nil)
	var history struct {
		Events []eventResponse `json:"events"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &history); err != nil {
		t.Fatal(err)
	}
	if len(history.Events) == 0 {
		t.Fatal("the document has no history")
	}
	for _, e := range history.Events {
		if e.Name == "" {
			t.Errorf("event %q has no display name", e.Type)
		}
		if e.Actor == "" {
			t.Errorf("event %q has no actor", e.Type)
		}
	}
}

// TestDocumentStatusCodes is the table in DESIGN.md 12, for the routes this
// milestone adds. One table, in one place, so that two handlers cannot
// disagree about what "conflict" means.
func TestDocumentStatusCodes(t *testing.T) {
	h := newDocAPIHarness(t)
	h.user(t, "editor@example.com", domain.Create)
	h.user(t, "rival@example.com", domain.Create)
	h.user(t, "reader@example.com", domain.Read)
	h.user(t, "stranger@example.com", domain.NoPrivilege)

	editor := h.login(t, "editor@example.com")
	rival := h.login(t, "rival@example.com")
	reader := h.login(t, "reader@example.com")
	stranger := h.login(t, "stranger@example.com")

	doc := h.createDoc(t, editor, "Contended")
	base := "/api/v1/documents/" + doc.UID

	t.Run("unauthenticated is 401", func(t *testing.T) {
		if w := h.do(t, http.MethodGet, base, "", nil); w.Code != http.StatusUnauthorized {
			t.Errorf("= %d %s", w.Code, w.Body)
		}
	})

	t.Run("an unknown uid is 404", func(t *testing.T) {
		if w := h.do(t, http.MethodGet, "/api/v1/documents/"+ids.MustNew(start), editor, nil); w.Code != http.StatusNotFound {
			t.Errorf("= %d %s", w.Code, w.Body)
		}
	})

	t.Run("a reader editing is 403", func(t *testing.T) {
		if w := h.do(t, http.MethodPost, base+"/checkout", reader, nil); w.Code != http.StatusForbidden {
			t.Errorf("= %d %s", w.Code, w.Body)
		}
	})

	t.Run("a document the caller may not see is 404, not 403", func(t *testing.T) {
		// Answering 403 would confirm that the document exists to somebody who
		// may not see it, which is a disclosure the status code makes for free.
		if w := h.do(t, http.MethodGet, base, stranger, nil); w.Code != http.StatusNotFound {
			t.Errorf("= %d %s", w.Code, w.Body)
		}
	})

	t.Run("a lock held elsewhere is 409", func(t *testing.T) {
		if w := h.do(t, http.MethodPost, base+"/checkout", editor, nil); w.Code != http.StatusOK {
			t.Fatalf("the first checkout = %d %s", w.Code, w.Body)
		}
		w := h.do(t, http.MethodPost, base+"/checkout", rival, nil)
		if w.Code != http.StatusConflict {
			t.Fatalf("= %d %s", w.Code, w.Body)
		}
		var p Problem
		if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		if p.Type != problemBase+"conflict" {
			t.Errorf("problem type = %q", p.Type)
		}
		// The holder is named, never numbered: an internal primary key never
		// appears in a JSON body (invariant 10).
		if !strings.Contains(p.Detail, "Test User") {
			t.Errorf("detail = %q, want it to name who holds the lock", p.Detail)
		}
		if strings.Contains(p.Detail, "user 1") {
			t.Errorf("detail = %q, and it carries an internal user id", p.Detail)
		}
		if w.Header().Get("Content-Type") != ContentTypeProblem {
			t.Errorf("content type = %q, want a problem document", w.Header().Get("Content-Type"))
		}
	})

	t.Run("invalid content is 422", func(t *testing.T) {
		w := h.do(t, http.MethodPatch, base, editor, map[string]any{"content": "not json"})
		if w.Code != http.StatusUnprocessableEntity {
			t.Errorf("= %d %s", w.Code, w.Body)
		}
	})

	t.Run("a version that is not a number is 422", func(t *testing.T) {
		if w := h.do(t, http.MethodGet, base+"/versions/one", editor, nil); w.Code != http.StatusUnprocessableEntity {
			t.Errorf("= %d %s", w.Code, w.Body)
		}
	})

	t.Run("a kind that is not one is 422", func(t *testing.T) {
		w := h.do(t, http.MethodPost, "/api/v1/documents", editor, map[string]any{
			"site": 1, "kind": "article", "element_type": "story", "title": "Nope",
		})
		if w.Code != http.StatusUnprocessableEntity {
			t.Errorf("= %d %s", w.Code, w.Body)
		}
	})
}

// TestRevertDeletingAnswers204 is what a caller sees when reverting removed the
// document: there is nothing left to render, and 204 says so.
func TestRevertDeletingAnswers204(t *testing.T) {
	h := newDocAPIHarness(t)
	h.user(t, "editor@example.com", domain.Create)
	token := h.login(t, "editor@example.com")

	doc := h.createDoc(t, token, "Never Saved")
	base := "/api/v1/documents/" + doc.UID

	if w := h.do(t, http.MethodPost, base+"/revert", token, nil); w.Code != http.StatusNoContent {
		t.Fatalf("revert = %d %s", w.Code, w.Body)
	}
	if w := h.do(t, http.MethodGet, base, token, nil); w.Code != http.StatusNotFound {
		t.Errorf("the document survived the revert: %d %s", w.Code, w.Body)
	}
}

// TestCancelCheckoutReleasesWithoutDiscarding is the reason DELETE on the
// checkout subresource exists at all.
func TestCancelCheckoutReleasesWithoutDiscarding(t *testing.T) {
	h := newDocAPIHarness(t)
	h.user(t, "editor@example.com", domain.Create)
	token := h.login(t, "editor@example.com")

	doc := h.createDoc(t, token, "In Progress")
	base := "/api/v1/documents/" + doc.UID

	if w := h.do(t, http.MethodPost, base+"/checkout", token, nil); w.Code != http.StatusOK {
		t.Fatalf("checkout = %d %s", w.Code, w.Body)
	}
	if w := h.do(t, http.MethodPatch, base, token,
		map[string]any{"title": "Half Written"}); w.Code != http.StatusOK {
		t.Fatalf("patch = %d %s", w.Code, w.Body)
	}

	w := h.do(t, http.MethodDelete, base+"/checkout", token, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("cancel = %d %s", w.Code, w.Body)
	}
	var out documentResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.CheckedOutBy != "" {
		t.Errorf("the lease is still held by %q", out.CheckedOutBy)
	}
	if out.Version == nil || out.Version.Title != "Half Written" {
		t.Errorf("the work was discarded: %+v", out.Version)
	}
}

// TestListDocumentsShowsOnlyWhatMayBeRead keeps the rule that a list never
// shows a row the reader may not open.
func TestListDocumentsShowsOnlyWhatMayBeRead(t *testing.T) {
	h := newDocAPIHarness(t)
	h.user(t, "editor@example.com", domain.Create)
	h.user(t, "stranger@example.com", domain.NoPrivilege)
	editor := h.login(t, "editor@example.com")
	stranger := h.login(t, "stranger@example.com")

	h.createDoc(t, editor, "One")
	h.createDoc(t, editor, "Two")

	for _, tc := range []struct {
		name  string
		token string
		want  int
	}{
		{"the editor sees both", editor, 2},
		{"the stranger sees none", stranger, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := h.do(t, http.MethodGet, "/api/v1/documents", tc.token, nil)
			if w.Code != http.StatusOK {
				t.Fatalf("= %d %s", w.Code, w.Body)
			}
			var out struct {
				Documents []documentResponse `json:"documents"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			if len(out.Documents) != tc.want {
				t.Errorf("got %d documents, want %d", len(out.Documents), tc.want)
			}
		})
	}
}

// TestElementTypesAreListable is what a client needs to name one when creating
// a document.
func TestElementTypesAreListable(t *testing.T) {
	h := newDocAPIHarness(t)
	h.user(t, "editor@example.com", domain.Create)
	token := h.login(t, "editor@example.com")

	w := h.do(t, http.MethodGet, "/api/v1/element-types", token, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("= %d %s", w.Code, w.Body)
	}
	var out struct {
		ElementTypes []elementTypeResponse `json:"element_types"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.ElementTypes) != 1 || out.ElementTypes[0].KeyName != "story" {
		t.Errorf("element types = %+v", out.ElementTypes)
	}
}
