// Copyright (c) 2026 Michael D Henderson.

package api

import (
	"fmt"
	"net/http"
	"time"

	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/service"
)

// The assignment, due-date, and queue routes (DESIGN.md 12, PLAN.md M5).
//
// Assignment is a subresource for the same reason a transition is: POST gives
// the document to somebody, DELETE takes it back, and there is no PATCH that
// sets an assignee among a dozen other fields. The due date is its own
// subresource beside it, which is the one addition this milestone makes to
// DESIGN.md 12's list -- see the comment on the routes in api.go.

// assignmentRequest is the body of POST /api/v1/documents/{uid}/assignment,
// which DESIGN.md 12 gives as {"user":"...","due_at":"..."}.
//
// User is a user uid, or "me" for the caller. DueAt is optional: handing work
// over with a deadline is one act, and making a client send two requests for
// it would let the second fail after the first succeeded.
type assignmentRequest struct {
	User  string `json:"user"`
	DueAt string `json:"due_at,omitempty"`
}

func (h *Handler) assignDocument(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	var req assignmentRequest
	if err := decodeJSON(r, &req); err != nil {
		h.writeError(w, r, err)
		return
	}

	var due *time.Time
	if req.DueAt != "" {
		at, err := domain.ParseDue(req.DueAt, h.svc.Now())
		if err != nil {
			h.writeError(w, r, err)
			return
		}
		due = &at
	}

	view, err := h.svc.Assign(r.Context(), identity, r.PathValue("uid"), req.User, due)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	h.writeView(w, r, http.StatusOK, view)
}

func (h *Handler) unassignDocument(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	view, err := h.svc.Unassign(r.Context(), identity, r.PathValue("uid"))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	h.writeView(w, r, http.StatusOK, view)
}

// dueRequest is the body of PUT /api/v1/documents/{uid}/due.
type dueRequest struct {
	// At is when the document is wanted: a date, a timestamp, or a duration
	// from now (domain.ParseDue).
	At string `json:"at"`
}

// setDue moves a deadline without touching the assignee.
//
// PUT rather than POST: the due date is one value that is replaced, not a
// thing that is created, and sending the same PUT twice leaves the same date.
func (h *Handler) setDue(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	var req dueRequest
	if err := decodeJSON(r, &req); err != nil {
		h.writeError(w, r, err)
		return
	}
	at, err := domain.ParseDue(req.At, h.svc.Now())
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	view, err := h.svc.SetDue(r.Context(), identity, r.PathValue("uid"), &at)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	h.writeView(w, r, http.StatusOK, view)
}

func (h *Handler) clearDue(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	view, err := h.svc.SetDue(r.Context(), identity, r.PathValue("uid"), nil)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	h.writeView(w, r, http.StatusOK, view)
}

// queueResponse is one saved queue definition and, when it was asked for by
// slug, what is in it.
type queueResponse struct {
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`

	// State, Assignee, and Overdue are the question the queue asks, so that a
	// client can show it, and so that "why is this empty" has an answer that
	// does not require reading the server's configuration.
	State    string `json:"state,omitempty"`
	Assignee string `json:"assignee,omitempty"`
	Overdue  bool   `json:"overdue,omitempty"`

	Count     int                `json:"count"`
	Documents []documentResponse `json:"documents"`
}

// listQueues is GET /api/v1/queues.
//
// DESIGN.md 12 names only the by-slug route. A client that cannot ask what
// queues exist has to be told out of band, which makes the saved definitions
// configuration only the server can see; this is the menu, and it costs one
// handler over a list already in memory.
func (h *Handler) listQueues(w http.ResponseWriter, r *http.Request, _ domain.Identity) {
	defined := h.svc.Queues()
	out := make([]queueResponse, 0, len(defined))
	for _, q := range defined {
		out = append(out, queueResponse{
			Slug:        q.Slug,
			Name:        q.Name,
			Description: q.Description,
			State:       q.State,
			Assignee:    string(q.Assignee),
			Overdue:     q.Overdue,
			Documents:   []documentResponse{},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"queues": out})
}

// showQueue is GET /api/v1/queues/{slug}.
func (h *Handler) showQueue(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	q, docs, err := h.svc.Queue(r.Context(), identity, r.PathValue("slug"))
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	now := h.svc.Now()
	who := h.lookup(r)
	wf := h.workflows(r)
	out := queueResponse{
		Slug:        q.Slug,
		Name:        q.Name,
		Description: q.Description,
		State:       q.State,
		Assignee:    string(q.Assignee),
		Overdue:     q.Overdue,
		Count:       len(docs),
		Documents:   make([]documentResponse, 0, len(docs)),
	}
	for _, d := range docs {
		out.Documents = append(out.Documents, newDocumentResponse(d, domain.Version{}, now, who, wf))
	}
	writeJSON(w, http.StatusOK, out)
}

// filterFrom reads the queue filters off a document list's query string
// (DESIGN.md 12, "filters as query params").
//
// The uid in "assignee" is resolved by the service, because resolving one is a
// read and a transport does not query the store (DESIGN.md 3).
func (h *Handler) filterFrom(r *http.Request, identity domain.Identity) (domain.DocumentFilter, error) {
	q := r.URL.Query()

	limit, err := intParam(r, "limit", domain.DefaultListLimit)
	if err != nil {
		return domain.DocumentFilter{}, err
	}
	site, err := int64Param(r, "site")
	if err != nil {
		return domain.DocumentFilter{}, err
	}
	unassigned, err := boolParam(r, "unassigned")
	if err != nil {
		return domain.DocumentFilter{}, err
	}
	overdue, err := boolParam(r, "overdue")
	if err != nil {
		return domain.DocumentFilter{}, err
	}

	return h.svc.ResolveFilter(r.Context(), identity, service.FilterRequest{
		State:      q.Get("state"),
		Site:       site,
		Assignee:   q.Get("assignee"),
		Unassigned: unassigned,
		Overdue:    overdue,
		Limit:      limit,
	})
}

// boolParam reads a flag-shaped query parameter.
//
// A bare "?overdue" and "?overdue=true" both mean yes, because that is what a
// client writes; anything else is refused rather than read as no, since a
// filter that silently does not apply is a list of the wrong documents.
func boolParam(r *http.Request, name string) (bool, error) {
	if !r.URL.Query().Has(name) {
		return false, nil
	}
	switch r.URL.Query().Get(name) {
	case "", "1", "true", "yes":
		return true, nil
	case "0", "false", "no":
		return false, nil
	default:
		return false, fmt.Errorf("%s=%q: want true or false: %w",
			name, r.URL.Query().Get(name), domain.ErrInvalid)
	}
}

// int64Param reads a non-negative integer query parameter, absent as 0.
func int64Param(r *http.Request, name string) (int64, error) {
	n, err := intParam(r, name, 0)
	return int64(n), err
}
