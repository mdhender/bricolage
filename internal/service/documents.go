// Copyright (c) 2026 Michael D Henderson.

package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/mdhender/bricolage/internal/authz"
	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/events"
	"github.com/mdhender/bricolage/internal/ids"
	"github.com/mdhender/bricolage/internal/store"
)

// The document use cases (PLAN.md M3): create, check out, edit, check in,
// cancel, revert, read, and diff.
//
// Two rules shape every method here. Authorization is resolved once, against
// the identity the transport already has, by the pure resolver in
// internal/authz -- no method issues a permission query of its own
// (DESIGN.md 7.2). And every state change writes its event inside the
// transaction that made it (invariant 7), which is why the event is handed to
// the store rather than recorded beside it: an event that can be rolled back
// separately from its change is not an audit record.
//
// Nothing in this file writes documents.state and nothing may:
// internal/workflow is its only writer (invariant 4). What M4 adds here is the
// placement of a new document into a workflow, which is not a transition --
// a document does not move into its initial state, it starts there -- and the
// two methods in workflow.go that hand a move to the engine.

// DocumentView is a document and the version being looked at, which is what
// every read of a document returns. They travel together because a document
// has no title of its own and asking for one would be asking which version's.
type DocumentView struct {
	Document domain.Document
	Version  domain.Version
}

// CreateDocument creates a document and its first working draft.
//
// Create is the privilege, resolved against the document that is about to
// exist rather than one that does -- which is why domain.Subject carries
// attributes rather than a document (DESIGN.md 7.2).
func (s *Service) CreateDocument(ctx context.Context, actor domain.Identity, in domain.NewDocument) (DocumentView, error) {
	if err := in.Validate(); err != nil {
		return DocumentView{}, err
	}

	subject := domain.Subject{SiteID: in.SiteID, DocKind: in.Kind}
	if !authz.Allows(actor.Grants, subject, domain.Create) {
		return DocumentView{}, fmt.Errorf("creating a %s on site %d: %w", in.Kind, in.SiteID, domain.ErrForbidden)
	}

	et, err := s.db.ElementTypeByKeyName(ctx, in.ElementTypeKey)
	if err != nil {
		return DocumentView{}, err
	}
	if et.Kind != in.Kind {
		// An element type declares which kind it applies to. Accepting a
		// mismatch would produce a document whose fields nothing can render.
		return DocumentView{}, fmt.Errorf("element type %q applies to %s, not %s: %w",
			et.KeyName, et.Kind, in.Kind, domain.ErrInvalid)
	}

	// The workflow that governs the document is resolved before it exists,
	// because documents.workflow_id is NOT NULL with a composite foreign key
	// to workflow_states: a document with no process is not a document this
	// schema can hold. A kind with no workflow is a clean refusal naming the
	// kind rather than a placement in somebody else's process.
	wf, err := s.db.WorkflowFor(ctx, in.Kind, in.SiteID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return DocumentView{}, fmt.Errorf(
				"no workflow governs %s documents on site %d; configure one first: %w",
				in.Kind, in.SiteID, domain.ErrConflict)
		}
		return DocumentView{}, err
	}

	now := s.Now()
	uid, err := ids.New(now)
	if err != nil {
		return DocumentView{}, err
	}

	doc, ver, err := s.db.CreateDocument(ctx, store.NewDocument{
		UID:           uid,
		SiteID:        in.SiteID,
		Kind:          in.Kind,
		ElementTypeID: et.ID,
		WorkflowID:    wf.ID,
		State:         wf.InitialState,
		Title:         in.Title,
		Slug:          in.Slug,
		CoverDate:     in.CoverDate,
		Content:       in.Content,
		CreatedBy:     actor.User.ID,
		CreatedAt:     now,
		Event: domain.Event{
			Type:    events.DocumentCreated,
			ActorID: actor.User.ID,
			Payload: map[string]any{
				"uid":          uid,
				"kind":         in.Kind,
				"element_type": et.KeyName,
				"title":        in.Title,
				"site":         in.SiteID,
				"workflow":     wf.Name,
				"state":        wf.InitialState,
			},
			OccurredAt: now,
		},
	})
	if err != nil {
		return DocumentView{}, err
	}
	return DocumentView{Document: doc, Version: ver}, nil
}

