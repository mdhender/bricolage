// Copyright (c) 2026 Michael D Henderson.

package api

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/events"
	"github.com/mdhender/bricolage/internal/reqctx"
	"github.com/mdhender/bricolage/internal/service"
)

// The document, version, and diff routes (DESIGN.md 12, PLAN.md M3).
//
// Handlers parse a request, call one service method, and render the result.
// Every refusal arrives as a domain sentinel and is turned into a status by
// statusFor, which is the only place in this repository that maps one
// (DESIGN.md 14): a held lock is a 409, an insufficient privilege is a 403,
// and a document the caller may not see is a 404.

// documentResponse is a document as the API speaks it: uids and key names,
// never an integer primary key (invariant 10).
type documentResponse struct {
	UID         string `json:"uid"`
	Kind        string `json:"kind"`
	ElementType string `json:"element_type"`
	Site        int64  `json:"site"`

	// Workflow and State are where the document is in its editorial process
	// (PLAN.md M4). The workflow is named by its uid, like everything else
	// the API speaks; the state is the slug, which is what a transition names
	// and what a grant scope matches on.
	Workflow string `json:"workflow,omitempty"`
	State    string `json:"state,omitempty"`

	// Category is the primary category's path, and empty when the document is
	// filed nowhere (PLAN.md M7). It is the path rather than the uid because
	// the path is what a person reads, what a grant carries, and what the URI
	// is built from; the full filing, including the categories a document
	// also appears in, is its own subresource.
	Category string `json:"category,omitempty"`

	// CheckedOutBy is the uid of whoever holds a live lease, and empty when
	// nobody does. An expired lease is nobody's, so it is reported as
	// unlocked: the API answers the question the editor is asking, which is
	// "may I check this out", not "was this ever checked out".
	CheckedOutBy   string     `json:"checked_out_by,omitempty"`
	CheckedOutName string     `json:"checked_out_by_name,omitempty"`
	LockExpiresAt  *time.Time `json:"lock_expires_at,omitempty"`

	// AssignedTo is whose work this is, by uid, and empty when nobody's.
	// DueAt is when it is wanted (PLAN.md M5). Overdue is computed against
	// the server's clock rather than left to the client's, because a browser
	// with a wrong clock would draw a different queue from the one the server
	// would return.
	AssignedTo     string     `json:"assigned_to,omitempty"`
	AssignedToName string     `json:"assigned_to_name,omitempty"`
	DueAt          *time.Time `json:"due_at,omitempty"`
	Overdue        bool       `json:"overdue,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Version is the version being looked at, absent from a list.
	Version *versionResponse `json:"version,omitempty"`
}

// versionResponse is one version.
type versionResponse struct {
	Version   int    `json:"version"`
	Title     string `json:"title"`
	Slug      string `json:"slug,omitempty"`
	CoverDate string `json:"cover_date,omitempty"`
	Content   string `json:"content"`
	Note      string `json:"note,omitempty"`

	// Draft is true while this is the open working draft. "Never checked in"
	// is the absence of a timestamp; there is no version 0 and no status
	// column (DESIGN.md 5.1).
	Draft       bool       `json:"draft"`
	CheckedInAt *time.Time `json:"checked_in_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

func newVersionResponse(v domain.Version) *versionResponse {
	if v.ID == 0 {
		return nil
	}
	out := &versionResponse{
		Version:   v.Number,
		Title:     v.Title,
		Slug:      v.Slug,
		CoverDate: v.CoverDate,
		Content:   v.Content,
		Note:      v.Note,
		Draft:     v.IsDraft(),
		CreatedAt: v.CreatedAt,
	}
	if !v.CheckedInAt.IsZero() {
		at := v.CheckedInAt
		out.CheckedInAt = &at
	}
	return out
}

// documentLookup resolves an internal user id to the uid and name the API
// speaks. The handler passes one in rather than the store, because a transport
// does not query (DESIGN.md 3).
type documentLookup func(id int64) (uid, name string)

// workflowLookup resolves an internal workflow id to the uid the API speaks.
// Like documentLookup it is a closure the handler is given, because a
// transport does not query the store (DESIGN.md 3).
type workflowLookup func(id int64) string

func newDocumentResponse(d domain.Document, v domain.Version, now time.Time, who documentLookup, wf workflowLookup) documentResponse {
	out := documentResponse{
		UID:         d.UID,
		Kind:        d.Kind,
		ElementType: d.ElementTypeKey,
		Site:        d.SiteID,
		State:       d.State,
		Category:    d.CategoryPath,
		CreatedAt:   d.CreatedAt,
		UpdatedAt:   d.UpdatedAt,
		Version:     newVersionResponse(v),
	}
	if d.WorkflowID != 0 && wf != nil {
		out.Workflow = wf(d.WorkflowID)
	}
	if d.Lock.Held(now) && who != nil {
		out.CheckedOutBy, out.CheckedOutName = who(d.Lock.UserID)
		expires := d.Lock.ExpiresAt
		out.LockExpiresAt = &expires
	}
	if d.AssignedTo != 0 && who != nil {
		out.AssignedTo, out.AssignedToName = who(d.AssignedTo)
	}
	if !d.DueAt.IsZero() {
		due := d.DueAt
		out.DueAt = &due
		out.Overdue = d.IsOverdue(now)
	}
	return out
}

// createDocumentRequest is the body of POST /api/v1/documents.
type createDocumentRequest struct {
	Site        int64  `json:"site"`
	Kind        string `json:"kind"`
	ElementType string `json:"element_type"`
	Title       string `json:"title"`
	Slug        string `json:"slug,omitempty"`
	CoverDate   string `json:"cover_date,omitempty"`
	Content     string `json:"content,omitempty"`
}

func (h *Handler) createDocument(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	var req createDocumentRequest
	if err := decodeJSON(r, &req); err != nil {
		h.writeError(w, r, err)
		return
	}
	view, err := h.svc.CreateDocument(r.Context(), identity, domain.NewDocument{
		SiteID:         req.Site,
		Kind:           req.Kind,
		ElementTypeKey: req.ElementType,
		Title:          req.Title,
		Slug:           req.Slug,
		CoverDate:      req.CoverDate,
		Content:        req.Content,
	})
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	h.writeView(w, r, http.StatusCreated, view)
}

// listDocuments is GET /api/v1/documents, with the queue filters DESIGN.md 12
// names as query parameters: state, site, assignee, unassigned, overdue, and
// limit (PLAN.md M5).
//
// Every one of them is enforced, and an unrecognised value is refused rather
// than ignored: a filter accepted and dropped is a list of the wrong documents
// presented as the right ones, which is the same failure as a guard nothing
// enforces (invariant 6).
func (h *Handler) listDocuments(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	filter, err := h.filterFrom(r, identity)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	docs, err := h.svc.ListDocuments(r.Context(), identity, filter)
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	now := h.svc.Now()
	who := h.lookup(r)
	wf := h.workflows(r)
	out := make([]documentResponse, 0, len(docs))
	for _, d := range docs {
		out = append(out, newDocumentResponse(d, domain.Version{}, now, who, wf))
	}
	writeJSON(w, http.StatusOK, map[string]any{"documents": out})
}

func (h *Handler) showDocument(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	view, err := h.svc.Document(r.Context(), identity, r.PathValue("uid"))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	h.writeView(w, r, http.StatusOK, view)
}

// patchDocumentRequest is the body of PATCH /api/v1/documents/{uid}.
//
// Every field is a pointer because an omitted field and an empty one are
// different requests. DESIGN.md 12 also lists categories and a due date here;
// categories arrive with the milestone that creates them, and the due date
// went to its own subresource in M5 rather than here. The reason is the lease:
// this route writes the working draft and needs a checkout, and requiring a
// checkout to set a deadline would mean taking the draft away from the person
// the deadline is for.
type patchDocumentRequest struct {
	Title     *string `json:"title,omitempty"`
	Slug      *string `json:"slug,omitempty"`
	CoverDate *string `json:"cover_date,omitempty"`
	Content   *string `json:"content,omitempty"`
}

// patchDocument edits the open working draft.
//
// It needs the edit lease, like every other write to a draft: metadata is
// content, and a title changed by somebody who has not checked the document
// out is a title changed behind the back of whoever has.
func (h *Handler) patchDocument(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	var req patchDocumentRequest
	if err := decodeJSON(r, &req); err != nil {
		h.writeError(w, r, err)
		return
	}
	view, err := h.svc.UpdateDraft(r.Context(), identity, r.PathValue("uid"), domain.DraftUpdate{
		Title:     req.Title,
		Slug:      req.Slug,
		CoverDate: req.CoverDate,
		Content:   req.Content,
	})
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	h.writeView(w, r, http.StatusOK, view)
}

func (h *Handler) checkoutDocument(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	view, err := h.svc.Checkout(r.Context(), identity, r.PathValue("uid"))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	h.writeView(w, r, http.StatusOK, view)
}

// cancelCheckout is DELETE /api/v1/documents/{uid}/checkout.
//
// It is not in DESIGN.md 12's original list, which named checkout, checkin and
// revert. Releasing a lease without either checking in or discarding the work
// is a real thing an editor does, and the alternative -- folding it into
// revert -- is how somebody loses an afternoon to a button they thought closed
// a form. DELETE on the subresource POST created is the shape the rest of this
// API already uses.
func (h *Handler) cancelCheckout(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	view, err := h.svc.CancelCheckout(r.Context(), identity, r.PathValue("uid"))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	h.writeView(w, r, http.StatusOK, view)
}

// checkinRequest is the body of POST /api/v1/documents/{uid}/checkin. The note
// is the check-in message, which is a different thing from a comment thread
// (DESIGN.md 5.5).
type checkinRequest struct {
	Note string `json:"note,omitempty"`
}

func (h *Handler) checkinDocument(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	var req checkinRequest
	if r.ContentLength != 0 {
		if err := decodeJSON(r, &req); err != nil {
			h.writeError(w, r, err)
			return
		}
	}
	view, err := h.svc.Checkin(r.Context(), identity, r.PathValue("uid"), req.Note)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	h.writeView(w, r, http.StatusOK, view)
}

// revertDocument answers 204 when the document is gone, because there is
// nothing left to render, and the document as it stands otherwise.
func (h *Handler) revertDocument(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	out, err := h.svc.Revert(r.Context(), identity, r.PathValue("uid"))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	if out.Deleted {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	h.writeView(w, r, http.StatusOK, out.View)
}

func (h *Handler) listVersions(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	_, versions, err := h.svc.Versions(r.Context(), identity, r.PathValue("uid"))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	out := make([]versionResponse, 0, len(versions))
	for _, v := range versions {
		out = append(out, *newVersionResponse(v))
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": out})
}

func (h *Handler) showVersion(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil {
		h.writeError(w, r, fmt.Errorf("version %q: not a number: %w", r.PathValue("n"), domain.ErrInvalid))
		return
	}
	view, err := h.svc.GetVersion(r.Context(), identity, r.PathValue("uid"), n)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	h.writeView(w, r, http.StatusOK, view)
}

// diffResponse carries both the rendered text and the structured runs.
//
// The text is what earl prints and what the golden file records (PLAN.md M3
// acceptance 7); the ops are what a UI needs to mark up a paragraph without
// parsing brackets back out of prose.
type diffResponse struct {
	From   int             `json:"from"`
	To     int             `json:"to"`
	Text   string          `json:"text"`
	Fields []fieldDiffJSON `json:"fields"`
}

type fieldDiffJSON struct {
	Field   string       `json:"field"`
	Changed bool         `json:"changed"`
	Ops     []diffOpJSON `json:"ops"`
}

type diffOpJSON struct {
	// Op is "=", "-", or "+".
	Op   string `json:"op"`
	Text string `json:"text"`
}

func (h *Handler) diffDocument(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	from, err := intParam(r, "from", 0)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	to, err := intParam(r, "to", 0)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	d, err := h.svc.Diff(r.Context(), identity, r.PathValue("uid"), from, to)
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	out := diffResponse{From: d.From.Number, To: d.To.Number, Text: d.Text()}
	for _, f := range d.Fields {
		fd := fieldDiffJSON{Field: f.Field, Changed: f.Changed, Ops: make([]diffOpJSON, 0, len(f.Ops))}
		for _, op := range f.Ops {
			fd.Ops = append(fd.Ops, diffOpJSON{Op: op.Kind.String(), Text: op.Text})
		}
		out.Fields = append(out.Fields, fd)
	}
	writeJSON(w, http.StatusOK, out)
}

// eventResponse is one row of a document's history (DESIGN.md 10). The actor
// is a uid, and the display name comes from the registry in internal/events
// rather than from a table of 153 seeded rows.
type eventResponse struct {
	Type       string         `json:"type"`
	Name       string         `json:"name"`
	Actor      string         `json:"actor,omitempty"`
	ActorName  string         `json:"actor_name,omitempty"`
	Payload    map[string]any `json:"payload,omitempty"`
	OccurredAt time.Time      `json:"occurred_at"`
}

func (h *Handler) documentEvents(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	limit, err := intParam(r, "limit", 100)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	got, err := h.svc.DocumentEvents(r.Context(), identity, r.PathValue("uid"), limit)
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	who := h.lookup(r)
	out := make([]eventResponse, 0, len(got))
	for _, e := range got {
		row := eventResponse{
			Type:       e.Type,
			Name:       events.Name(e.Type),
			Payload:    e.Payload,
			OccurredAt: e.OccurredAt,
		}
		if e.ActorID != 0 && who != nil {
			row.Actor, row.ActorName = who(e.ActorID)
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out})
}

// elementTypeResponse is what a client needs to name an element type when
// creating a document, and what an administrator needs to edit one.
//
// fixed_uri is here as of M7 because it decides which of an output channel's
// two URI formats a document of this type uses, and a client showing an
// address has to be able to say why it looks like that.
type elementTypeResponse struct {
	UID       string `json:"uid"`
	KeyName   string `json:"key_name"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	TopLevel  bool   `json:"top_level"`
	FixedURI  bool   `json:"fixed_uri"`
	Paginated bool   `json:"paginated"`
	Schema    string `json:"schema"`
}

func newElementTypeResponse(et domain.ElementType) elementTypeResponse {
	return elementTypeResponse{
		UID:       et.UID,
		KeyName:   et.KeyName,
		Name:      et.Name,
		Kind:      et.Kind,
		TopLevel:  et.TopLevel,
		FixedURI:  et.FixedURI,
		Paginated: et.Paginated,
		Schema:    et.Schema,
	}
}

func (h *Handler) listElementTypes(w http.ResponseWriter, r *http.Request, _ domain.Identity) {
	types, err := h.svc.ElementTypes(r.Context())
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	out := make([]elementTypeResponse, 0, len(types))
	for _, et := range types {
		out = append(out, newElementTypeResponse(et))
	}
	writeJSON(w, http.StatusOK, map[string]any{"element_types": out})
}

// writeView renders a document and the version being looked at.
func (h *Handler) writeView(w http.ResponseWriter, r *http.Request, status int, view service.DocumentView) {
	writeJSON(w, status, newDocumentResponse(view.Document, view.Version, h.svc.Now(), h.lookup(r), h.workflows(r)))
}

// workflows resolves an internal workflow id to the uid the API speaks.
//
// It is a closure over one service call for the same reason lookup is, and it
// caches within a request because a list of a hundred documents is usually one
// workflow: the first call loads the table and the rest are free. It asks for
// identifiers only, not for whole processes, so a workflow somewhere else in
// the system being half configured does not cost this document its name.
//
// A failure to load leaves the name empty rather than failing the request --
// a document is readable whether or not the process governing it can be named
// -- but it is logged, because a name that is silently missing looks exactly
// like a document that has no workflow, and those are different problems.
func (h *Handler) workflows(r *http.Request) workflowLookup {
	var (
		loaded bool
		uids   map[int64]string
	)
	return func(id int64) string {
		if !loaded {
			loaded = true
			got, err := h.svc.WorkflowUIDs(r.Context())
			if err != nil {
				h.log.Error("naming workflows",
					"path", r.URL.Path,
					"request_id", reqctx.RequestID(r.Context()),
					"error", err)
			}
			uids = got
		}
		return uids[id]
	}
}

// lookup resolves an internal user id to the uid and name the API speaks.
//
// It is a closure over one service call rather than a query in a handler
// (DESIGN.md 3), and it caches within a request because a history of fifty
// events is usually two or three people.
func (h *Handler) lookup(r *http.Request) documentLookup {
	cache := map[int64][2]string{}
	return func(id int64) (string, string) {
		if hit, ok := cache[id]; ok {
			return hit[0], hit[1]
		}
		u, err := h.svc.UserByID(r.Context(), id)
		if err != nil {
			// A user who has been removed is not a reason to fail a read of
			// something they touched. The audit row survives them.
			cache[id] = [2]string{"", ""}
			return "", ""
		}
		cache[id] = [2]string{u.UID, u.Name}
		return u.UID, u.Name
	}
}

// intParam reads a non-negative integer query parameter.
func intParam(r *http.Request, name string, def int) (int, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s=%q: not a number: %w", name, raw, domain.ErrInvalid)
	}
	return n, nil
}
