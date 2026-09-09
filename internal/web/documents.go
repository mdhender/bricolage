// Copyright (c) 2026 Michael D Henderson.

package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/service"
)

// The document screens: the list, the page, and the editor.

// documentsPage is GET /documents.
type documentsPage struct {
	Base

	Documents []documentRow

	// Filter is what was asked for, redrawn in the form so that a person can
	// see the question they are looking at the answer to.
	Filter documentFilterForm
	States []string
}

// documentFilterForm is the filter as the form spells it, which is not quite
// how the service takes it: "nobody" is a word in the box and a separate field
// in the request, because "anybody's" and "nobody's" are different questions.
type documentFilterForm struct {
	State    string
	Assignee string
	Overdue  bool
}

// documentRow is one line of a list, with the names a person reads already
// resolved. The lookup is the handler's, because a template does not query.
type documentRow struct {
	Document domain.Document
	Version  domain.Version
	Assignee string
	Overdue  bool
	Locked   bool
	LockedBy string
}

// documents lists what the caller may read, with the same filters
// GET /api/v1/documents takes.
func (h *Handler) documents(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	q := r.URL.Query()
	limit, err := intParam(r, "limit", 0)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	form := documentFilterForm{
		State:    q.Get("state"),
		Assignee: q.Get("assignee"),
		Overdue:  q.Get("overdue") == "on" || q.Get("overdue") == "true",
	}
	req := service.FilterRequest{
		State:      form.State,
		Assignee:   form.Assignee,
		Unassigned: form.Assignee == "nobody",
		Overdue:    form.Overdue,
		Limit:      limit,
	}
	if req.Unassigned {
		req.Assignee = ""
	}

	filter, err := h.svc.ResolveFilter(r.Context(), identity, req)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	docs, err := h.svc.ListDocuments(r.Context(), identity, filter)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	h.render(w, r, "documents.gohtml", http.StatusOK, documentsPage{
		Base:      h.base(r, identity, "Documents"),
		Documents: h.rows(r, docs),
		Filter:    form,
		States:    h.states(r),
	})
}

// rows resolves the names a list shows.
func (h *Handler) rows(r *http.Request, docs []domain.Document) []documentRow {
	now := h.svc.Now()
	who := h.lookup(r)
	out := make([]documentRow, 0, len(docs))
	for _, d := range docs {
		row := documentRow{Document: d, Overdue: d.IsOverdue(now), Locked: d.Lock.Held(now)}
		if d.AssignedTo != 0 {
			row.Assignee = who(d.AssignedTo)
		}
		if row.Locked {
			row.LockedBy = who(d.Lock.UserID)
		}
		out = append(out, row)
	}
	return out
}

// states lists every state every workflow declares, for the filter form. A
// state no process declares is a filter that matches nothing, so offering the
// declared ones is the difference between a working filter and a typo.
func (h *Handler) states(r *http.Request) []string {
	workflows, err := h.svc.Workflows(r.Context())
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, w := range workflows {
		for _, s := range w.States {
			if !seen[s.Slug] {
				seen[s.Slug] = true
				out = append(out, s.Slug)
			}
		}
	}
	return out
}

// documentPage is GET /documents/{uid}: everything one document is.
//
// The panels beside the document each come from their own service method, and
// each carries its own refusal rather than failing the page. "This server was
// not started with --output" is a fact about the resources panel; a document
// page that 503'd because of it would be a page an editor could not use to
// write a story.
type documentPage struct {
	Base

	Doc     domain.Document
	Version domain.Version

	Locked   bool
	LockedBy string
	Mine     bool
	Assignee string
	Overdue  bool

	Content []contentPair

	Actions   actionBar
	Approvals approvalPanel
	Threads   threadPanel

	Filings   []domain.Filing
	FilingErr string

	Categories []domain.Category

	URIs      []service.DocumentURI
	URIErr    string
	Resources []domain.Resource
	ResErr    string
	Channels  []domain.OutputChannel

	CanPublish bool
	CanPreview bool
}