// Checkout takes the edit lease and makes sure there is a working draft
// (PLAN.md M3 acceptance 3 and 4).
//
// The refusal when somebody else holds it is domain.ErrConflict, a 409 at the
// edge, and not domain.ErrForbidden: a held lock is a statement about the
// document rather than about the person asking, and telling an editor they are
// "not permitted" to edit something they are permitted to edit sends them to
// an administrator instead of to a colleague.
func (s *Service) Checkout(ctx context.Context, actor domain.Identity, uid string) (DocumentView, error) {
	doc, err := s.mayEdit(ctx, actor, uid)
	if err != nil {
		return DocumentView{}, err
	}

	now := s.Now()
	doc, ver, err := s.db.Checkout(ctx, doc.ID, actor.User.ID, now, now.Add(s.lockLease), domain.Event{
		Type:    events.DocumentCheckedOut,
		ActorID: actor.User.ID,
		Payload: map[string]any{
			"uid":        doc.UID,
			"expires_at": now.Add(s.lockLease),
		},
		OccurredAt: now,
	})
	if err != nil {
		return DocumentView{}, err
	}
	return DocumentView{Document: doc, Version: ver}, nil
}

// UpdateDraft writes the open working draft and extends the lease.
//
// The update is applied to the draft as it stands rather than to what the
// caller believes it to be, so two edits to different fields do not overwrite
// one another. The fields that changed are named in the event; their contents
// are not, because a document body is not something this system logs
// (DESIGN.md 14).
func (s *Service) UpdateDraft(ctx context.Context, actor domain.Identity, uid string, u domain.DraftUpdate) (DocumentView, error) {
	if u.IsEmpty() {
		return DocumentView{}, fmt.Errorf("update: nothing to change: %w", domain.ErrInvalid)
	}
	doc, err := s.mayEdit(ctx, actor, uid)
	if err != nil {
		return DocumentView{}, err
	}

	draft, err := s.db.DraftForDocument(ctx, doc.ID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return DocumentView{}, fmt.Errorf("document %s is not checked out: %w", uid, domain.ErrConflict)
		}
		return DocumentView{}, err
	}
	updated, err := u.Apply(draft)
	if err != nil {
		return DocumentView{}, err
	}

	now := s.Now()
	doc, ver, err := s.db.UpdateDraft(ctx, doc.ID, actor.User.ID, now, now.Add(s.lockLease), updated, domain.Event{
		Type:    events.DocumentDraftUpdated,
		ActorID: actor.User.ID,
		Payload: map[string]any{
			"uid":     doc.UID,
			"version": draft.Number,
			"fields":  changedFields(draft, updated),
		},
		OccurredAt: now,
	})
	if err != nil {
		return DocumentView{}, err
	}
	return DocumentView{Document: doc, Version: ver}, nil
}

// changedFields names what an update actually changed, for the event payload.
// A request that sets a field to what it already held changed nothing, and
// saying otherwise makes the history harder to read rather than fuller.
func changedFields(before, after domain.Version) []string {
	var out []string
	if before.Title != after.Title {
		out = append(out, "title")
	}
	if before.Slug != after.Slug {
		out = append(out, "slug")
	}
	if before.CoverDate != after.CoverDate {
		out = append(out, "cover_date")
	}
	if before.Content != after.Content {
		out = append(out, "content")
	}
	if out == nil {
		out = []string{}
	}
	return out
}

// Checkin closes the working draft, which becomes an immutable version
// (PLAN.md M3 acceptance 1).
func (s *Service) Checkin(ctx context.Context, actor domain.Identity, uid, note string) (DocumentView, error) {
	doc, err := s.mayEdit(ctx, actor, uid)
	if err != nil {
		return DocumentView{}, err
	}

	// The draft is read first so that the event can name the version being
	// checked in. After the check-in the row is immutable and its number is
	// what a reader of the history needs to ask for it back.
	draft, err := s.db.DraftForDocument(ctx, doc.ID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return DocumentView{}, fmt.Errorf("document %s is not checked out: %w", uid, domain.ErrConflict)
		}
		return DocumentView{}, err
	}

	now := s.Now()
	doc, ver, err := s.db.Checkin(ctx, doc.ID, actor.User.ID, now, note, domain.Event{
		Type:    events.DocumentCheckedIn,
		ActorID: actor.User.ID,
		Payload: map[string]any{
			"uid":     doc.UID,
			"version": draft.Number,
			"note":    note,
		},
		OccurredAt: now,
	})
	if err != nil {
		return DocumentView{}, err
	}
	return DocumentView{Document: doc, Version: ver}, nil
}

