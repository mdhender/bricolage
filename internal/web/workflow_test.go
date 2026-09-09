// Copyright (c) 2026 Michael D Henderson.

package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/mdhender/bricolage/internal/domain"
)

// PLAN.md M13 acceptance 2, both halves: the action bar on a document is
// generated from workflow.Available, and a manually forged POST for a refused
// transition returns 409.
//
// The expectations are read from Available rather than written down here. A
// test that hardcoded "Archive is refused" would keep passing if the bar
// stopped being drawn from the engine and started being drawn from a list; by
// asking the same source the template is given, what is asserted is that the
// two agree.

// TestActionBarIsAvailableRendered is acceptance 2's first half.
func TestActionBarIsAvailableRendered(t *testing.T) {
	h := newHarness(t).seeded(t)

	// The document is created by somebody holding Create, and looked at by
	// somebody holding Edit: "Submit" needs Edit and "Archive" needs Create,
	// so the writer's bar has a permitted move and one refused on privilege.
	h.user(t, "editor@example.com", domain.Create)
	h.user(t, "writer@example.com", domain.Edit)
	uid := h.document(t, h.session(t, "editor@example.com"), "The Quick Brown Fox")
	session := h.session(t, "writer@example.com")

	identity, err := h.svc.Authenticate(t.Context(), session.Value)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	_, allowed, err := h.svc.Transitions(t.Context(), identity, uid)
	if err != nil {
		t.Fatalf("Transitions: %v", err)
	}
	if len(allowed) == 0 {
		t.Fatal("the seeded workflow declares no transitions out of draft; this test proves nothing")
	}

	body := h.get(t, "/documents/"+uid, session).Body.String()

	var refused, permitted int
	for _, a := range allowed {
		if !strings.Contains(body, a.Transition.Name) {
			t.Errorf("the action bar does not offer %q, which Available lists", a.Transition.Name)
		}
		if a.Permitted {
			permitted++
			continue
		}
		refused++
		// A refused move is drawn with its reason rather than left out. An
		// action that vanishes teaches nobody anything.
		if a.Reason != "" && !strings.Contains(body, a.Reason) {
			t.Errorf("%q is refused (%q) and the bar does not say why", a.Transition.Name, a.Reason)
		}
		if a.Guard != "" && !strings.Contains(body, string(a.Guard)) {
			t.Errorf("%q was refused by the %s guard and the bar does not name it", a.Transition.Name, a.Guard)
		}
	}
	if refused == 0 {
		t.Fatal("no transition was refused for a writer; this test cannot see whether refusals are drawn")
	}
	if permitted == 0 {
		t.Fatal("no transition was permitted for a writer; this test cannot see whether buttons are drawn")
	}
	if !strings.Contains(body, "disabled") {
		t.Error("the bar draws no disabled button, so a refused move was left out rather than shown")
	}
}

// TestForgedTransitionIsRefused is acceptance 2's second half.
//
// The POST does not go near the bar that would have drawn the button. Three
// forgeries are refused, and the statuses are not all the same, deliberately:
// a guard refusal and a move the state machine does not declare are 409,
// because both are statements about the document, and a privilege refusal is
// 403, because that one is a statement about the person (DESIGN.md 12). A UI
// that rendered "you may not" and "not yet" the same way is a UI that sends an
// editor to ask for permission they already have.
func TestForgedTransitionIsRefused(t *testing.T) {
	h := newHarness(t).seeded(t)
	h.user(t, "editor@example.com", domain.Create)
	h.user(t, "writer@example.com", domain.Edit)
	editor := h.session(t, "editor@example.com")
	writer := h.session(t, "writer@example.com")
	uid := h.document(t, editor, "The Quick Brown Fox")

	// Into review, where the seeded process wants an approval before it will
	// go any further. Nobody approves, so "Approve" is refused by a guard.
	if w := h.post(t, "/documents/"+uid+"/transitions", editor, url.Values{"to": {"review"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("submitting to review = %d: %s", w.Code, w.Body)
	}

	identity, err := h.svc.Authenticate(t.Context(), editor.Value)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	_, allowed, err := h.svc.Transitions(t.Context(), identity, uid)
	if err != nil {
		t.Fatalf("Transitions: %v", err)
	}
	var guardRefused string
	for _, a := range allowed {
		if !a.Permitted && a.Guard != "" && !a.NeedsNote {
			guardRefused = a.Transition.To
			break
		}
	}
	if guardRefused == "" {
		t.Fatal("no guard refused anything in review, so there is no button to forge")
	}

	for _, tc := range []struct {
		name    string
		session *http.Cookie
		to      string
		want    int
	}{
		{"a guard refused it", editor, guardRefused, http.StatusConflict},
		{"the state machine does not declare it from here", editor, "published", http.StatusConflict},
		{"no workflow declares the state at all", editor, "nonsense", http.StatusConflict},
		{"the caller does not hold the privilege", writer, guardRefused, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := h.post(t, "/documents/"+uid+"/transitions", tc.session, url.Values{"to": {tc.to}})
			if w.Code != tc.want {
				t.Errorf("POST a forged transition to %q = %d, want %d: %s", tc.to, w.Code, tc.want, w.Body)
			}
		})
	}

	// The document did not move. A refusal that answered 409 and wrote the
	// state anyway would pass every assertion above.
	view, err := h.svc.Document(t.Context(), identity, uid)
	if err != nil {
		t.Fatalf("Document: %v", err)
	}
	if view.Document.State != "review" {
		t.Errorf("the document is in %q after four refused transitions, want review", view.Document.State)
	}
}

// TestPermittedTransitionMoves asserts the other side of the same rule: a
// button the bar draws performs the move, and the HTMX fragment that comes
// back is the bar the new state produces.
func TestPermittedTransitionMoves(t *testing.T) {
	h := newHarness(t).seeded(t)
	h.user(t, "editor@example.com", domain.Publish)
	session := h.session(t, "editor@example.com")
	uid := h.document(t, session, "The Quick Brown Fox")

	identity, err := h.svc.Authenticate(t.Context(), session.Value)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	_, allowed, err := h.svc.Transitions(t.Context(), identity, uid)
	if err != nil {
		t.Fatalf("Transitions: %v", err)
	}
	var to string
	for _, a := range allowed {
		if a.Permitted {
			to = a.Transition.To
			break
		}
	}
	if to == "" {
		t.Fatal("nothing is permitted for a publisher on a fresh draft")
	}

	req := url.Values{"to": {to}, "note": {"because"}}
	w := h.postHX(t, "/documents/"+uid+"/transitions", session, req)
	if w.Code != http.StatusOK {
		t.Fatalf("POST a permitted transition = %d, want 200: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), `id="actions"`) {
		t.Errorf("the HTMX answer is not the action bar: %s", w.Body)
	}
	if !strings.Contains(w.Body.String(), to) {
		t.Errorf("the returned bar does not mention the new state %q: %s", to, w.Body)
	}

	view, err := h.svc.Document(t.Context(), identity, uid)
	if err != nil {
		t.Fatalf("Document: %v", err)
	}
	if view.Document.State != to {
		t.Errorf("the document is in %q, want %q", view.Document.State, to)
	}
}
