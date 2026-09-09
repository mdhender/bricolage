// Copyright (c) 2026 Michael D Henderson.

package workflow

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mdhender/bricolage/internal/authz"
	"github.com/mdhender/bricolage/internal/clock"
	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/events"
	"github.com/mdhender/bricolage/internal/store"
)

// Engine moves documents between states (DESIGN.md 6).
//
// It holds a clock rather than reading one, because EffectSetDueIn computes a
// date and a due date nobody can set at an arbitrary instant is a due date
// nobody tests (invariant 3).
type Engine struct {
	db    *store.DB
	clock clock.Clock
}

// New builds an engine over an open database.
func New(db *store.DB, c clock.Clock) (*Engine, error) {
	if db == nil {
		return nil, fmt.Errorf("workflow: no database")
	}
	if c == nil {
		return nil, fmt.Errorf("workflow: no clock; every component that needs the time is given one (invariant 3)")
	}
	return &Engine{db: db, clock: c}, nil
}

// Allowed is one transition out of the current state, marked with whether the
// actor may perform it and why not (DESIGN.md 6.2).
//
// Returning refused transitions with reasons is deliberate: the UI can grey
// out "Approve" and say "needs one more approval", which is far better than
// the action silently not existing. An action that vanishes teaches nobody
// anything, and it is what sends an editor to an administrator.
type Allowed struct {
	Transition domain.Transition

	// Reason is why not, when Permitted is false. It is written for the
	// person reading it rather than for the code that produced it.
	Reason string

	// Guard names the guard that refused, when a guard did. It is empty when
	// the refusal was a privilege rather than a guard: those are different
	// answers -- "you may not" against "not yet" -- and a UI that renders them
	// the same way is a UI that tells an editor to go and ask for permission
	// they already have.
	Guard domain.Guard

	Permitted bool

	// NeedsNote reports that the transition declares GuardNoteRequired, so a
	// client should offer a note field rather than treating the refusal as
	// final. Available is asked before anybody has typed one, so a
	// note-requiring transition comes back refused; this is how the UI knows
	// the refusal is a prompt.
	NeedsNote bool
}

// Request is one transition somebody is asking to perform.
type Request struct {
	// Document is the document to move. It is loaded by the caller, which has
	// already resolved the uid and checked that the actor may read it.
	Document domain.Document

	// To is the state to move to.
	To string

	// Note is the check-in-style message the transition carries. It is what
	// GuardNoteRequired demands and what the event payload records.
	Note string

	// Actor is who is asking. It carries grants as well as a user, because a
	// transition declares the privilege it needs and resolving one takes the
	// caller's grants (DESIGN.md 7.2).
	Actor domain.Identity
}

// facts is everything check reads: the document, the version that has a slug
// and a cover date, the counters the two data-driven guards need, the actor,
// the instant, and the note.
//
// It exists so that Available and Do decide from the same information. The
// store loads it once, in one query set, and hands the same struct to both
// paths; a second loader would be the beginning of the disagreement invariant 5
// exists to prevent.
type facts struct {
	store.TransitionFacts

	// State is the definition of the state being left, which is where
	// RequiredApprovals lives.
	State domain.WorkflowState

	Actor domain.Identity
	Now   time.Time

	// Note is the note the request carries. Available passes the empty string,
	// because it is asked before anybody has typed one.
	Note string
}

// Available returns every transition out of the document's current state, each
// marked with whether the actor may perform it and why not (DESIGN.md 6.2).
//
// The UI renders from this. Do re-runs the identical checks against facts
// reloaded inside its transaction, which is why the two cannot disagree about
// anything but a change somebody else committed in between.
func (e *Engine) Available(ctx context.Context, doc domain.Document, actor domain.Identity) ([]Allowed, error) {
	w, err := e.db.WorkflowByID(ctx, doc.WorkflowID)
	if err != nil {
		return nil, err
	}
	loaded, err := e.db.TransitionFactsFor(ctx, doc)
	if err != nil {
		return nil, err
	}
	f, err := newFacts(w, loaded, actor, e.clock.Now().UTC(), "")
	if err != nil {
		return nil, err
	}

	out := make([]Allowed, 0, len(w.Transitions))
	for _, t := range w.From(f.Document.State) {
		a := Allowed{Transition: t, Permitted: true, NeedsNote: t.NeedsNote()}
		if err := check(t, f); err != nil {
			a.Permitted = false
			a.Reason = reasonOf(err)
			a.Guard, _ = domain.GuardOf(err)
		}
		out = append(out, a)
	}
	return out, nil
}