// CancelCheckout releases the lease and leaves the draft alone.
//
// It is not Revert. Cancelling says "I am not editing this now"; reverting
// says "throw away what I wrote". Conflating them is how somebody loses an
// afternoon's work to a button they thought closed a form.
func (s *Service) CancelCheckout(ctx context.Context, actor domain.Identity, uid string) (DocumentView, error) {
	doc, err := s.mayEdit(ctx, actor, uid)
	if err != nil {
		return DocumentView{}, err
	}

	now := s.Now()
	doc, err = s.db.CancelCheckout(ctx, doc.ID, actor.User.ID, now, domain.Event{
		Type:       events.DocumentCheckoutCanceled,
		ActorID:    actor.User.ID,
		Payload:    map[string]any{"uid": doc.UID},
		OccurredAt: now,
	})
	if err != nil {
		return DocumentView{}, err
	}

	draft, err := s.db.DraftForDocument(ctx, doc.ID)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return DocumentView{}, err
	}
	return DocumentView{Document: doc, Version: draft}, nil
}

// RevertResult says what reverting did: the document as it stands, or that it
// is gone.
type RevertResult struct {
	Deleted bool
	View    DocumentView
}

// Revert discards the open working draft (PLAN.md M3 acceptance 5).
//
// A document whose draft was its only version is deleted, because there is no
// version 0 to fall back to and nothing to show. The event is written against
// the document's id before the row goes, so a deleted document still has a
// history.
func (s *Service) Revert(ctx context.Context, actor domain.Identity, uid string) (RevertResult, error) {
	doc, err := s.mayEdit(ctx, actor, uid)
	if err != nil {
		return RevertResult{}, err
	}

	now := s.Now()
	out, err := s.db.Revert(ctx, doc.ID, actor.User.ID, now, domain.Event{
		Type:       events.DocumentReverted,
		ActorID:    actor.User.ID,
		Payload:    map[string]any{"uid": doc.UID},
		OccurredAt: now,
	})
	if err != nil {
		return RevertResult{}, err
	}
	return RevertResult{
		Deleted: out.Deleted,
		View:    DocumentView{Document: out.Document, Version: out.Version},
	}, nil
}

// Document reads one document and the version currently being looked at.
func (s *Service) Document(ctx context.Context, actor domain.Identity, uid string) (DocumentView, error) {
	doc, err := s.mayRead(ctx, actor, uid)
	if err != nil {
		return DocumentView{}, err
	}
	return s.viewOf(ctx, doc)
}

// viewOf pairs a document with its current version, which is what every read
// and every write returns. A document has no title of its own, so handing one
// back alone would make the caller ask which version's.
func (s *Service) viewOf(ctx context.Context, doc domain.Document) (DocumentView, error) {
	view := DocumentView{Document: doc}
	if doc.CurrentVersionID == 0 {
		return view, nil
	}
	versions, err := s.db.VersionsForDocument(ctx, doc.ID)
	if err != nil {
		return DocumentView{}, err
	}
	for _, v := range versions {
		if v.ID == doc.CurrentVersionID {
			view.Version = v
			break
		}
	}
	return view, nil
}

// Versions returns a document's versions, newest first.
func (s *Service) Versions(ctx context.Context, actor domain.Identity, uid string) (domain.Document, []domain.Version, error) {
	doc, err := s.mayRead(ctx, actor, uid)
	if err != nil {
		return domain.Document{}, nil, err
	}
	versions, err := s.db.VersionsForDocument(ctx, doc.ID)
	return doc, versions, err
}

// GetVersion reads one numbered version of a document.
func (s *Service) GetVersion(ctx context.Context, actor domain.Identity, uid string, number int) (DocumentView, error) {
	doc, err := s.mayRead(ctx, actor, uid)
	if err != nil {
		return DocumentView{}, err
	}
	if number <= 0 {
		return DocumentView{}, fmt.Errorf("version %d: versions are numbered from 1: %w", number, domain.ErrInvalid)
	}
	v, err := s.db.VersionByNumber(ctx, doc.ID, number)
	if err != nil {
		return DocumentView{}, err
	}
	return DocumentView{Document: doc, Version: v}, nil
}

