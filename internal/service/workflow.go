// Copyright (c) 2026 Michael D Henderson.

package service

import (
	"context"

	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/workflow"
)

// The workflow use cases (PLAN.md M4): what may this document do next, and do
// one of those things.
//
// Both are two lines, and that is the point. The rule lives in
// internal/workflow, where Available and Do share one check function
// (invariant 5); this package resolves the uid, decides that the caller may
// read the document, and hands over. A second opinion about a transition
// formed here would be the disagreement the invariant exists to prevent.

// Transitions returns every transition out of the document's current state,
// each marked with whether the actor may perform it and why not.
//
// Read is the privilege it needs. Asking what you could do is not doing it,
// and a menu that only renders for people who could act on every entry is a
// menu that hides the process from everybody else -- which is how an editor
// learns the rules by being refused.
func (s *Service) Transitions(ctx context.Context, actor domain.Identity, uid string) (domain.Document, []workflow.Allowed, error) {
	doc, err := s.mayRead(ctx, actor, uid)
	if err != nil {
		return domain.Document{}, nil, err
	}
	allowed, err := s.engine.Available(ctx, doc, actor)
	return doc, allowed, err
}

// Transition performs one transition.
//
// The privilege the move needs is declared on the transition and checked by
// the engine against this document, so what is checked here is only that the
// caller may see the document at all: a 404 for somebody who may not, rather
// than a 403 that confirms it exists.
//
// Everything else -- the transition being declared, the guards, the effects,
// the event -- happens inside one transaction in the engine (DESIGN.md 6.4).
func (s *Service) Transition(ctx context.Context, actor domain.Identity, uid, to, note string) (DocumentView, error) {
	doc, err := s.mayRead(ctx, actor, uid)
	if err != nil {
		return DocumentView{}, err
	}

	moved, err := s.engine.Do(ctx, workflow.Request{
		Document: doc,
		To:       to,
		Note:     note,
		Actor:    actor,
	})
	if err != nil {
		return DocumentView{}, err
	}
	return s.viewOf(ctx, moved)
}

// Workflows returns every configured workflow with its states and
// transitions. It is what "cmsdb seed" reports and what an admin screen lists.
func (s *Service) Workflows(ctx context.Context) ([]domain.Workflow, error) {
	return s.db.ListWorkflows(ctx)
}
