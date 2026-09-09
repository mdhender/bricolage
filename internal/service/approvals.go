// Copyright (c) 2026 Michael D Henderson.

package service

import (
	"context"
	"fmt"

	"github.com/mdhender/bricolage/internal/authz"
	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/events"
	"github.com/mdhender/bricolage/internal/store"
)

// The approval use cases (PLAN.md M11): sign off on a version, and take the
// sign-off back.
//
// An approval is not a transition. It records that one person, in one state,
// signed off on one version, and GuardApprovalsMet counts them when a move out
// of that state is attempted. Three rules shape everything below, and all
// three come from the design rather than from this package:
//
//   - The privilege is the process's, not this method's. An approval exists to
//     satisfy a guard on some transition out of the state the document is in,
//     so the person whose sign-off counts is the person that transition would
//     let make the move (domain.Workflow.ApprovalPrivilege).
//   - The version must be checked in. Approvals attach to a version so that
//     changes invalidate sign-off, and a working draft mutates in place --
//     approving one would sign off on content that is still being written, and
//     the version id that is supposed to expire the approval would never
//     change.
//   - Both operations are idempotent. Approving twice is one row and one event;
//     withdrawing an approval that is not there is not an error.

// ApprovalView is what an approval operation and the approvals listing return:
// the document, the state the approvals are counted in, how many the process
// wants, and the rows themselves.
//
// Required travels with the count because "2 of 2" is the sentence a client
// puts beside an Approve button, and computing it from two round trips is how
// the two disagree.
type ApprovalView struct {
	Document domain.Document

	// Version is the version the approvals below are about.
	Version domain.Version

	// State is the state the document is in now, which is the state
	// GuardApprovalsMet will count approvals of when it is next asked.
	State string

	// Current reports whether Version is the version being looked at now.
	//
	// It is what makes the count mean two different things honestly. For the
	// current version the question is the guard's -- how many approvals of
	// this version, in this state -- and the answer is compared against
	// Required. For an older version there is no live question to ask: the
	// document has moved on, and what is worth reporting is the rows that
	// were recorded, whichever state each was given in.
	Current bool

	// Required is what the current state's required_approvals demands.
	Required int

	// Approvals are the rows, oldest first.
	Approvals []domain.Approval

	// Changed reports whether the request that produced this view actually
	// wrote something. A second approval by the same person, and a withdrawal
	// of an approval nobody recorded, both report false.
	Changed bool
}

// Count is how many approvals this view reports: for the current version, the
// number GuardApprovalsMet compares against Required; for an older one, every
// approval it ever collected.
func (v ApprovalView) Count() int {
	if !v.Current {
		return len(v.Approvals)
	}
	return domain.CountApprovalsIn(v.Approvals, v.State)
}

// Met reports whether the guard would be satisfied. An older version has no
// live question to answer, so it is never met: the document is not going to
// move on the strength of a version it has left behind.
func (v ApprovalView) Met() bool { return v.Current && v.Count() >= v.Required }

// Approvals returns the approvals recorded against one version of a document,
// with the count the current state demands.
//
// version is a version number, or 0 for the one being looked at now. An older
// version's approvals stay queryable after a check-in has dropped the live
// count to zero (PLAN.md M11 acceptance 3): who signed off on what is a fact
// about last Tuesday, and a counter that resets is not a reason to forget it.
func (s *Service) Approvals(ctx context.Context, actor domain.Identity, uid string, version int) (ApprovalView, error) {
	doc, err := s.mayRead(ctx, actor, uid)
	if err != nil {
		return ApprovalView{}, err
	}

	view, err := s.approvalView(ctx, doc, version)
	if err != nil {
		return ApprovalView{}, err
	}
	view.Approvals, err = s.db.ApprovalsForVersion(ctx, view.Version.ID)
	return view, err
}

// Approve records the actor's sign-off on the document's current version, in
// the state it is in.
//
// Approving twice is idempotent: not an error, and not two rows
// (PLAN.md M11 acceptance 1). The second call returns the first call's
// approval, with Changed false and no second event.
func (s *Service) Approve(ctx context.Context, actor domain.Identity, uid string) (ApprovalView, error) {
	doc, view, err := s.mayApprove(ctx, actor, uid)
	if err != nil {
		return ApprovalView{}, err
	}

	now := s.Now()
	_, created, err := s.db.CreateApproval(ctx, store.NewApproval{
		DocumentID: doc.ID,
		VersionID:  view.Version.ID,
		State:      doc.State,
		UserID:     actor.User.ID,
		CreatedAt:  now,
	}, domain.Event{
		Type:    events.DocumentApproved,
		ActorID: actor.User.ID,
		Payload: map[string]any{
			"uid":        doc.UID,
			"state":      doc.State,
			"version":    view.Version.Number,
			"version_id": view.Version.ID,
		},
		OccurredAt: now,
	})
	if err != nil {
		return ApprovalView{}, err
	}
	view.Changed = created
	view.Approvals, err = s.db.ApprovalsForVersion(ctx, view.Version.ID)
	return view, err
}

