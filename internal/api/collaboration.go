// Copyright (c) 2026 Michael D Henderson.

package api

import (
	"net/http"
	"time"

	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/service"
)

// The comment and approval routes (DESIGN.md 12, PLAN.md M11).
//
// Both tables have been in the schema since M4, because the two guards that
// read them are enforced and a guard whose data nothing can produce is a guard
// nobody has tested. These are the routes that let a person produce it.
//
// Approvals are a subresource of the document rather than a collection of
// their own, and the withdrawal is DELETE .../approvals/current: an approval
// is addressed as "mine", never by identifier, because withdrawing somebody
// else's sign-off is not an operation this system has. Comments are addressed
// by uid at the top level for their resolution, because resolving a thread is
// an act on the thread rather than on the document it hangs from.

// commentResponse is one comment as the API speaks it.
type commentResponse struct {
	UID string `json:"uid"`

	// Author is the uid of whoever wrote it, never an internal key
	// (invariant 10). AuthorName is there so that a client can draw a
	// discussion without a lookup per line.
	Author     string `json:"author,omitempty"`
	AuthorName string `json:"author_name,omitempty"`

	Body string `json:"body"`

	// Version is the version being looked at when the comment was written,
	// and absent for a comment about the document as a whole.
	Version int `json:"version,omitempty"`

	// Resolved and the two fields under it describe a closed thread. A reply
	// never carries them: a thread is resolved as a whole.
	Resolved       bool       `json:"resolved"`
	ResolvedAt     *time.Time `json:"resolved_at,omitempty"`
	ResolvedBy     string     `json:"resolved_by,omitempty"`
	ResolvedByName string     `json:"resolved_by_name,omitempty"`

	CreatedAt time.Time `json:"created_at"`

	// Replies are the thread's, oldest first, and absent on a reply: threads
	// are one level deep (DESIGN.md 5.5).
	Replies []commentResponse `json:"replies,omitempty"`
}

// commentsResponse is GET /api/v1/documents/{uid}/comments.
type commentsResponse struct {
	UID string `json:"uid"`

	// Open is how many threads are still unresolved, which is the number
	// GuardCommentsResolved refuses on. It is here so that a client can say
	// "2 unresolved" without counting a list it may have paged.
	Open int `json:"open"`

	Threads []commentResponse `json:"threads"`
}

func newCommentResponse(c domain.Comment, lookup documentLookup) commentResponse {
	author, authorName := lookup(c.AuthorID)
	out := commentResponse{
		UID:        c.UID,
		Author:     author,
		AuthorName: authorName,
		Body:       c.Body,
		Version:    c.VersionNumber,
		Resolved:   c.Resolved(),
		CreatedAt:  c.CreatedAt,
	}
	if c.Resolved() {
		at := c.ResolvedAt
		out.ResolvedAt = &at
		out.ResolvedBy, out.ResolvedByName = lookup(c.ResolvedBy)
	}
	return out
}

func newThreadResponse(t domain.Thread, lookup documentLookup) commentResponse {
	out := newCommentResponse(t.Comment, lookup)
	for _, r := range t.Replies {
		out.Replies = append(out.Replies, newCommentResponse(r, lookup))
	}
	return out
}

// listComments is GET /api/v1/documents/{uid}/comments.
func (h *Handler) listComments(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	doc, threads, err := h.svc.Comments(r.Context(), identity, r.PathValue("uid"))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	lookup := h.lookup(r)
	out := commentsResponse{UID: doc.UID, Open: domain.OpenThreads(threads), Threads: make([]commentResponse, 0, len(threads))}
	for _, t := range threads {
		out.Threads = append(out.Threads, newThreadResponse(t, lookup))
	}
	writeJSON(w, http.StatusOK, out)
}

// createCommentRequest is the body of POST
// /api/v1/documents/{uid}/comments.
//
// ReplyTo is a comment uid, and there is deliberately no version field: a
// comment records the version that was on screen, which is the document's
// current one, and letting a client name a version would produce comments
// about versions nobody was reading.
type createCommentRequest struct {
	Body    string `json:"body"`
	ReplyTo string `json:"reply_to,omitempty"`
}

