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

// The transport half of M9 and M10. What is under test here is the status code
// and the response shape; the publish itself is asserted in internal/service,
// the traversal in internal/publish, and the SQL in internal/store.

// newPublishAPIHarness is the preview harness with an output tree behind it,
// so that POST .../publications has a publisher to reach. Its related-asset
// policy is the default, which is "fail".
func newPublishAPIHarness(t *testing.T) *harness {
	t.Helper()
	return newPublishAPIHarnessWith(t, config.DefaultRelatedFailure)
}

// newPublishAPIHarnessWith is the same server with a chosen cascade policy.
// It is a parameter rather than a setter because config.RelatedFailure is
// resolved once, when the service is built: a policy something could change
// between two requests would be a policy no log line could name.
func newPublishAPIHarnessWith(t *testing.T, policy config.RelatedFailure) *harness {
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
	svc, err := service.New(db, service.Options{
		Clock: c, Renderer: engine, Publisher: publisher, RelatedFailure: policy,
	})
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
// filed at the root and left in draft. The uids it is given become the
// content's "related" list, which is what M10's cascade reads.
func (h *harness) filedStory(t *testing.T, token, title, slug string, refs ...string) string {
	t.Helper()
	payload := map[string]any{"body": "Words."}
	if len(refs) > 0 {
		payload["related"] = refs
	}
	content, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encoding the content: %v", err)
	}
	rec := h.do(t, http.MethodPost, "/api/v1/documents", token, map[string]any{
		"site": 1, "kind": "story", "element_type": "story",
		"title": title, "slug": slug, "cover_date": "2026-03-01",
		"content": string(content),
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

// approvePublishable walks a filed story to a publishable state, which is what
// a document has to be before the cascade will gather it.
func (h *harness) approvePublishable(t *testing.T, token, uid string) {
	t.Helper()
	for _, step := range []struct {
		path string
		body map[string]any
	}{
		{"/checkout", nil},
		{"/checkin", map[string]any{"note": "ready"}},
		{"/transitions", map[string]any{"to": "review"}},
		{"/transitions", map[string]any{"to": "approved"}},
	} {
		rec := h.do(t, http.MethodPost, "/api/v1/documents/"+uid+step.path, token, step.body)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST %s = %d %s", step.path, rec.Code, rec.Body.String())
		}
	}
}

// TestPublicationsCarryTheCascade is M10's transport shape: what the cascade
// gathered, and the status a dry run answers with.
func TestPublicationsCarryTheCascade(t *testing.T) {
	h := newPublishAPIHarness(t)
	h.user(t, "admin@example.com", domain.Publish)
	token := h.login(t, "admin@example.com")

	rel := h.filedStory(t, token, "The Sidebar", "the-sidebar")
	h.approvePublishable(t, token, rel)
	root := h.filedStory(t, token, "The Feature", "the-feature", rel)
	h.approvePublishable(t, token, root)

	t.Run("a dry run is 200 and schedules nothing", func(t *testing.T) {
		rec := h.do(t, http.MethodPost, "/api/v1/documents/"+root+"/publications", token,
			map[string]any{"dry_run": true})
		// 200 and not 202: nothing at all was accepted for processing, and
		// the answer is a report that is complete when it is read.
		if rec.Code != http.StatusOK {
			t.Fatalf("POST /publications dry_run = %d %s, want 200", rec.Code, rec.Body.String())
		}
		var out publicationResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if !out.DryRun {
			t.Error("the response does not say it was a dry run")
		}
		if out.Job != "" {
			t.Errorf("job = %q; a dry run schedules none", out.Job)
		}
		if out.ScheduledFor.IsZero() {
			t.Error("scheduled_for is the zero time; a dry run still says when it would happen")
		}
		if len(out.Related) != 1 || out.Related[0].UID != rel {
			t.Fatalf("related = %+v, want the sidebar", out.Related)
		}
		if out.Related[0].Title != "The Sidebar" || out.Related[0].Version != 1 {
			t.Errorf("related = %+v, want its title and pinned version", out.Related[0])
		}
		if out.Related[0].Job != "" {
			t.Errorf("related job = %q; a dry run schedules none", out.Related[0].Job)
		}
	})

	t.Run("a real publish is 202 and names a job for each", func(t *testing.T) {
		rec := h.do(t, http.MethodPost, "/api/v1/documents/"+root+"/publications", token, nil)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("POST /publications = %d %s, want 202", rec.Code, rec.Body.String())
		}
		var out publicationResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out.DryRun {
			t.Error("a real publish reported itself as a dry run")
		}
		if out.Job == "" {
			t.Error("the root was scheduled with no job")
		}
		if len(out.Related) != 1 || out.Related[0].Job == "" {
			t.Fatalf("related = %+v, want the sidebar with a job of its own", out.Related)
		}
	})
}

// TestPublicationsRefusedCascadeIsAProblemDocument is PLAN.md M10
// acceptance 2 at the transport edge: a 409 whose problem document names every
// document that held the publish up.
//
// The extension member is the point. A client drawing an editor a list of what
// is blocking a publish must not have to parse it out of "detail".
func TestPublicationsRefusedCascadeIsAProblemDocument(t *testing.T) {
	h := newPublishAPIHarness(t)
	h.user(t, "admin@example.com", domain.Publish)
	token := h.login(t, "admin@example.com")

	// Left in draft, which the default workflow does not call publishable.
	blocked := h.filedStory(t, token, "Unfinished", "unfinished")
	root := h.filedStory(t, token, "The Feature", "the-feature", blocked)
	h.approvePublishable(t, token, root)

	rec := h.do(t, http.MethodPost, "/api/v1/documents/"+root+"/publications", token, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("POST /publications = %d %s, want 409", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != ContentTypeProblem {
		t.Errorf("Content-Type = %q, want %q", got, ContentTypeProblem)
	}

	var p Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Refusals) != 1 {
		t.Fatalf("refusals = %+v, want the unfinished story alone", p.Refusals)
	}
	if p.Refusals[0].UID != blocked {
		t.Errorf("the refusal names %q, want %s", p.Refusals[0].UID, blocked)
	}
	if p.Refusals[0].Reason != string(domain.RefusedState) {
		t.Errorf("reason = %q, want %q", p.Refusals[0].Reason, domain.RefusedState)
	}
	if p.Refusals[0].ReferencedBy != root {
		t.Errorf("referenced_by = %q, want %s", p.Refusals[0].ReferencedBy, root)
	}
	// And the sentence names it too, for a client with no extension-member
	// support and for a person reading a log.
	if !strings.Contains(p.Detail, blocked) {
		t.Errorf("detail = %q, want it to name %s", p.Detail, blocked)
	}
}

// TestPublishUnderWarnStillReportsTheRefusal keeps the two settings distinct at
// the transport edge: under "warn" the root publishes and the refusal is
// reported beside it, in the same 202.
func TestPublishUnderWarnStillReportsTheRefusal(t *testing.T) {
	h := newPublishAPIHarnessWith(t, config.RelatedFailureWarn)
	h.user(t, "admin@example.com", domain.Publish)
	token := h.login(t, "admin@example.com")

	blocked := h.filedStory(t, token, "Unfinished", "unfinished")
	root := h.filedStory(t, token, "The Feature", "the-feature", blocked)
	h.approvePublishable(t, token, root)

	rec := h.do(t, http.MethodPost, "/api/v1/documents/"+root+"/publications", token, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST /publications = %d %s, want 202", rec.Code, rec.Body.String())
	}
	var out publicationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Job == "" {
		t.Error("under warn the root publishes; it was scheduled with no job")
	}
	if len(out.Refusals) != 1 || out.Refusals[0].UID != blocked {
		t.Fatalf("refusals = %+v, want the unfinished story named", out.Refusals)
	}
	if out.WouldRefuse {
		t.Error("would_refuse is set on a publish that went ahead")
	}
}