// WithdrawApproval removes the actor's own sign-off of the current version in
// the current state.
//
// It is the actor's own and nobody else's. An approval is a statement by one
// person, and a system in which somebody else can take yours back is a system
// whose approval count means nothing; an editor who disagrees moves the
// document instead, and EffectClearApprovals is how a process discards them
// all at once.
//
// Withdrawing an approval that is not there is not an error, for the reason
// DELETE is idempotent: the caller means "make sure my approval is not there",
// and it is not.
func (s *Service) WithdrawApproval(ctx context.Context, actor domain.Identity, uid string) (ApprovalView, error) {
	doc, view, err := s.mayApprove(ctx, actor, uid)
	if err != nil {
		return ApprovalView{}, err
	}

	now := s.Now()
	removed, err := s.db.WithdrawApproval(ctx, doc.ID, view.Version.ID, doc.State, actor.User.ID, domain.Event{
		Type:    events.DocumentApprovalWithdrawn,
		ActorID: actor.User.ID,
		Payload: map[string]any{
			"uid":        doc.UID,
			"state":      doc.State,
			"version":    view.Version.Number,
			"version_id": view.Version.ID,
		},
		OccurredAt: now,
	})
	if err != nil {
		return ApprovalView{}, err
	}
	view.Changed = removed
	view.Approvals, err = s.db.ApprovalsForVersion(ctx, view.Version.ID)
	return view, err
}

// mayApprove resolves everything the two writers above share: the document,
// the version the approval attaches to, and whether this actor may record one
// while the document is where it is.
//
// The three refusals are three different answers and the transport edge turns
// them into three different statuses. A state whose process counts no
// approvals is a 409 -- a statement about the document, like a transition the
// state machine does not declare -- because no grant would change it. A
// working draft is a 409 for the same reason. Not holding the privilege the
// process asks for is a 403, which is a statement about the person.
func (s *Service) mayApprove(ctx context.Context, actor domain.Identity, uid string) (domain.Document, ApprovalView, error) {
	doc, err := s.mayRead(ctx, actor, uid)
	if err != nil {
		return domain.Document{}, ApprovalView{}, err
	}
	wf, err := s.db.WorkflowByID(ctx, doc.WorkflowID)
	if err != nil {
		return domain.Document{}, ApprovalView{}, err
	}

	want, ok := wf.ApprovalPrivilege(doc.State)
	if !ok {
		return domain.Document{}, ApprovalView{}, fmt.Errorf(
			"document %s is in %q, and no transition out of it counts approvals in workflow %q: %w",
			uid, doc.State, wf.Name, domain.ErrConflict)
	}
	if !authz.Allows(actor.Grants, doc.Subject(), want) {
		return domain.Document{}, ApprovalView{}, fmt.Errorf(
			"approving document %s in %q: %s is required, because that is what moving it out of %q asks for: %w",
			uid, doc.State, want, doc.State, domain.ErrForbidden)
	}

	view, err := s.approvalView(ctx, doc, 0)
	if err != nil {
		return domain.Document{}, ApprovalView{}, err
	}
	if view.Version.ID == 0 || view.Version.IsDraft() {
		// Approvals attach to a version so that a change invalidates the
		// sign-off. A working draft is edited in place, so an approval of one
		// would still be attached after the content had changed underneath
		// it -- the version id, which is the whole mechanism, would not move.
		return domain.Document{}, ApprovalView{}, fmt.Errorf(
			"document %s has no checked-in version to approve; its current version is still an open working draft: %w",
			uid, domain.ErrConflict)
	}
	return doc, view, nil
}

// approvalView assembles the document, the version, and what the current state
// demands. version is a version number, or 0 for the current one.
func (s *Service) approvalView(ctx context.Context, doc domain.Document, version int) (ApprovalView, error) {
	view := ApprovalView{Document: doc, State: doc.State}

	if version > 0 {
		v, err := s.db.VersionByNumber(ctx, doc.ID, version)
		if err != nil {
			return ApprovalView{}, err
		}
		view.Version = v
	} else {
		got, err := s.viewOf(ctx, doc)
		if err != nil {
			return ApprovalView{}, err
		}
		view.Version = got.Version
	}
	view.Current = view.Version.ID != 0 && view.Version.ID == doc.CurrentVersionID

	// Required comes from the state the document is in now, whichever version
	// is being listed: it is what the process demands before this document
	// moves, and a historical version has no process of its own.
	wf, err := s.db.WorkflowByID(ctx, doc.WorkflowID)
	if err != nil {
		return ApprovalView{}, err
	}
	if st, ok := wf.State(doc.State); ok {
		view.Required = st.RequiredApprovals
	}
	return view, nil
}