// Do performs one transition (DESIGN.md 6.4): load, check, update, event, in
// one transaction.
//
// The check runs inside the transaction, against facts read there, so the
// document a guard was checked against is the document the write lands on. A
// failed guard returns before anything is written and the transaction rolls
// back with the state, the events, and the jobs unchanged (PLAN.md M4
// acceptance 6).
//
// Alert evaluation happens after commit, driven off the event row, so a
// failing notification cannot roll back an editorial action (DESIGN.md 6.4).
// The alert engine is M12; nothing here waits for it.
func (e *Engine) Do(ctx context.Context, req Request) (domain.Document, error) {
	w, err := e.db.WorkflowByID(ctx, req.Document.WorkflowID)
	if err != nil {
		return domain.Document{}, err
	}
	now := e.clock.Now().UTC()

	return e.db.ApplyTransition(ctx, store.TransitionRequest{
		DocumentID: req.Document.ID,
		Now:        now,
		Decide: func(loaded store.TransitionFacts) (store.TransitionOutcome, error) {
			// The transition is resolved from the state read inside the
			// transaction, not from the one the caller saw. A document that
			// moved while the request was in flight is a document whose menu
			// is stale, and the answer is that the move is not declared from
			// where it now is.
			t, err := w.Transition(loaded.Document.State, req.To)
			if err != nil {
				return store.TransitionOutcome{}, err
			}
			f, err := newFacts(w, loaded, req.Actor, now, req.Note)
			if err != nil {
				return store.TransitionOutcome{}, err
			}
			if err := check(t, f); err != nil {
				return store.TransitionOutcome{}, err
			}
			return outcome(t, f)
		},
	})
}

// newFacts assembles what check reads and refuses a document whose state its
// workflow does not declare.
//
// That refusal cannot happen through this system -- the composite foreign key
// on documents makes it impossible -- which is exactly why it is worth saying
// out loud here rather than dereferencing a state that is not there.
func newFacts(w domain.Workflow, loaded store.TransitionFacts, actor domain.Identity, now time.Time, note string) (facts, error) {
	state, ok := w.State(loaded.Document.State)
	if !ok {
		return facts{}, fmt.Errorf("document %s is in state %q, which workflow %q does not declare: %w",
			loaded.Document.UID, loaded.Document.State, w.Name, domain.ErrConflict)
	}
	return facts{TransitionFacts: loaded, State: state, Actor: actor, Now: now, Note: note}, nil
}

// check is the one function Available and Do share (invariant 5,
// DESIGN.md 6.2).
//
// It returns nil when the actor may perform the transition and an error saying
// why not otherwise. A privilege refusal wraps domain.ErrForbidden; a guard
// refusal is a *domain.GuardError naming the guard, which the transport edge
// turns into a 409 carrying that name.
//
// The privilege is checked first and the guards in the order the transition
// declares them, so the reason a person is given is stable rather than
// depending on map iteration.
func check(t domain.Transition, f facts) error {
	if !authz.Allows(f.Actor.Grants, f.Document.Subject(), t.Privilege) {
		return &privilegeError{
			Transition: t.Name,
			Reason:     fmt.Sprintf("%s is required over this document", t.Privilege),
		}
	}
	for _, g := range t.Guards {
		if err := checkGuard(g, t, f); err != nil {
			return err
		}
	}
	return nil
}