// ListDocuments returns the documents the actor may read.
//
// The filter is applied here rather than in SQL because the rule is
// authz.Resolve, which is a pure function over grants the caller already
// holds, and a second implementation of it in SQL is a second implementation
// that will disagree. M5 adds the queue filters and the index for them; what
// this must never become is a list that shows a row the reader may not open.
func (s *Service) ListDocuments(ctx context.Context, actor domain.Identity, limit int) ([]domain.Document, error) {
	all, err := s.db.ListDocuments(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Document, 0, len(all))
	for _, d := range all {
		if authz.Allows(actor.Grants, d.Subject(), domain.Read) {
			out = append(out, d)
		}
	}
	return out, nil
}

// Diff is the word-level difference between two versions of a document
// (PLAN.md M3 acceptance 7).
type Diff struct {
	Document domain.Document
	From     domain.Version
	To       domain.Version
	Fields   []domain.FieldDiff
}

// Text renders the diff the way earl prints it and the golden file records it.
func (d Diff) Text() string {
	return domain.RenderDiff(d.From.Number, d.To.Number, d.Fields)
}

// Diff compares two versions of a document.
func (s *Service) Diff(ctx context.Context, actor domain.Identity, uid string, from, to int) (Diff, error) {
	doc, err := s.mayRead(ctx, actor, uid)
	if err != nil {
		return Diff{}, err
	}
	if from <= 0 || to <= 0 {
		return Diff{}, fmt.Errorf("versions %d and %d: versions are numbered from 1: %w", from, to, domain.ErrInvalid)
	}
	a, err := s.db.VersionByNumber(ctx, doc.ID, from)
	if err != nil {
		return Diff{}, err
	}
	b, err := s.db.VersionByNumber(ctx, doc.ID, to)
	if err != nil {
		return Diff{}, err
	}
	return Diff{Document: doc, From: a, To: b, Fields: domain.DiffVersions(a, b)}, nil
}

// DocumentEvents returns a document's history, newest first (DESIGN.md 10).
//
// This is what the audit spine is for: a query rather than a log grep. It is
// readable by anybody who may read the document, because the history of a
// document is part of the document.
func (s *Service) DocumentEvents(ctx context.Context, actor domain.Identity, uid string, limit int) ([]domain.Event, error) {
	doc, err := s.mayRead(ctx, actor, uid)
	if err != nil {
		return nil, err
	}
	return s.db.EventsForSubject(ctx, domain.SubjectDocument, doc.ID, limit)
}

// UserByID resolves an internal user id to the user the API renders.
//
// It is here rather than reached for in a handler because a transport does not
// query the store (DESIGN.md 3). The id always comes from a row this process
// already holds -- a lock holder, an event's actor -- and never from the wire
// (invariant 10).
func (s *Service) UserByID(ctx context.Context, id int64) (domain.User, error) {
	return s.db.UserByID(ctx, id)
}

// ElementTypes returns every element type, which is what a client needs to
// name one when creating a document.
func (s *Service) ElementTypes(ctx context.Context) ([]domain.ElementType, error) {
	return s.db.ListElementTypes(ctx)
}

// mayRead and mayEdit load a document and resolve the privilege the operation
// needs against it.
//
// They exist so that no handler and no other method decides for itself what
// "may edit this document" means. The subject comes from the document, the
// answer comes from authz.Resolve, and both are in one place.
func (s *Service) mayRead(ctx context.Context, actor domain.Identity, uid string) (domain.Document, error) {
	return s.mayDo(ctx, actor, uid, domain.Read)
}

func (s *Service) mayEdit(ctx context.Context, actor domain.Identity, uid string) (domain.Document, error) {
	return s.mayDo(ctx, actor, uid, domain.Edit)
}

func (s *Service) mayDo(ctx context.Context, actor domain.Identity, uid string, want domain.Privilege) (domain.Document, error) {
	doc, err := s.db.DocumentByUID(ctx, uid)
	if err != nil {
		return domain.Document{}, err
	}
	if !authz.Allows(actor.Grants, doc.Subject(), want) {
		// A caller who may not read it is told it is not there, so that the
		// API does not confirm the existence of documents to people who may
		// not see them. A caller who may read but not edit is told the truth,
		// because they can already see the row.
		if want != domain.Read && authz.Allows(actor.Grants, doc.Subject(), domain.Read) {
			return domain.Document{}, fmt.Errorf("document %s: %s is required: %w", uid, want, domain.ErrForbidden)
		}
		return domain.Document{}, fmt.Errorf("document %q: %w", uid, domain.ErrNotFound)
	}
	return doc, nil
}
