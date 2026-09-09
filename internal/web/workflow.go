// Copyright (c) 2026 Michael D Henderson.

package web

import (
	"net/http"

	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/workflow"
)

// The desk actions (PLAN.md M13 acceptance 2).
//
// The action bar is workflow.Available, rendered. Every transition out of the
// document's current state is drawn: the permitted ones as buttons, the
// refused ones disabled with the reason beside them and, when a guard refused
// rather than a privilege, its name. A refused action that vanished would
// teach nobody anything -- an editor would be left asking why "Approve" is
// not there, and the answer is "it needs one more approval".
//
// Nothing here decides whether a move is permitted. Available and Do share one
// check function (invariant 5), so what the bar draws and what the POST
// performs are the same rule asked twice: a button that appears is one the
// engine will accept unless something changed underneath it, and a POST forged
// for a button that does not appear is refused inside the transaction and
// answers 409.

// actionBar is what the partial draws.
type actionBar struct {
	DocumentUID string
	State       string
	Allowed     []workflow.Allowed

	// Err is why the bar is empty, when it is. A workflow that cannot be
	// loaded is worth saying out loud rather than drawing as "no actions".
	Err string
}

// actionBar asks the engine what is available.
func (h *Handler) actionBar(r *http.Request, identity domain.Identity, doc domain.Document) actionBar {
	bar := actionBar{DocumentUID: doc.UID, State: doc.State}
	_, allowed, err := h.svc.Transitions(r.Context(), identity, doc.UID)
	if err != nil {
		bar.Err = err.Error()
		return bar
	}
	bar.Allowed = allowed
	return bar
}

// transition is POST /documents/{uid}/transitions.
//
// A "to" the state machine does not declare from where the document is, and a
// move a guard refuses, are both refusals from the service, and both reach the
// browser as the status internal/edge maps them to -- 409 for either. This
// handler contains no check of its own: a second opinion here is exactly the
// defect invariant 5 exists to prevent.
func (h *Handler) transition(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	if err := form(r); err != nil {
		h.fail(w, r, err)
		return
	}
	uid := r.PathValue("uid")
	to := r.PostFormValue("to")

	view, err := h.svc.Transition(r.Context(), identity, uid, to, r.PostFormValue("note"))
	if err != nil {
		h.fail(w, r, err)
		return
	}

	// HTMX swaps the bar for the one the new state produces; a browser with
	// no JavaScript is redirected back to the document, which redraws it.
	if hx(r) {
		h.partial(w, r, "actions", http.StatusOK, h.actionBar(r, identity, view.Document))
		return
	}
	redirect(w, r, "/documents/"+uid, "moved to "+view.Document.State)
}