// checkGuard is the closed vocabulary, enforced. Every guard a transition may
// declare has a case here, and a name with no case is refused when the row is
// read (domain.ParseGuards) rather than ignored when it runs.
func checkGuard(g domain.Guard, t domain.Transition, f facts) error {
	switch g {
	case domain.GuardNoteRequired:
		if f.Note == "" {
			return g.Refused(t.Name, "a note is required")
		}

	case domain.GuardAssigneeOnly:
		// An unassigned document passes: nobody has been given the work, so
		// anybody holding the privilege may take the step.
		if f.Document.AssignedTo != 0 && f.Document.AssignedTo != f.Actor.User.ID {
			return g.Refused(t.Name, "this document is assigned to somebody else")
		}

	case domain.GuardApprovalsMet:
		// Approvals of the current version only. Adding a version drops the
		// count to zero, which is what makes changes invalidate sign-off
		// (DESIGN.md 5.5, PLAN.md M4 acceptance 7).
		have := f.ApprovalsByState[f.State.Slug]
		if want := f.State.RequiredApprovals; have < want {
			return g.Refused(t.Name, fmt.Sprintf("not enough approvals: %d of %d", have, want))
		}

	case domain.GuardNotLocked:
		if f.Document.Lock.Held(f.Now) {
			return g.Refused(t.Name, "this document is checked out")
		}

	case domain.GuardCommentsResolved:
		if n := f.UnresolvedComments; n > 0 {
			return g.Refused(t.Name, fmt.Sprintf("%s unresolved", plural(n, "comment")))
		}

	case domain.GuardHasSlug:
		if f.Version.Slug == "" {
			return g.Refused(t.Name, "this document has no slug")
		}

	case domain.GuardHasCoverDate:
		if f.Version.CoverDate == "" {
			return g.Refused(t.Name, "this document has no cover date")
		}

	default:
		// Unreachable through the store, which parses guards against the
		// vocabulary and refuses an unknown name. It is here so that adding a
		// constant without a case fails closed rather than silently allowing
		// the transition it was meant to guard (invariant 6).
		return fmt.Errorf("guard %q is configured on transition %s and this binary does not enforce it: %w",
			g, t.Name, domain.ErrInvalid)
	}
	return nil
}

// outcome applies the transition's effects and builds the event.
//
// Effects are computed here and written by the store in the same statement as
// the state, so a document is never briefly in review while still assigned to
// whoever submitted it.
func outcome(t domain.Transition, f facts) (store.TransitionOutcome, error) {
	// A transition declaring two contradictory assignment effects is refused
	// when its row is read, not here (domain.Transition.Validate), so the
	// order the two are applied in below is never load-bearing.
	out := store.TransitionOutcome{State: t.To}

	if t.HasEffect(domain.EffectClearAssignee) {
		out.SetAssignee = domain.Ref(int64(0))
	}
	if t.HasEffect(domain.EffectAssignToActor) {
		out.SetAssignee = domain.Ref(f.Actor.User.ID)
	}
	if d, ok, err := t.DueIn(); err != nil {
		return store.TransitionOutcome{}, err
	} else if ok {
		out.SetDueAt = domain.Ref(f.Now.Add(d))
	}
	out.ClearApprovals = t.HasEffect(domain.EffectClearApprovals)

	// The payload carries enough to reconstruct what happened (invariant 7):
	// where it went from and to, which transition, who, what they said, and
	// which guards were satisfied to let it through.
	payload := map[string]any{
		"uid":        f.Document.UID,
		"transition": t.Name,
		"from":       t.From,
		"to":         t.To,
		"guards":     t.GuardNames(),
		"effects":    t.EffectNames(),
	}
	if f.Note != "" {
		payload["note"] = f.Note
	}
	if out.SetDueAt != nil {
		payload["due_at"] = *out.SetDueAt
	}
	out.Event = domain.Event{
		Type:       events.DocumentTransitioned,
		ActorID:    f.Actor.User.ID,
		Payload:    payload,
		OccurredAt: f.Now,
	}
	return out, nil
}

// privilegeError is a transition refused because the actor does not hold the
// privilege it declares.
//
// It is a type rather than a wrapped fmt.Errorf so that Allowed.Reason can be
// the sentence a person reads -- "create is required over this document" --
// rather than the whole error chain with ": forbidden" on the end. earl prints
// Reason verbatim in a column, and an error's own message is written for
// whoever is debugging it.
//
// It answers to ErrForbidden, so the one mapping function at the transport edge
// turns it into a 403 with no new case, and it is deliberately not a
// GuardError: a guard is a statement about the document and a 409, a privilege
// is a statement about the person and a 403. Rendering them the same way sends
// an editor to an administrator for something no administrator can fix.
type privilegeError struct {
	Transition string
	Reason     string
}

func (e *privilegeError) Error() string {
	return fmt.Sprintf("%s: %s: %v", e.Transition, e.Reason, domain.ErrForbidden)
}

func (e *privilegeError) Unwrap() error { return domain.ErrForbidden }

// reasonOf renders a refusal for a person.
//
// The two refusals a check can produce each carry their own sentence; anything
// else is a failure rather than a refusal, and its own message is the best
// thing to show.
func reasonOf(err error) string {
	var ge *domain.GuardError
	if errors.As(err, &ge) {
		return ge.Reason
	}
	var pe *privilegeError
	if errors.As(err, &pe) {
		return pe.Reason
	}
	return err.Error()
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
