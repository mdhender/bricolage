// Copyright (c) 2026 Michael D Henderson.

package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mdhender/bricolage/internal/authz"
	"github.com/mdhender/bricolage/internal/config"
	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/events"
	"github.com/mdhender/bricolage/internal/store"
)

// Assignment, due dates, and queues (PLAN.md M5).
//
// Three rules shape this file.
//
// Assigning work is not a transition. A document handed from an editor to a
// writer has not moved anywhere in the process, and internal/workflow remains
// the only thing that writes documents.state (invariant 4). What a transition
// may do is assign as an *effect* -- EffectAssignToActor and
// EffectClearAssignee -- and those go through the engine like everything else.
//
// Assigning work does not need the edit lease. The lease protects the working
// draft, which is content; the assignee and the due date are properties of the
// document row. Requiring a checkout to hand something over would mean taking
// the draft away from the person you are handing it to, which is the opposite
// of what was asked.
//
// The privilege is Edit, resolved against the document. Somebody who may
// change a document may say who is working on it and when it is wanted; a
// reader may not. There is deliberately no separate "assign" privilege: the
// scale is ordered and cumulative (DESIGN.md 7), and a sixth level whose only
// difference from Edit is this one operation would be a level nobody could
// explain.

// SelfAssignee is the name a caller uses for themselves, in a filter or in an
// assignment. It saves a client a round trip to /api/v1/me and it is what the
// saved queue "mine" resolves to per request.
const SelfAssignee = "me"

// Assign gives a document to somebody, optionally with a due date
// (PLAN.md M5 acceptance 2).
//
// The assignee is named by uid, like everything else the API speaks
// (invariant 10), or by SelfAssignee for the caller. A uid nobody holds is a
// 404 naming the user rather than a silent assignment to nobody.
func (s *Service) Assign(ctx context.Context, actor domain.Identity, uid, assignee string, due *time.Time) (DocumentView, error) {
	doc, err := s.mayAssign(ctx, actor, uid)
	if err != nil {
		return DocumentView{}, err
	}
	user, err := s.resolveUser(ctx, actor, assignee)
	if err != nil {
		return DocumentView{}, err
	}

	now := s.Now()
	payload := map[string]any{
		"uid":      doc.UID,
		"assignee": user.UID,
		"name":     user.Name,
	}
	if due != nil {
		payload["due_at"] = due.UTC()
	}

	moved, err := s.db.UpdateAssignment(ctx, store.AssignmentUpdate{
		DocumentID:  doc.ID,
		SetAssignee: domain.Ref(user.ID),
		SetDueAt:    due,
		Now:         now,
		Event: domain.Event{
			Type:       events.DocumentAssigned,
			ActorID:    actor.User.ID,
			Payload:    payload,
			OccurredAt: now,
		},
	})
	if err != nil {
		return DocumentView{}, err
	}
	return s.viewOf(ctx, moved)
}

// Unassign returns a document to the pile.
//
// It leaves the due date alone. A deadline belongs to the work rather than to
// whoever happens to be holding it, and clearing one because somebody put the
// document down is how a story quietly stops being late.
func (s *Service) Unassign(ctx context.Context, actor domain.Identity, uid string) (DocumentView, error) {
	doc, err := s.mayAssign(ctx, actor, uid)
	if err != nil {
		return DocumentView{}, err
	}

	// Who it was taken from is recorded before the write, because afterwards
	// there is nothing to read: the whole point of the operation is that the
	// column is now NULL.
	payload := map[string]any{"uid": doc.UID}
	if doc.AssignedTo != 0 {
		if prior, err := s.db.UserByID(ctx, doc.AssignedTo); err == nil {
			payload["was"] = prior.UID
			payload["was_name"] = prior.Name
		}
	}

	now := s.Now()
	moved, err := s.db.UpdateAssignment(ctx, store.AssignmentUpdate{
		DocumentID:  doc.ID,
		SetAssignee: domain.Ref(int64(0)),
		Now:         now,
		Event: domain.Event{
			Type:       events.DocumentUnassigned,
			ActorID:    actor.User.ID,
			Payload:    payload,
			OccurredAt: now,
		},
	})
	if err != nil {
		return DocumentView{}, err
	}
	return s.viewOf(ctx, moved)
}

// SetDue moves a document's deadline, or clears it when due is nil
// (PLAN.md M5 acceptance 2).
//
// It leaves the assignee alone: a deadline can be set on work nobody has
// picked up yet, which is how a desk gets a date before it gets a person.
func (s *Service) SetDue(ctx context.Context, actor domain.Identity, uid string, due *time.Time) (DocumentView, error) {
	doc, err := s.mayAssign(ctx, actor, uid)
	if err != nil {
		return DocumentView{}, err
	}

	now := s.Now()
	payload := map[string]any{"uid": doc.UID}
	if due == nil {
		payload["cleared"] = true
	} else {
		payload["due_at"] = due.UTC()
	}

	// A nil due date means "clear it", so the pointer handed to the store is
	// never nil: nil there means "leave the column alone", which is a third
	// thing this method never asks for.
	set := due
	if set == nil {
		set = domain.Ref(time.Time{})
	}

	moved, err := s.db.UpdateAssignment(ctx, store.AssignmentUpdate{
		DocumentID: doc.ID,
		SetDueAt:   set,
		Now:        now,
		Event: domain.Event{
			Type:       events.DocumentDueChanged,
			ActorID:    actor.User.ID,
			Payload:    payload,
			OccurredAt: now,
		},
	})
	if err != nil {
		return DocumentView{}, err
	}
	return s.viewOf(ctx, moved)
}

