// Copyright (c) 2026 Michael D Henderson.

package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mdhender/bricolage/internal/domain"
)

// The transport half of M7. What is under test here is the status code and the
// response shape: the refusals themselves are asserted in internal/service,
// where the rows written can be counted.

// TestCategoryRoutes walks the collection: create, list, show, move, delete.
func TestCategoryRoutes(t *testing.T) {
	h := newDocAPIHarness(t)
	h.user(t, "admin@example.com", domain.Publish)
	token := h.login(t, "admin@example.com")

	create := func(t *testing.T, parent, directory string) categoryResponse {
		t.Helper()
		rec := h.do(t, http.MethodPost, "/api/v1/categories", token, map[string]any{
			"site": int64(1), "parent": parent, "directory": directory, "name": directory,
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("POST /categories = %d %s", rec.Code, rec.Body.String())
		}
		var out categoryResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decoding: %v", err)
		}
		return out
	}

	features := create(t, "/", "features")
	if features.Path != "/features/" {
		t.Errorf("path = %q, want /features/", features.Path)
	}
	if features.Parent != "/" || features.Depth != 1 {
		t.Errorf("the response is %+v; want parent / and depth 1", features)
	}
	film := create(t, "/features/", "film")
	reviews := create(t, "/features/film/", "reviews")

	t.Run("list is in path order and carries the root", func(t *testing.T) {
		rec := h.do(t, http.MethodGet, "/api/v1/categories?site=1", token, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /categories = %d %s", rec.Code, rec.Body.String())
		}
		var out struct {
			Categories []categoryResponse `json:"categories"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		want := []string{"/", "/features/", "/features/film/", "/features/film/reviews/"}
		if len(out.Categories) != len(want) {
			t.Fatalf("%d categories, want %d: %+v", len(out.Categories), len(want), out.Categories)
		}
		for i, c := range out.Categories {
			if c.Path != want[i] {
				t.Errorf("categories[%d] = %q, want %q", i, c.Path, want[i])
			}
		}
	})

	t.Run("a listing with no site is refused", func(t *testing.T) {
		rec := h.do(t, http.MethodGet, "/api/v1/categories", token, nil)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("GET /categories with no site = %d, want 422", rec.Code)
		}
	})

	t.Run("show", func(t *testing.T) {
		rec := h.do(t, http.MethodGet, "/api/v1/categories/"+film.UID, token, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /categories/{uid} = %d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("move rewrites the subtree", func(t *testing.T) {
		rec := h.do(t, http.MethodPatch, "/api/v1/categories/"+film.UID, token,
			map[string]any{"parent": "/"})
		if rec.Code != http.StatusOK {
			t.Fatalf("PATCH /categories/{uid} = %d %s", rec.Code, rec.Body.String())
		}
		var out categoryResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out.Path != "/film/" {
			t.Errorf("the moved category is at %q, want /film/", out.Path)
		}

		rec = h.do(t, http.MethodGet, "/api/v1/categories/"+reviews.UID, token, nil)
		var child categoryResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &child); err != nil {
			t.Fatal(err)
		}
		if child.Path != "/film/reviews/" {
			t.Errorf("the descendant is at %q, want /film/reviews/", child.Path)
		}
	})

	t.Run("a move that would orphan the tree is a 409", func(t *testing.T) {
		rec := h.do(t, http.MethodPatch, "/api/v1/categories/"+film.UID, token,
			map[string]any{"parent": "/film/reviews/"})
		if rec.Code != http.StatusConflict {
			t.Errorf("moving a category inside itself = %d, want 409: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("delete refuses a category with children", func(t *testing.T) {
		rec := h.do(t, http.MethodDelete, "/api/v1/categories/"+film.UID, token, nil)
		if rec.Code != http.StatusConflict {
			t.Errorf("deleting a category with children = %d, want 409", rec.Code)
		}
		rec = h.do(t, http.MethodDelete, "/api/v1/categories/"+reviews.UID, token, nil)
		if rec.Code != http.StatusNoContent {
			t.Errorf("deleting an empty leaf = %d, want 204: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("an unknown uid is a 404", func(t *testing.T) {
		rec := h.do(t, http.MethodGet, "/api/v1/categories/nope", token, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET an unknown category = %d, want 404", rec.Code)
		}
	})
}

// TestCheckinNamesTheOffendingFields is PLAN.md M7 acceptance 5 at the
// transport edge: a 422 whose problem document carries the field errors.
func TestCheckinNamesTheOffendingFields(t *testing.T) {
	h := newDocAPIHarness(t)
	h.user(t, "editor@example.com", domain.Create)
	token := h.login(t, "editor@example.com")

	rec := h.do(t, http.MethodPost, "/api/v1/documents", token, map[string]any{
		"site": 1, "kind": "story", "element_type": "story",
		"title": "A Story", "content": `{"boyd":"typo","deck":42}`,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("a working draft may be invalid, so creating one must succeed: %d %s",
			rec.Code, rec.Body.String())
	}
	var doc documentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}

	rec = h.do(t, http.MethodPost, "/api/v1/documents/"+doc.UID+"/checkin", token, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("checking in content that violates the schema = %d, want 422: %s",
			rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != ContentTypeProblem {
		t.Errorf("Content-Type = %q, want %q", got, ContentTypeProblem)
	}

	var p Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Errors) != 2 {
		t.Fatalf("the problem document carries %d field errors, want 2: %+v", len(p.Errors), p)
	}
	named := map[string]string{}
	for _, e := range p.Errors {
		named[e.Field] = e.Message
	}
	for _, field := range []string{"deck", "boyd"} {
		if named[field] == "" {
			t.Errorf("the refusal does not name %q: %+v", p.Errors, p)
		}
	}
}

// TestDocumentCategoriesAndURIs covers the two document subresources M7 adds.
func TestDocumentCategoriesAndURIs(t *testing.T) {
	h := newDocAPIHarness(t)
	h.user(t, "admin@example.com", domain.Publish)
	token := h.login(t, "admin@example.com")

	rec := h.do(t, http.MethodPost, "/api/v1/categories", token, map[string]any{
		"site": 1, "parent": "/", "directory": "features", "name": "Features",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /categories = %d %s", rec.Code, rec.Body.String())
	}
	rec = h.do(t, http.MethodPost, "/api/v1/output-channels", token, map[string]any{
		"site": 1, "name": "Web", "use_slug": true,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /output-channels = %d %s", rec.Code, rec.Body.String())
	}

	rec = h.do(t, http.MethodPost, "/api/v1/documents", token, map[string]any{
		"site": 1, "kind": "story", "element_type": "story",
		"title": "A Story", "slug": "a-story", "cover_date": "2026-03-01",
		"content": `{"body":"Words."}`,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /documents = %d %s", rec.Code, rec.Body.String())
	}
	var doc documentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Category != "" {
		t.Errorf("a new document reports category %q, want none", doc.Category)
	}

	t.Run("an unfiled document has no address", func(t *testing.T) {
		rec := h.do(t, http.MethodGet, "/api/v1/documents/"+doc.UID+"/uris", token, nil)
		if rec.Code != http.StatusConflict {
			t.Errorf("GET /uris on an unfiled document = %d, want 409: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("filing it", func(t *testing.T) {
		rec := h.do(t, http.MethodPut, "/api/v1/documents/"+doc.UID+"/categories", token,
			map[string]any{"categories": []string{"/features/", "/"}})
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT /categories = %d %s", rec.Code, rec.Body.String())
		}
		var out struct {
			Categories []filingResponse `json:"categories"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if len(out.Categories) != 2 || !out.Categories[0].Primary || out.Categories[0].Category != "/features/" {
			t.Fatalf("the filing is %+v; the first path given is the primary one", out.Categories)
		}

		// And the document now reports it, which is what a grant matches on.
		rec = h.do(t, http.MethodGet, "/api/v1/documents/"+doc.UID, token, nil)
		var got documentResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Category != "/features/" {
			t.Errorf("the document reports category %q, want /features/", got.Category)
		}
	})

	t.Run("the addresses that follow", func(t *testing.T) {
		rec := h.do(t, http.MethodGet, "/api/v1/documents/"+doc.UID+"/uris", token, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /uris = %d %s", rec.Code, rec.Body.String())
		}
		var out struct {
			UID  string        `json:"uid"`
			URIs []uriResponse `json:"uris"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if len(out.URIs) != 1 {
			t.Fatalf("%d addresses, want 1: %+v", len(out.URIs), out.URIs)
		}
		if want := "/features/2026/03/01/a-story"; out.URIs[0].URI != want {
			t.Errorf("uri = %q, want %q", out.URIs[0].URI, want)
		}
		if out.URIs[0].URL == "" || out.URIs[0].File == "" {
			t.Errorf("the response omits the URL or the file: %+v", out.URIs[0])
		}
	})
}

// TestElementTypeRoutes covers the administration surface M7 adds, including
// what may not change.
func TestElementTypeRoutes(t *testing.T) {
	h := newDocAPIHarness(t)
	h.user(t, "admin@example.com", domain.Publish)
	token := h.login(t, "admin@example.com")

	rec := h.do(t, http.MethodPost, "/api/v1/element-types", token, map[string]any{
		"key_name": "page", "kind": "story", "fixed_uri": true,
		"schema": `{"fields":[{"name":"body","type":"block"}]}`,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /element-types = %d %s", rec.Code, rec.Body.String())
	}
	var et elementTypeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &et); err != nil {
		t.Fatal(err)
	}
	if !et.FixedURI {
		t.Error("fixed_uri did not survive the round trip")
	}

	t.Run("show", func(t *testing.T) {
		rec := h.do(t, http.MethodGet, "/api/v1/element-types/page", token, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("GET /element-types/page = %d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("a schema nobody can implement is a 422", func(t *testing.T) {
		rec := h.do(t, http.MethodPost, "/api/v1/element-types", token, map[string]any{
			"key_name": "broken", "kind": "story",
			"schema": `{"fields":[{"name":"body","type":"markdown"}]}`,
		})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("a bad schema = %d, want 422: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("the key name and the kind are fixed at creation", func(t *testing.T) {
		for _, body := range []map[string]any{
			{"key_name": "renamed"},
			{"kind": "media"},
		} {
			rec := h.do(t, http.MethodPatch, "/api/v1/element-types/page", token, body)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Errorf("PATCH %v = %d, want 422", body, rec.Code)
			}
		}
	})

	t.Run("an update leaves what it does not mention alone", func(t *testing.T) {
		rec := h.do(t, http.MethodPatch, "/api/v1/element-types/page", token,
			map[string]any{"name": "Static page"})
		if rec.Code != http.StatusOK {
			t.Fatalf("PATCH /element-types/page = %d %s", rec.Code, rec.Body.String())
		}
		var got elementTypeResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Name != "Static page" || !got.FixedURI {
			t.Errorf("the update is %+v; it changed something it did not mention", got)
		}
	})
}

// TestGrantSpeaksCategoryPaths is the shape change M7 makes to an existing
// route: a grant names a category by path, and the server resolves it.
//
// Taking both the id and the path from the client is what lets them disagree,
// and a grant whose path names a different row from its id matches the wrong
// documents with nothing to notice.
func TestGrantSpeaksCategoryPaths(t *testing.T) {
	h := newDocAPIHarness(t)
	h.user(t, "admin@example.com", domain.Publish)
	token := h.login(t, "admin@example.com")

	rec := h.do(t, http.MethodPost, "/api/v1/categories", token, map[string]any{
		"site": 1, "parent": "/", "directory": "features", "name": "Features",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /categories = %d %s", rec.Code, rec.Body.String())
	}
	if _, err := h.db.CreateRole(t.Context(), "features-editor", "Features editor"); err != nil {
		t.Fatal(err)
	}

	rec = h.do(t, http.MethodPost, "/api/v1/grants", token, map[string]any{
		"role": "features-editor", "privilege": "edit",
		"site": 1, "category": "/features/",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /grants = %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Description string `json:"description"`
		Scope       struct {
			Category     *string `json:"category"`
			CategoryDeep bool    `json:"category_deep"`
		} `json:"scope"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Scope.Category == nil || *out.Scope.Category != "/features/" {
		t.Fatalf("the grant's scope is %+v, want the category path", out.Scope)
	}
	if !out.Scope.CategoryDeep {
		t.Error("category_deep defaults to false; a grant on a section should cover it")
	}

	t.Run("a category with no site is a 422", func(t *testing.T) {
		rec := h.do(t, http.MethodPost, "/api/v1/grants", token, map[string]any{
			"role": "features-editor", "privilege": "edit", "category": "/features/",
		})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("a category grant with no site = %d, want 422: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("a category that does not exist is a 404", func(t *testing.T) {
		rec := h.do(t, http.MethodPost, "/api/v1/grants", token, map[string]any{
			"role": "features-editor", "privilege": "edit", "site": 1, "category": "/nowhere/",
		})
		if rec.Code != http.StatusNotFound {
			t.Errorf("a grant on a category that does not exist = %d, want 404", rec.Code)
		}
	})
}

// TestSitesRoute is how a client learns the id every other route wants.
func TestSitesRoute(t *testing.T) {
	h := newDocAPIHarness(t)
	h.user(t, "reader@example.com", domain.Read)
	token := h.login(t, "reader@example.com")

	rec := h.do(t, http.MethodGet, "/api/v1/sites", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /sites = %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Sites []siteResponse `json:"sites"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Sites) != 1 || out.Sites[0].Domain != "example.com" {
		t.Errorf("sites = %+v, want the one the harness created", out.Sites)
	}
}
