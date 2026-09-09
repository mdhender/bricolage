// Copyright (c) 2026 Michael D Henderson.

package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mdhender/bricolage/internal/domain"
)

// The transport half of M4. What is under test here is the status code, the
// problem document, and the shape of the menu: the rules themselves are
// asserted in internal/workflow, where the rows written can be counted.

// transitions reads the menu for a document.
func (h *harness) transitions(t *testing.T, token, uid string) transitionsResponse {
	t.Helper()
	w := h.do(t, http.MethodGet, "/api/v1/documents/"+uid+"/transitions", token, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET transitions = %d %s", w.Code, w.Body)
	}
	var out transitionsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func findTransition(t *testing.T, menu transitionsResponse, to string) transitionResponse {
	t.Helper()
	for _, tr := range menu.Transitions {
		if tr.To == to {
			return tr
		}
	}
	t.Fatalf("no transition to %q: %+v", to, menu)
	return transitionResponse{}
}

// TestTransitionsAreASubresource is DESIGN.md 12's shape: GET says what the
// state machine permits and why, POST performs one.
func TestTransitionsAreASubresource(t *testing.T) {
	h := newDocAPIHarness(t)
	h.user(t, "admin@example.com", domain.Publish)
	token := h.login(t, "admin@example.com")

	doc := h.createDoc(t, token, "The Quick Brown Fox")
	if doc.State != "draft" {
		t.Fatalf("a new document is in state %q, want draft", doc.State)
	}
	if doc.Workflow == "" {
		t.Error("the document response names no workflow; the API speaks uids, and this is one")
	}

	menu := h.transitions(t, token, doc.UID)
	if menu.State != "draft" {
		t.Errorf("the menu reports state %q, want draft", menu.State)
	}

	// The refused entry is in the list with a reason and the guard that
	// refused, which is what lets a UI grey it out and say why.
	submit := findTransition(t, menu, "review")
	if submit.Permitted {
		t.Error("submit is permitted on a document with no slug")
	}
	if submit.Guard != string(domain.GuardHasSlug) {
		t.Errorf("the refusal names guard %q, want has_slug", submit.Guard)
	}
	if submit.Reason == "" {
		t.Error("the refusal carries no reason")
	}
	if submit.Privilege != "edit" {
		t.Errorf("submit needs privilege %q, want the name rather than a number", submit.Privilege)
	}
	if len(submit.Guards) == 0 {
		t.Error("the entry does not list the guards the transition declares")
	}

	// Give it a slug, and the same menu permits the move.
	h.checkoutEditCheckin(t, token, doc.UID, "quick-brown-fox")

	menu = h.transitions(t, token, doc.UID)
	if submit = findTransition(t, menu, "review"); !submit.Permitted {
		t.Fatalf("submit is still refused: %s", submit.Reason)
	}

	w := h.do(t, http.MethodPost, "/api/v1/documents/"+doc.UID+"/transitions", token,
		map[string]any{"to": "review"})
	if w.Code != http.StatusOK {
		t.Fatalf("POST transitions = %d %s", w.Code, w.Body)
	}
	var moved documentResponse
	if err := json.Unmarshal(w.Body.Bytes(), &moved); err != nil {
		t.Fatal(err)
	}
	if moved.State != "review" {
		t.Errorf("state = %q, want review", moved.State)
	}
}

// TestUndeclaredTransitionIs409ForAnAdministrator is PLAN.md M4 acceptance 2 at
// the transport edge.
func TestUndeclaredTransitionIs409ForAnAdministrator(t *testing.T) {
	h := newDocAPIHarness(t)
	h.user(t, "admin@example.com", domain.Publish)
	token := h.login(t, "admin@example.com")
	doc := h.createDoc(t, token, "Straight To Press")

	w := h.do(t, http.MethodPost, "/api/v1/documents/"+doc.UID+"/transitions", token,
		map[string]any{"to": "published"})
	if w.Code != http.StatusConflict {
		t.Fatalf("draft -> published for an administrator = %d %s, want 409", w.Code, w.Body)
	}

	var p Problem
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Guard != "" {
		t.Errorf("the problem names guard %q; no guard was reached, because the move is not in the machine", p.Guard)
	}
	if p.Type != problemBase+"conflict" {
		t.Errorf("problem type = %q, want a conflict rather than a guard refusal", p.Type)
	}
}

// TestGuardRefusalNamesTheGuardInTheProblem is DESIGN.md 12's worked example:
// a 409 carrying a "guard" extension member so that a client can act on the
// specific refusal without parsing prose out of "detail".
func TestGuardRefusalNamesTheGuardInTheProblem(t *testing.T) {
	h := newDocAPIHarness(t)
	h.user(t, "admin@example.com", domain.Publish)
	token := h.login(t, "admin@example.com")
	doc := h.createDoc(t, token, "Unslugged")

	w := h.do(t, http.MethodPost, "/api/v1/documents/"+doc.UID+"/transitions", token,
		map[string]any{"to": "review"})
	if w.Code != http.StatusConflict {
		t.Fatalf("submit without a slug = %d %s, want 409", w.Code, w.Body)
	}
	if got := w.Header().Get("Content-Type"); got != ContentTypeProblem {
		t.Errorf("Content-Type = %q, want %q", got, ContentTypeProblem)
	}

	var p Problem
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Guard != string(domain.GuardHasSlug) {
		t.Errorf("the problem's guard is %q, want has_slug", p.Guard)
	}
	if p.Type != problemBase+"guard-failed" {
		t.Errorf("problem type = %q, want the guard-failed type", p.Type)
	}
	if p.Title != "Transition refused" {
		t.Errorf("problem title = %q", p.Title)
	}
}

// TestTransitionRefusedByPrivilegeIs403 keeps the two refusals apart. A guard
// is a statement about the document and a 409; a privilege is a statement about
// the person and a 403, and rendering them the same way sends an editor to an
// administrator for something no administrator can fix.
func TestTransitionRefusedByPrivilegeIs403(t *testing.T) {
	h := newDocAPIHarness(t)
	h.user(t, "admin@example.com", domain.Publish)
	h.user(t, "writer@example.com", domain.Edit)
	admin := h.login(t, "admin@example.com")
	writer := h.login(t, "writer@example.com")

	doc := h.createDoc(t, admin, "Not Yours To Archive")

	w := h.do(t, http.MethodPost, "/api/v1/documents/"+doc.UID+"/transitions", writer,
		map[string]any{"to": "archived"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("archive by a writer = %d %s, want 403", w.Code, w.Body)
	}
	var p Problem
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Guard != "" {
		t.Errorf("a privilege refusal names guard %q", p.Guard)
	}
}

// TestTransitionsOnADocumentTheCallerMayNotSeeIs404 keeps the API from
// confirming that a document exists.
func TestTransitionsOnADocumentTheCallerMayNotSeeIs404(t *testing.T) {
	h := newDocAPIHarness(t)
	h.user(t, "admin@example.com", domain.Publish)
	h.user(t, "nobody@example.com", domain.NoPrivilege)
	admin := h.login(t, "admin@example.com")
	nobody := h.login(t, "nobody@example.com")

	doc := h.createDoc(t, admin, "Private")

	if w := h.do(t, http.MethodGet, "/api/v1/documents/"+doc.UID+"/transitions", nobody, nil); w.Code != http.StatusNotFound {
		t.Errorf("GET transitions = %d %s, want 404", w.Code, w.Body)
	}
	w := h.do(t, http.MethodPost, "/api/v1/documents/"+doc.UID+"/transitions", nobody,
		map[string]any{"to": "review"})
	if w.Code != http.StatusNotFound {
		t.Errorf("POST transitions = %d %s, want 404", w.Code, w.Body)
	}
}

// TestNoRouteSetsAStateDirectly is DESIGN.md 12's rule: never expose a plain
// PATCH that sets state. The menu is the only way in, so that the workflow is
// visible in the API rather than hidden behind an assignment.
func TestNoRouteSetsAStateDirectly(t *testing.T) {
	h := newDocAPIHarness(t)
	h.user(t, "admin@example.com", domain.Publish)
	token := h.login(t, "admin@example.com")
	doc := h.createDoc(t, token, "Not Assignable")

	// PATCH refuses unknown fields, so "state" in the body is a 422 rather
	// than a silently ignored key.
	w := h.do(t, http.MethodPatch, "/api/v1/documents/"+doc.UID, token,
		map[string]any{"state": "published"})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("PATCH with a state = %d %s, want 422", w.Code, w.Body)
	}
	if got := h.transitions(t, token, doc.UID); got.State != "draft" {
		t.Errorf("state = %q after a PATCH that named one, want draft", got.State)
	}
}

// TestListWorkflowsRendersTheProcess is the read surface a client needs to
// render a state machine it did not configure.
func TestListWorkflowsRendersTheProcess(t *testing.T) {
	h := newDocAPIHarness(t)
	h.user(t, "viewer@example.com", domain.Read)
	token := h.login(t, "viewer@example.com")

	w := h.do(t, http.MethodGet, "/api/v1/workflows", token, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/workflows = %d %s", w.Code, w.Body)
	}
	var out struct {
		Workflows []workflowResponse `json:"workflows"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Workflows) != 1 {
		t.Fatalf("got %d workflows, want the seeded default", len(out.Workflows))
	}
	wf := out.Workflows[0]
	if wf.InitialState != "draft" || len(wf.States) != 5 || len(wf.Transitions) != 10 {
		t.Errorf("the default workflow renders as %+v", wf)
	}
	for _, s := range wf.States {
		if s.Slug == "published" && !s.Publishable {
			t.Error("published is not publishable")
		}
	}
}

// checkoutEditCheckin gives a document a slug through the ordinary editing
// cycle, which is what the guards are written against.
func (h *harness) checkoutEditCheckin(t *testing.T, token, uid, slug string) {
	t.Helper()
	if w := h.do(t, http.MethodPost, "/api/v1/documents/"+uid+"/checkout", token, nil); w.Code != http.StatusOK {
		t.Fatalf("checkout = %d %s", w.Code, w.Body)
	}
	if w := h.do(t, http.MethodPatch, "/api/v1/documents/"+uid, token,
		map[string]any{"slug": slug}); w.Code != http.StatusOK {
		t.Fatalf("patch = %d %s", w.Code, w.Body)
	}
	if w := h.do(t, http.MethodPost, "/api/v1/documents/"+uid+"/checkin", token,
		map[string]any{"note": "ready"}); w.Code != http.StatusOK {
		t.Fatalf("checkin = %d %s", w.Code, w.Body)
	}
}
