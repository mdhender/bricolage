// Copyright (c) 2026 Michael D Henderson.

package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mdhender/bricolage/internal/domain"
)

// The transport half of M11. What is under test here is the status code, the
// problem document, and the shape of the response: the rules themselves are
// asserted in internal/service and internal/workflow, where the rows written
// can be counted.

// reviewable creates a document, checks it in, and submits it, which is the
// state an approval is about.
//
// It creates its own rather than using createDoc, because the default
// workflow's Submit declares has_slug and a fixture without one would fail
// every test below for the wrong reason.
func (h *harness) reviewable(t *testing.T, token, title, slug string) string {
	t.Helper()
	w := h.do(t, http.MethodPost, "/api/v1/documents", token, map[string]any{
		"site": 1, "kind": "story", "element_type": "story",
		"title": title, "slug": slug, "cover_date": "2026-03-01",
		"content": `{"body":"The quick brown fox."}`,
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("POST /api/v1/documents = %d %s", w.Code, w.Body)
	}
	var doc documentResponse
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		path string
		body map[string]any
	}{
		{"/checkout", nil},
		{"/checkin", map[string]any{"note": "first cut"}},
		{"/transitions", map[string]any{"to": "review"}},
	} {
		rec := h.do(t, http.MethodPost, "/api/v1/documents/"+doc.UID+step.path, token, step.body)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST %s = %d %s", step.path, rec.Code, rec.Body)
		}
	}
	return doc.UID
}

func (h *harness) approvals(t *testing.T, method, path, token string, body any, want int) approvalsResponse {
	t.Helper()
	w := h.do(t, method, path, token, body)
	if w.Code != want {
		t.Fatalf("%s %s = %d %s, want %d", method, path, w.Code, w.Body, want)
	}
	var out approvalsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestApprovalsAreASubresource is DESIGN.md 12's shape: POST records the
// caller's sign-off and DELETE .../current takes it back. An approval is
// addressed as "mine" and never by identifier, because withdrawing somebody
// else's is not an operation this system has.
func TestApprovalsAreASubresource(t *testing.T) {
	h := newDocAPIHarness(t)
	h.user(t, "admin@example.com", domain.Publish)
	token := h.login(t, "admin@example.com")
	uid := h.reviewable(t, token, "Signed Off", "signed-off")
	base := "/api/v1/documents/" + uid + "/approvals"

	// Nobody has approved it, and the process wants one.
	before := h.approvals(t, http.MethodGet, base, token, nil, http.StatusOK)
	if before.Count != 0 || before.Required != 1 || before.Met || before.Approved {
		t.Errorf("a fresh document reports %+v, want no approvals and one required", before)
	}

	// 201 the first time: this call recorded one.
	first := h.approvals(t, http.MethodPost, base, token, nil, http.StatusCreated)
	if first.Count != 1 || !first.Met || !first.Approved {
		t.Errorf("after approving: %+v, want one approval and the guard met", first)
	}
	if len(first.Approvals) != 1 || first.Approvals[0].User == "" {
		t.Errorf("the approvals are %+v, want one naming a user by uid", first.Approvals)
	}

	// 200 the second: idempotent, and nothing was created.
	second := h.approvals(t, http.MethodPost, base, token, nil, http.StatusOK)
	if second.Count != 1 {
		t.Errorf("count = %d after approving twice, want 1", second.Count)
	}

	// The withdrawal says whether it removed anything, both times.
	gone := h.approvals(t, http.MethodDelete, base+"/current", token, nil, http.StatusOK)
	if !gone.Withdrawn || gone.Count != 0 || gone.Approved {
		t.Errorf("after withdrawing: %+v, want it removed and the count back to zero", gone)
	}
	again := h.approvals(t, http.MethodDelete, base+"/current", token, nil, http.StatusOK)
	if again.Withdrawn {
		t.Errorf("the second withdrawal reported that it removed something: %+v", again)
	}
}

// TestApprovingADraftIsAConflict is the version rule at the edge: a statement
// about the document, so a 409 rather than a 403.
func TestApprovingADraftIsAConflict(t *testing.T) {
	h := newDocAPIHarness(t)
	h.user(t, "admin@example.com", domain.Publish)
	token := h.login(t, "admin@example.com")

	w := h.do(t, http.MethodPost, "/api/v1/documents", token, map[string]any{
		"site": 1, "kind": "story", "element_type": "story",
		"title": "Still Being Written", "slug": "still-being-written",
		"content": `{"body":"The quick brown fox."}`,
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("POST /api/v1/documents = %d %s", w.Code, w.Body)
	}
	var doc documentResponse
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	w = h.do(t, http.MethodPost, "/api/v1/documents/"+doc.UID+"/transitions", token,
		map[string]any{"to": "review"})
	if w.Code != http.StatusOK {
		t.Fatalf("POST /transitions = %d %s", w.Code, w.Body)
	}

	w = h.do(t, http.MethodPost, "/api/v1/documents/"+doc.UID+"/approvals", token, nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("POST /approvals against an open draft = %d %s, want 409", w.Code, w.Body)
	}
	if got := w.Header().Get("Content-Type"); got != ContentTypeProblem {
		t.Errorf("Content-Type = %q, want %q", got, ContentTypeProblem)
	}
}

// TestApprovingNeedsTheProcessesPrivilege is a 403 rather than a 409: a
// statement about the person.
func TestApprovingNeedsTheProcessesPrivilege(t *testing.T) {
	h := newDocAPIHarness(t)
	h.user(t, "admin@example.com", domain.Publish)
	h.user(t, "writer@example.com", domain.Edit)
	admin := h.login(t, "admin@example.com")
	writer := h.login(t, "writer@example.com")

	uid := h.reviewable(t, admin, "Not Yours To Approve", "not-yours")
	w := h.do(t, http.MethodPost, "/api/v1/documents/"+uid+"/approvals", writer, nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("a writer approving = %d %s, want 403", w.Code, w.Body)
	}
}

// TestCommentsAreAThread covers the listing, the reply, and the resolution
// route, which is addressed by the comment's uid rather than the document's.
func TestCommentsAreAThread(t *testing.T) {
	h := newDocAPIHarness(t)
	h.user(t, "admin@example.com", domain.Publish)
	token := h.login(t, "admin@example.com")
	uid := h.reviewable(t, token, "Under Discussion", "under-discussion")
	base := "/api/v1/documents/" + uid + "/comments"

	w := h.do(t, http.MethodPost, base, token, map[string]any{"body": "the lede is buried"})
	if w.Code != http.StatusCreated {
		t.Fatalf("POST /comments = %d %s", w.Code, w.Body)
	}
	var root commentResponse
	if err := json.Unmarshal(w.Body.Bytes(), &root); err != nil {
		t.Fatal(err)
	}
	if root.UID == "" || root.Author == "" || root.Version != 1 {
		t.Errorf("the comment is %+v, want a uid, an author uid, and the version it was written against", root)
	}

	w = h.do(t, http.MethodPost, base, token, map[string]any{"body": "moved it up", "reply_to": root.UID})
	if w.Code != http.StatusCreated {
		t.Fatalf("POST /comments (reply) = %d %s", w.Code, w.Body)
	}

	w = h.do(t, http.MethodGet, base, token, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /comments = %d %s", w.Code, w.Body)
	}
	var list commentsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Threads) != 1 || len(list.Threads[0].Replies) != 1 {
		t.Fatalf("the listing is %+v, want one thread with one reply", list)
	}
	if list.Open != 1 {
		t.Errorf("open = %d, want 1: a reply is not a second open question", list.Open)
	}

	// Resolving a reply is a 409 naming the thread.
	reply := list.Threads[0].Replies[0]
	w = h.do(t, http.MethodPost, "/api/v1/comments/"+reply.UID+"/resolution", token, nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("resolving a reply = %d %s, want 409", w.Code, w.Body)
	}

	// Resolving the thread is a 200, and it says who closed it.
	w = h.do(t, http.MethodPost, "/api/v1/comments/"+root.UID+"/resolution", token, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /resolution = %d %s", w.Code, w.Body)
	}
	var resolved commentResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resolved); err != nil {
		t.Fatal(err)
	}
	if !resolved.Resolved || resolved.ResolvedBy == "" || resolved.ResolvedAt == nil {
		t.Errorf("the resolved comment is %+v, want it closed and named", resolved)
	}

	w = h.do(t, http.MethodGet, base, token, nil)
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if list.Open != 0 {
		t.Errorf("open = %d after resolving the thread, want 0", list.Open)
	}
}