// document draws one document.
func (h *Handler) document(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	uid := r.PathValue("uid")
	view, err := h.svc.Document(r.Context(), identity, uid)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	now := h.svc.Now()
	who := h.lookup(r)
	p := documentPage{
		Base:       h.base(r, identity, view.Version.Title),
		Doc:        view.Document,
		Version:    view.Version,
		Locked:     view.Document.Lock.Held(now),
		Mine:       view.Document.Lock.Held(now) && view.Document.Lock.UserID == identity.User.ID,
		Overdue:    view.Document.IsOverdue(now),
		CanPublish: h.svc.PublishConfigured(),
		CanPreview: h.svc.PreviewConfigured(),
	}
	if p.Locked {
		p.LockedBy = who(view.Document.Lock.UserID)
	}
	if view.Document.AssignedTo != 0 {
		p.Assignee = who(view.Document.AssignedTo)
	}
	if et, err := h.svc.ElementType(r.Context(), view.Document.ElementTypeKey); err == nil {
		if schema, err := domain.ParseElementSchema(et.Schema); err == nil {
			p.Content = contentPairs(schema, view.Version.Content)
		}
	}

	p.Actions = h.actionBar(r, identity, view.Document)
	p.Approvals = h.approvalPanel(r, identity, uid)
	p.Threads = h.threadPanel(r, identity, uid)

	if _, filings, err := h.svc.DocumentCategories(r.Context(), identity, uid); err != nil {
		p.FilingErr = err.Error()
	} else {
		p.Filings = filings
	}
	if cats, err := h.svc.Categories(r.Context(), identity, view.Document.SiteID); err == nil {
		p.Categories = cats
	}
	if _, uris, err := h.svc.DocumentURIs(r.Context(), identity, uid); err != nil {
		p.URIErr = err.Error()
	} else {
		p.URIs = uris
	}
	if _, res, err := h.svc.Resources(r.Context(), identity, uid); err != nil {
		p.ResErr = err.Error()
	} else {
		p.Resources = res
	}
	if channels, err := h.svc.OutputChannels(r.Context(), identity, view.Document.SiteID); err == nil {
		p.Channels = channels
	}

	h.render(w, r, "document.gohtml", http.StatusOK, p)
}

// newDocumentPage is GET /documents/new.
type newDocumentPage struct {
	Base

	Sites        []domain.Site
	ElementTypes []domain.ElementType
	Kinds        []string
}

