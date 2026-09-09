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
	"github.com/mdhender/bricolage/internal/publish"
	"github.com/mdhender/bricolage/internal/render"
	"github.com/mdhender/bricolage/internal/service"
	"github.com/mdhender/bricolage/internal/store"
)

// The transport half of M9. What is under test here is the status code and the
// response shape; the publish itself is asserted in internal/service and the
// SQL in internal/store.

// newPublishAPIHarness is the preview harness with an output tree behind it,
// so that POST .../publications has a publisher to reach.
func newPublishAPIHarness(t *testing.T) *harness {
	t.Helper()

	db, err := store.OpenMemory(t.Context())
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	engine, err := render.New(render.Options{FS: fstest.MapFS{
		"example.com/story.gohtml": &fstest.MapFile{Data: []byte(`<h1>{{.Version.Title}}</h1>`)},
	}, Reload: true})
	if err != nil {
		t.Fatalf("render.New: %v", err)
	}
	// t.TempDir already exists; publish.NewTree never creates its root
	// (invariant 19).
	tree, err := publish.NewTree(t.TempDir())
	if err != nil {
		t.Fatalf("publish.NewTree: %v", err)
	}
	t.Cleanup(func() { _ = tree.Close() })

	c := clock.NewFake(start)
	publisher, err := publish.New(publish.Options{DB: db, Renderer: engine, Tree: tree, Clock: c})
	if err != nil {
		t.Fatalf("publish.New: %v", err)
	}
	svc, err := service.New(db, service.Options{Clock: c, Renderer: engine, Publisher: publisher})
	if err != nil {
		t.Fatalf("service.New: %v", err)
	}

	mux := http.NewServeMux()
	Register(mux, Deps{Service: svc, Environment: config.Production})

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

// publishable creates a channel and a filed, checked-in, approved story, which
// is the shape a publish needs.
func (h *harness) publishable(t *testing.T, token string) string {
	t.Helper()
	uid := h.previewable(t, token)

	for _, step := range []struct {
		method, path string
		body         map[string]any
		want         int
	}{
		{http.MethodPost, "/api/v1/documents/" + uid + "/checkout", nil, http.StatusOK},
		{http.MethodPost, "/api/v1/documents/" + uid + "/checkin", map[string]any{"note": "ready"}, http.StatusOK},
		{http.MethodPost, "/api/v1/documents/" + uid + "/transitions", map[string]any{"to": "review"}, http.StatusOK},
		{http.MethodPost, "/api/v1/documents/" + uid + "/transitions", map[string]any{"to": "approved"}, http.StatusOK},
	} {
		rec := h.do(t, step.method, step.path, token, step.body)
		if rec.Code != step.want {
			t.Fatalf("%s %s = %d %s", step.method, step.path, rec.Code, rec.Body.String())
		}
	}
	return uid
}

// filedStory creates one more story on the channel previewable already made,
// filed at the root and left in draft.
func (h *harness) filedStory(t *testing.T, token, title, slug string) string {
	t.Helper()
	rec := h.do(t, http.MethodPost, "/api/v1/documents", token, map[string]any{
		"site": 1, "kind": "story", "element_type": "story",
		"title": title, "slug": slug, "cover_date": "2026-03-01",
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

// TestPublicationsAndResources is the round trip: schedule, and then ask what
// the document's addresses hold.
func TestPublicationsAndResources(t *testing.T) {
	h := newPublishAPIHarness(t)
	h.user(t, "admin@example.com", domain.Publish)
	token := h.login(t, "admin@example.com")
	uid := h.publishable(t, token)

	rec := h.do(t, http.MethodPost, "/api/v1/documents/"+uid+"/publications", token, nil)
	// 202, not 201: nothing has been published, a job has been scheduled.
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST /publications = %d %s", rec.Code, rec.Body.String())
	}
	var out publicationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.UID != uid || out.Job == "" {
		t.Errorf("response = %+v, want the document and the job it scheduled", out)
	}
	if out.Version != 1 {
		t.Errorf("version = %d, want the checked-in version 1; the pin is the promise", out.Version)
	}
	if !out.ScheduledFor.Equal(h.clock.Now()) {
		t.Errorf("scheduled_for = %v, want now", out.ScheduledFor)
	}

	t.Run("and --at schedules it", func(t *testing.T) {
		rec := h.do(t, http.MethodPost, "/api/v1/documents/"+uid+"/publications", token,
			map[string]any{"at": "6h"})
		if rec.Code != http.StatusAccepted {
			t.Fatalf("POST /publications = %d %s", rec.Code, rec.Body.String())
		}
		var later publicationResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &later); err != nil {
			t.Fatal(err)
		}
		if !later.ScheduledFor.After(out.ScheduledFor) {
			t.Errorf("scheduled_for = %v, want it after %v", later.ScheduledFor, out.ScheduledFor)
		}
	})

	t.Run("and a schedule nobody can read is a 422", func(t *testing.T) {
		rec := h.do(t, http.MethodPost, "/api/v1/documents/"+uid+"/publications", token,
			map[string]any{"at": "next tuesday"})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("POST /publications = %d %s, want 422", rec.Code, rec.Body.String())
		}
	})

	t.Run("and resources are empty until a job runs", func(t *testing.T) {
		rec := h.do(t, http.MethodGet, "/api/v1/documents/"+uid+"/resources", token, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /resources = %d %s", rec.Code, rec.Body.String())
		}
		var got struct {
			UID       string             `json:"uid"`
			Resources []resourceResponse `json:"resources"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.UID != uid {
			t.Errorf("uid = %q, want %q", got.UID, uid)
		}
		if len(got.Resources) != 0 {
			t.Errorf("resources = %+v before any job ran", got.Resources)
		}
		// An empty list is [] and not null, because a client that has to
		// handle both is a client that handles one of them wrongly.
		if !strings.Contains(rec.Body.String(), `"resources":[]`) {
			t.Errorf("body = %s, want an empty array", rec.Body.String())
		}
	})
}

// TestPublicationsRefusals covers the three statuses of DESIGN.md 12's table
// that this route can produce.
func TestPublicationsRefusals(t *testing.T) {
	h := newPublishAPIHarness(t)
	h.user(t, "admin@example.com", domain.Publish)
	token := h.login(t, "admin@example.com")
	h.user(t, "writer@example.com", domain.Edit)
	writer := h.login(t, "writer@example.com")
	uid := h.publishable(t, token)

	t.Run("unauthenticated is 401", func(t *testing.T) {
		rec := h.do(t, http.MethodPost, "/api/v1/documents/"+uid+"/publications", "", nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("= %d, want 401", rec.Code)
		}
	})

	t.Run("privilege insufficient is 403", func(t *testing.T) {
		rec := h.do(t, http.MethodPost, "/api/v1/documents/"+uid+"/publications", writer, nil)
		if rec.Code != http.StatusForbidden {
			t.Errorf("= %d %s, want 403", rec.Code, rec.Body.String())
		}
	})

	t.Run("an unknown uid is 404", func(t *testing.T) {
		rec := h.do(t, http.MethodPost, "/api/v1/documents/nope/publications", token, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("= %d, want 404", rec.Code)
		}
	})

	t.Run("a state the workflow does not call publishable is 409", func(t *testing.T) {
		draft := h.filedStory(t, token, "Unfinished", "unfinished")
		rec := h.do(t, http.MethodPost, "/api/v1/documents/"+draft+"/publications", token, nil)
		if rec.Code != http.StatusConflict {
			t.Errorf("= %d %s, want 409", rec.Code, rec.Body.String())
		}
	})
}

// TestPublishIsUnavailableWithoutAnOutputTree is the 503 row of DESIGN.md 12's
// table, whose detail reaches the client in production because it names the
// flag rather than an internal failure.
func TestPublishIsUnavailableWithoutAnOutputTree(t *testing.T) {
	h := newPreviewHarness(t, config.Production, map[string]string{
		"example.com/story.gohtml": `<h1>{{.Version.Title}}</h1>`,
	})
	h.user(t, "admin@example.com", domain.Publish)
	token := h.login(t, "admin@example.com")
	uid := h.previewable(t, token)

	rec := h.do(t, http.MethodPost, "/api/v1/documents/"+uid+"/publications", token, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST /publications = %d %s, want 503", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "--output") {
		t.Errorf("body = %s, want it to name the flag that was not given", rec.Body.String())
	}
}