// Queue runs one saved queue definition (DESIGN.md 12, PLAN.md M5).
//
// The definition lives in configuration rather than in the schema, so this
// translates a config.Queue into a domain.DocumentFilter and then runs the
// same code path a filtered list runs. There is deliberately no second query
// behind a queue: a saved question that resolved differently from the same
// question asked directly is a saved question nobody could trust.
func (s *Service) Queue(ctx context.Context, actor domain.Identity, slug string) (config.Queue, []domain.Document, error) {
	q, ok := s.queues.Lookup(slug)
	if !ok {
		return config.Queue{}, nil, fmt.Errorf("queue %q (have %s): %w",
			slug, strings.Join(s.queues.Slugs(), ", "), domain.ErrNotFound)
	}

	filter := domain.DocumentFilter{State: q.State, Overdue: q.Overdue}
	switch q.Assignee {
	case config.QueueAssigneeActor:
		filter.AssignedTo = actor.User.ID
	case config.QueueAssigneeNobody:
		filter.Unassigned = true
	}

	docs, err := s.ListDocuments(ctx, actor, filter)
	return q, docs, err
}

// Queues returns every saved queue definition, in the order configured.
func (s *Service) Queues() []config.Queue { return s.queues.List() }

// FilterRequest is a queue query as a client spells it, with the assignee
// still a name rather than a user.
//
// It is resolved here rather than at the transport edge because resolving a
// uid is a read, and a transport does not query the store (DESIGN.md 3).
type FilterRequest struct {
	State string
	Site  int64

	// Assignee is a user uid, or SelfAssignee, or "" for anybody's.
	Assignee string

	Unassigned bool
	Overdue    bool
	Limit      int
}

// ResolveFilter resolves a request into the filter the store queries with,
// refusing a contradiction before it becomes a query that quietly matches
// nothing. The result carries an internal user id and never leaves this
// process (invariant 10).
func (s *Service) ResolveFilter(ctx context.Context, actor domain.Identity, req FilterRequest) (domain.DocumentFilter, error) {
	f := domain.DocumentFilter{
		State:      req.State,
		SiteID:     req.Site,
		Unassigned: req.Unassigned,
		Overdue:    req.Overdue,
		Limit:      req.Limit,
	}
	if req.Assignee != "" {
		if req.Unassigned {
			return domain.DocumentFilter{}, fmt.Errorf(
				"a document is assigned to somebody or to nobody, not both: %w", domain.ErrInvalid)
		}
		user, err := s.resolveUser(ctx, actor, req.Assignee)
		if err != nil {
			return domain.DocumentFilter{}, err
		}
		f.AssignedTo = user.ID
	}
	return f, f.Validate()
}

// resolveUser maps the name a caller used to a user.
//
// SelfAssignee is the caller, which is what makes one saved queue definition
// serve every editor and what lets a client filter by "mine" without a round
// trip to /api/v1/me first.
func (s *Service) resolveUser(ctx context.Context, actor domain.Identity, name string) (domain.User, error) {
	name = strings.TrimSpace(name)
	switch name {
	case "":
		return domain.User{}, fmt.Errorf("no user named: %w", domain.ErrInvalid)
	case SelfAssignee:
		return actor.User, nil
	}
	u, err := s.db.UserByUID(ctx, name)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return domain.User{}, fmt.Errorf("user %q: %w", name, domain.ErrNotFound)
		}
		return domain.User{}, err
	}
	return u, nil
}

// mayAssign loads a document and resolves the privilege assignment needs.
//
// It is mayEdit's rule without mayEdit's lease: the lease protects the working
// draft and this touches the document row. Reusing mayEdit's name for it would
// invite somebody to "simplify" the two into one call that took the lock.
func (s *Service) mayAssign(ctx context.Context, actor domain.Identity, uid string) (domain.Document, error) {
	return s.mayDo(ctx, actor, uid, domain.Edit)
}

// ListDocuments returns the documents the actor may read, matching a filter
// (PLAN.md M5 acceptance 1).
//
// The authorization filter is applied here rather than in SQL because the rule
// is authz.Resolve, a pure function over grants the caller already holds, and
// a second implementation of it in SQL is a second implementation that will
// disagree. What this must never become is a list that shows a row the reader
// may not open.
//
// The consequence is that a limit bounds what the store returns rather than
// what the caller sees: a page of a hundred rows can come back with eighty if
// twenty of them are somebody else's site. That is the honest trade for having
// one implementation of the rule, and it is the same trade M3 made.
func (s *Service) ListDocuments(ctx context.Context, actor domain.Identity, filter domain.DocumentFilter) ([]domain.Document, error) {
	if err := filter.Validate(); err != nil {
		return nil, err
	}
	all, err := s.db.QueryDocuments(ctx, store.DocumentQuery{Filter: filter, Now: s.Now()})
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