func (h *Handler) newDocumentForm(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	sites, err := h.svc.Sites(r.Context(), identity)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	types, err := h.svc.ElementTypes(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.render(w, r, "document_new.gohtml", http.StatusOK, newDocumentPage{
		Base:         h.base(r, identity, "New document"),
		Sites:        sites,
		ElementTypes: types,
		Kinds:        domain.DocKinds,
	})
}

// createDocument is POST /documents.
func (h *Handler) createDocument(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	if err := form(r); err != nil {
		h.fail(w, r, err)
		return
	}
	siteID, err := strconv.ParseInt(r.PostFormValue("site"), 10, 64)
	if err != nil {
		h.fail(w, r, fmt.Errorf("site: %q is not a site: %w", r.PostFormValue("site"), domain.ErrInvalid))
		return
	}
	in := domain.NewDocument{
		SiteID:         siteID,
		Kind:           r.PostFormValue("kind"),
		ElementTypeKey: r.PostFormValue("element_type"),
		Title:          strings.TrimSpace(r.PostFormValue("title")),
		Slug:           strings.TrimSpace(r.PostFormValue("slug")),
		CoverDate:      strings.TrimSpace(r.PostFormValue("cover_date")),
	}

	// The first draft's content is built from the element type's schema, so
	// that a new story starts with the boxes it will be checked in against
	// rather than with an empty JSON object nobody can see the shape of.
	if et, err := h.svc.ElementType(r.Context(), in.ElementTypeKey); err == nil {
		if schema, err := domain.ParseElementSchema(et.Schema); err == nil {
			if content, err := contentFromForm(schema, r); err == nil {
				in.Content = content
			}
		}
	}

	view, err := h.svc.CreateDocument(r.Context(), identity, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	redirect(w, r, "/documents/"+view.Document.UID, "created")
}

// editorPage is GET /documents/{uid}/edit.
type editorPage struct {
	Base

	Doc     domain.Document
	Version domain.Version
	Fields  []fieldInput

	// Editable is false when somebody else holds the lease, or nobody does.
	// The form is drawn anyway, disabled, with the reason: an editor who
	// cannot see the boxes cannot tell "you may not" from "check it out
	// first".
	Editable bool
	Why      string
}

// editor draws the working draft.
func (h *Handler) editor(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	uid := r.PathValue("uid")
	view, err := h.svc.Document(r.Context(), identity, uid)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	p := editorPage{
		Base:    h.base(r, identity, "Editing "+view.Version.Title),
		Doc:     view.Document,
		Version: view.Version,
	}
	now := h.svc.Now()
	switch {
	case !view.Document.Lock.Held(now):
		p.Why = "this document is not checked out; check it out to edit the draft"
	case view.Document.Lock.UserID != identity.User.ID:
		p.Why = "somebody else holds the edit lease: " + h.lookup(r)(view.Document.Lock.UserID)
	default:
		p.Editable = true
	}

	et, err := h.svc.ElementType(r.Context(), view.Document.ElementTypeKey)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	schema, err := domain.ParseElementSchema(et.Schema)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	p.Fields = editorFields(schema, view.Version.Content)

	h.render(w, r, "editor.gohtml", http.StatusOK, p)
}

// saveDraft is POST /documents/{uid}/edit: DESIGN.md 12's
// PATCH /documents/{uid}, which writes the working draft and needs the lease.
func (h *Handler) saveDraft(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	if err := form(r); err != nil {
		h.fail(w, r, err)
		return
	}
	uid := r.PathValue("uid")

	view, err := h.svc.Document(r.Context(), identity, uid)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	et, err := h.svc.ElementType(r.Context(), view.Document.ElementTypeKey)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	schema, err := domain.ParseElementSchema(et.Schema)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	content, err := contentFromForm(schema, r)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	title := strings.TrimSpace(r.PostFormValue("title"))
	slug := strings.TrimSpace(r.PostFormValue("slug"))
	cover := strings.TrimSpace(r.PostFormValue("cover_date"))
	if _, err := h.svc.UpdateDraft(r.Context(), identity, uid, domain.DraftUpdate{
		Title:     &title,
		Slug:      &slug,
		CoverDate: &cover,
		Content:   &content,
	}); err != nil {
		h.fail(w, r, err)
		return
	}
	redirect(w, r, "/documents/"+uid+"/edit", "saved")
}

// checkout is POST /documents/{uid}/checkout: take the edit lease.
func (h *Handler) checkout(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	if _, err := h.svc.Checkout(r.Context(), identity, r.PathValue("uid")); err != nil {
		h.fail(w, r, err)
		return
	}
	redirect(w, r, "/documents/"+r.PathValue("uid")+"/edit", "checked out")
}

// cancelCheckout is POST /documents/{uid}/checkout/cancel: release the lease,
// keeping the draft. It is a different act from revert, and the difference is
// the one somebody loses an afternoon to (DESIGN.md 12).
func (h *Handler) cancelCheckout(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	if _, err := h.svc.CancelCheckout(r.Context(), identity, r.PathValue("uid")); err != nil {
		h.fail(w, r, err)
		return
	}
	redirect(w, r, "/documents/"+r.PathValue("uid"), "checkout cancelled; the draft is still there")
}

// checkin is POST /documents/{uid}/checkin: close the draft, making it an
// immutable version. This is where content is validated against the element
// type, so a refusal here names the offending fields.
func (h *Handler) checkin(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	if err := form(r); err != nil {
		h.fail(w, r, err)
		return
	}
	if _, err := h.svc.Checkin(r.Context(), identity, r.PathValue("uid"), r.PostFormValue("note")); err != nil {
		h.fail(w, r, err)
		return
	}
	redirect(w, r, "/documents/"+r.PathValue("uid"), "checked in")
}

// revert is POST /documents/{uid}/revert: discard the working draft.
func (h *Handler) revert(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	result, err := h.svc.Revert(r.Context(), identity, r.PathValue("uid"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	notice := "the draft was rolled back to the last checked-in version"
	if result.Deleted {
		notice = "the draft was discarded"
	}
	redirect(w, r, "/documents/"+r.PathValue("uid"), notice)
}

// assign is POST /documents/{uid}/assignment.
//
// Assignment is not a transition and needs no edit lease: requiring a checkout
// to hand work over would mean taking the draft away from the person being
// handed it (DESIGN.md 12).
func (h *Handler) assign(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	if err := form(r); err != nil {
		h.fail(w, r, err)
		return
	}
	due, err := parseWhen(r.PostFormValue("due_at"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if _, err := h.svc.Assign(r.Context(), identity, r.PathValue("uid"), r.PostFormValue("user"), due); err != nil {
		h.fail(w, r, err)
		return
	}
	redirect(w, r, "/documents/"+r.PathValue("uid"), "assigned")
}

// unassign is POST /documents/{uid}/assignment/clear. It leaves the due date
// alone: putting a document down does not make it less late.
func (h *Handler) unassign(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	if _, err := h.svc.Unassign(r.Context(), identity, r.PathValue("uid")); err != nil {
		h.fail(w, r, err)
		return
	}
	redirect(w, r, "/documents/"+r.PathValue("uid"), "unassigned")
}

// setDue is POST /documents/{uid}/due. An empty box clears it.
func (h *Handler) setDue(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	if err := form(r); err != nil {
		h.fail(w, r, err)
		return
	}
	due, err := parseWhen(r.PostFormValue("due_at"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if _, err := h.svc.SetDue(r.Context(), identity, r.PathValue("uid"), due); err != nil {
		h.fail(w, r, err)
		return
	}
	notice := "due date set"
	if due == nil {
		notice = "due date cleared"
	}
	redirect(w, r, "/documents/"+r.PathValue("uid"), notice)
}

// fileDocument is POST /documents/{uid}/categories: DESIGN.md 12's PUT, which
// replaces the filing. The first path given is the primary one.
func (h *Handler) fileDocument(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	if err := form(r); err != nil {
		h.fail(w, r, err)
		return
	}
	var paths []string
	for _, raw := range r.PostForm["categories"] {
		for _, p := range strings.Split(raw, ",") {
			if p = strings.TrimSpace(p); p != "" {
				paths = append(paths, p)
			}
		}
	}
	if _, _, err := h.svc.FileDocument(r.Context(), identity, r.PathValue("uid"), paths); err != nil {
		h.fail(w, r, err)
		return
	}
	redirect(w, r, "/documents/"+r.PathValue("uid"), "filed")
}

// lookup resolves an internal user id to the name a page shows.
//
// The handler holds it rather than the template, because a template that could
// query would be a template that queried once per row. It is the same closure
// internal/api passes its response builders, for the same reason.
func (h *Handler) lookup(r *http.Request) func(id int64) string {
	cache := map[int64]string{}
	return func(id int64) string {
		if hit, ok := cache[id]; ok {
			return hit
		}
		u, err := h.svc.UserByID(r.Context(), id)
		if err != nil {
			// A user who has been removed is not a reason to fail a read of
			// something they touched. The audit row survives them.
			cache[id] = ""
			return ""
		}
		name := u.Name
		if name == "" {
			name = u.Email
		}
		cache[id] = name
		return name
	}
}

// parseWhen reads a due date from a form box, accepting the two shapes a
// browser sends: "2026-03-01T09:00" from datetime-local and "2026-03-01" from
// date. An empty box means "no due date", which is a clear rather than an
// error.
func parseWhen(raw string) (*time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.Parse(layout, raw); err == nil {
			t = t.UTC()
			return &t, nil
		}
	}
	return nil, fmt.Errorf("due_at: %q is not a date or a time: %w", raw, domain.ErrInvalid)
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