func (h *Handler) createComment(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	var req createCommentRequest
	if err := decodeJSON(r, &req); err != nil {
		h.writeError(w, r, err)
		return
	}
	_, c, err := h.svc.Comment(r.Context(), identity, r.PathValue("uid"), req.Body, req.ReplyTo)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, newCommentResponse(c, h.lookup(r)))
}

// resolveComment is POST /api/v1/comments/{uid}/resolution.
//
// It answers 200 rather than 201. Resolution is a property of the thread and
// this sets it; nothing was created, and a second call to the same route is
// the same answer rather than a second resolution.
func (h *Handler) resolveComment(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	_, c, err := h.svc.ResolveComment(r.Context(), identity, r.PathValue("uid"))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newCommentResponse(c, h.lookup(r)))
}

// approvalResponse is one person's sign-off.
type approvalResponse struct {
	User     string    `json:"user"`
	UserName string    `json:"user_name,omitempty"`
	State    string    `json:"state"`
	At       time.Time `json:"at"`
}

// approvalsResponse is what every approval route returns: who has signed off
// on which version, and how far that is from what the process wants.
//
// Count and Required are both here because "1 of 2" is the sentence a client
// puts beside an Approve button, and a client computing it from a list it may
// have filtered would eventually disagree with the guard.
type approvalsResponse struct {
	UID     string `json:"uid"`
	State   string `json:"state"`
	Version int    `json:"version"`

	// Current reports whether Version is the one being looked at now. An
	// older version's approvals are history: Count is every one it collected,
	// and Met is false because the document is not going to move on the
	// strength of a version it has left behind.
	Current bool `json:"current"`

	Count     int  `json:"count"`
	Required  int  `json:"required"`
	Met       bool `json:"met"`
	Approved  bool `json:"approved"`
	Withdrawn bool `json:"withdrawn,omitempty"`

	Approvals []approvalResponse `json:"approvals"`
}

func newApprovalsResponse(v service.ApprovalView, actor domain.Identity, lookup documentLookup) approvalsResponse {
	out := approvalsResponse{
		UID:       v.Document.UID,
		State:     v.State,
		Version:   v.Version.Number,
		Current:   v.Current,
		Count:     v.Count(),
		Required:  v.Required,
		Met:       v.Met(),
		Approved:  domain.ApprovedBy(v.Approvals, v.State, actor.User.ID),
		Approvals: make([]approvalResponse, 0, len(v.Approvals)),
	}
	for _, a := range v.Approvals {
		uid, name := lookup(a.UserID)
		out.Approvals = append(out.Approvals, approvalResponse{
			User: uid, UserName: name, State: a.State, At: a.CreatedAt,
		})
	}
	return out
}

// listApprovals is GET /api/v1/documents/{uid}/approvals?version=N.
//
// It is an addition to DESIGN.md 12's list, which names only the two writers.
// A client that can record an approval and cannot read one has to infer the
// count from a transition refusal, and "the old version's approvals remain
// queryable" (PLAN.md M11 acceptance 3) is a promise nothing could check.
func (h *Handler) listApprovals(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	version, err := intParam(r, "version", 0)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	view, err := h.svc.Approvals(r.Context(), identity, r.PathValue("uid"), version)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newApprovalsResponse(view, identity, h.lookup(r)))
}

// approveDocument is POST /api/v1/documents/{uid}/approvals.
//
// Approving twice is 200 rather than 201, and 201 the first time: the second
// request created nothing, and answering both the same way would hide the one
// fact a client might want -- whether this call is what recorded it.
func (h *Handler) approveDocument(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	view, err := h.svc.Approve(r.Context(), identity, r.PathValue("uid"))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	status := http.StatusOK
	if view.Changed {
		status = http.StatusCreated
	}
	writeJSON(w, status, newApprovalsResponse(view, identity, h.lookup(r)))
}

// withdrawApproval is DELETE /api/v1/documents/{uid}/approvals/current.
//
// "current" is the caller's own approval of the current version, which is the
// only approval anybody may remove. It answers 200 with the remaining
// approvals rather than 204, because the number that matters afterwards is
// what the count now is.
func (h *Handler) withdrawApproval(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	view, err := h.svc.WithdrawApproval(r.Context(), identity, r.PathValue("uid"))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	out := newApprovalsResponse(view, identity, h.lookup(r))
	out.Withdrawn = view.Changed
	writeJSON(w, http.StatusOK, out)
}