// TestAnEmptyCommentIs422 is the one refusal the transport itself has to get
// right: a malformed body is the caller's own mistake, and its detail reaches
// them in production as well as in development.
func TestAnEmptyCommentIs422(t *testing.T) {
	h := newDocAPIHarness(t)
	h.user(t, "admin@example.com", domain.Publish)
	token := h.login(t, "admin@example.com")
	doc := h.createDoc(t, token, "Nothing To Say")

	w := h.do(t, http.MethodPost, "/api/v1/documents/"+doc.UID+"/comments", token,
		map[string]any{"body": "   "})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("POST an empty comment = %d %s, want 422", w.Code, w.Body)
	}

	// An unknown field is refused too, so that {"text": ...} is found out now
	// rather than later as an empty column.
	w = h.do(t, http.MethodPost, "/api/v1/documents/"+doc.UID+"/comments", token,
		map[string]any{"text": "wrong field"})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("POST an unknown field = %d %s, want 422", w.Code, w.Body)
	}
}

// TestCollaborationRoutesNeedASession is the same assertion every other route
// carries: none of these answers an unauthenticated caller.
func TestCollaborationRoutesNeedASession(t *testing.T) {
	h := newDocAPIHarness(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/documents/x/comments"},
		{http.MethodPost, "/api/v1/documents/x/comments"},
		{http.MethodPost, "/api/v1/comments/x/resolution"},
		{http.MethodGet, "/api/v1/documents/x/approvals"},
		{http.MethodPost, "/api/v1/documents/x/approvals"},
		{http.MethodDelete, "/api/v1/documents/x/approvals/current"},
	} {
		w := h.do(t, tc.method, tc.path, "", nil)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with no token = %d, want 401", tc.method, tc.path, w.Code)
		}
	}
}
