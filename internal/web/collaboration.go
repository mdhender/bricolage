// Copyright (c) 2026 Michael D Henderson.

package web

import (
	"net/http"

	"github.com/mdhender/bricolage/internal/domain"
)

// Comments and approvals (PLAN.md M11's service methods, M13's screens).

// threadPanel is the discussion on a document.
type threadPanel struct {
	DocumentUID string
	Threads     []threadView
	Open        int
	Err         string
}

// threadView is one thread with its author names resolved.
type threadView struct {
	Comment    domain.Comment
	Author     string
	ResolvedBy string
	Replies    []threadView
	Resolved   bool
}

// threadPanel loads the threads and resolves the names.
func (h *Handler) threadPanel(r *http.Request, identity domain.Identity, uid string) threadPanel {
	panel := threadPanel{DocumentUID: uid}
	_, threads, err := h.svc.Comments(r.Context(), identity, uid)
	if err != nil {
		panel.Err = err.Error()
		return panel
	}
	who := h.lookup(r)
	panel.Open = domain.OpenThreads(threads)
	for _, t := range threads {
		view := threadView{Comment: t.Comment, Author: who(t.AuthorID), Resolved: t.Resolved()}
		if t.Resolved() {
			view.ResolvedBy = who(t.ResolvedBy)
		}
		for _, reply := range t.Replies {
			view.Replies = append(view.Replies, threadView{Comment: reply, Author: who(reply.AuthorID)})
		}
		panel.Threads = append(panel.Threads, view)
	}
	return panel
}

// comment is POST /documents/{uid}/comments. Commenting needs read and no edit
// lease, for the reason assignment and filing do not need one.
func (h *Handler) comment(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	if err := form(r); err != nil {
		h.fail(w, r, err)
		return
	}
	uid := r.PathValue("uid")
	if _, _, err := h.svc.Comment(r.Context(), identity, uid,
		r.PostFormValue("body"), r.PostFormValue("reply_to")); err != nil {
		h.fail(w, r, err)
		return
	}
	if hx(r) {
		h.partial(w, r, "threads", http.StatusOK, h.threadPanel(r, identity, uid))
		return
	}
	redirect(w, r, "/documents/"+uid+"#comments", "comment added")
}

// resolveComment is POST /comments/{uid}/resolution.
//
// The uid is a comment's and not a document's, because deciding a question is
// settled is an act on the discussion (DESIGN.md 12). Resolving a reply is a
// 409 naming the thread, and that refusal comes from the service.
func (h *Handler) resolveComment(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	doc, _, err := h.svc.ResolveComment(r.Context(), identity, r.PathValue("uid"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if hx(r) {
		h.partial(w, r, "threads", http.StatusOK, h.threadPanel(r, identity, doc.UID))
		return
	}
	redirect(w, r, "/documents/"+doc.UID+"#comments", "thread resolved")
}

// approvalPanel is the sign-offs on the version being looked at.
type approvalPanel struct {
	DocumentUID string

	Version  int
	State    string
	Required int
	Given    int

	// Mine reports that the caller's own approval is among them, which is
	// what decides between an Approve button and a Withdraw one. An approval
	// is addressed as "mine" and never by identifier: withdrawing somebody
	// else's is not an operation this system has (DESIGN.md 12).
	Mine bool

	Approvals []approvalRow
	Err       string
}

type approvalRow struct {
	Approval domain.Approval
	Who      string
}

func (h *Handler) approvalPanel(r *http.Request, identity domain.Identity, uid string) approvalPanel {
	panel := approvalPanel{DocumentUID: uid}
	view, err := h.svc.Approvals(r.Context(), identity, uid, 0)
	if err != nil {
		panel.Err = err.Error()
		return panel
	}
	who := h.lookup(r)
	panel.Version = view.Version.Number
	panel.State = view.State
	panel.Required = view.Required
	panel.Given = domain.CountApprovalsIn(view.Approvals, view.State)
	for _, a := range view.Approvals {
		panel.Approvals = append(panel.Approvals, approvalRow{Approval: a, Who: who(a.UserID)})
		if a.UserID == identity.User.ID && a.State == view.State {
			panel.Mine = true
		}
	}
	return panel
}

// approve is POST /documents/{uid}/approvals.
//
// Approving is authorised by the process rather than by a privilege of its
// own: what it needs is the lowest privilege the transitions out of this state
// ask for, and a state no transition counts approvals in refuses everybody
// (DESIGN.md 5.5). That rule is the service's, and this handler does not
// restate it.
func (h *Handler) approve(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	uid := r.PathValue("uid")
	if _, err := h.svc.Approve(r.Context(), identity, uid); err != nil {
		h.fail(w, r, err)
		return
	}
	if hx(r) {
		h.partial(w, r, "approvals", http.StatusOK, h.approvalPanel(r, identity, uid))
		return
	}
	redirect(w, r, "/documents/"+uid, "approved")
}

// withdrawApproval is POST /documents/{uid}/approvals/withdraw: DESIGN.md 12's
// DELETE .../approvals/current, spelled for a form.
func (h *Handler) withdrawApproval(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	uid := r.PathValue("uid")
	if _, err := h.svc.WithdrawApproval(r.Context(), identity, uid); err != nil {
		h.fail(w, r, err)
		return
	}
	if hx(r) {
		h.partial(w, r, "approvals", http.StatusOK, h.approvalPanel(r, identity, uid))
		return
	}
	redirect(w, r, "/documents/"+uid, "approval withdrawn")
}
