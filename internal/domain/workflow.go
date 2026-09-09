// Copyright (c) 2026 Michael D Henderson.

package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

// The workflow vocabulary (DESIGN.md 6.1).
//
// DESIGN.md 6.1 shows these types under internal/workflow, and the engine --
// Engine, Available, Do, and the check they share -- is there. The types
// themselves are here because internal/store converts rows to domain types and
// internal/workflow imports internal/store: a Transition declared in the
// engine's package could never be scanned out of workflow_transitions without
// inverting the dependency graph. They are pure data with pure functions over
// them, which is what this package is for, and the split leaves invariant 5
// exactly where it was -- one check function, in the engine.

// Guard is a precondition a transition declares (DESIGN.md 6.1).
//
// The set is closed. Transitions are data so that an editorial process can be
// configured, and guards are a closed vocabulary so that every guard which can
// be configured is one the engine actually enforces.
//
// The rule this exists to prevent: the system we learned from shipped
// desk.pre_chk_rules and desk.post_chk_rules -- columns with foreign keys,
// indexes, an accessor API, and a comment reading "Do the pre-desk rule
// checks" above a call that checked nothing. Nothing in its entire tree ever
// read them. A rule column that nothing implements is worse than no column,
// because it lies to whoever reads the schema (invariant 6).
type Guard string

const (
	// GuardNoteRequired refuses a transition performed without a note. It is
	// the one guard that is a property of the request rather than of the
	// document, which is why Available reports it refused until a note is
	// offered and marks the transition as needing one.
	GuardNoteRequired Guard = "note_required"

	// GuardAssigneeOnly refuses anybody but the document's assignee.
	//
	// An unassigned document passes: nobody has been given the work, so
	// anybody holding the privilege may take the step. The alternative --
	// refusing until somebody is assigned -- makes the guard unusable in any
	// workflow whose first transition it appears on, which is every workflow
	// that would want it.
	GuardAssigneeOnly Guard = "assignee_only"

	// GuardApprovalsMet refuses until the current version carries as many
	// approvals in the state being left as that state requires.
	//
	// The count is of the current version only (DESIGN.md 5.5): edit the
	// document, a new version exists, and the old approvals no longer satisfy
	// the guard. Getting "changes invalidate sign-off" right costs one foreign
	// key.
	GuardApprovalsMet Guard = "approvals_met"

	// GuardNotLocked refuses while somebody holds a live edit lease. An
	// expired lease is not a lock (DESIGN.md 5.1).
	GuardNotLocked Guard = "not_locked"

	// GuardCommentsResolved refuses while the document carries an unresolved
	// comment.
	GuardCommentsResolved Guard = "comments_resolved"

	// GuardHasSlug refuses a current version with no slug.
	GuardHasSlug Guard = "has_slug"

	// GuardHasCoverDate refuses a current version with no cover date.
	GuardHasCoverDate Guard = "has_cover_date"

	// GuardHasCheckedInVersion refuses a document that has never been checked
	// in (PLAN.md M9).
	//
	// It is the eighth guard and it arrived with EffectPublish, because the
	// two together are what keeps invariant 5 true. A publish pins a
	// checked-in version (invariant 8) and a document whose only version is
	// its first draft has none, so without this the engine would have to
	// refuse while applying the effect -- after check had already said yes,
	// which is exactly the disagreement between the menu and the action that
	// invariant 5 exists to make unrepresentable.
	//
	// It reads the newest checked-in version rather than the current one. The
	// two differ while somebody holds the document checked out, and that case
	// is not a refusal: a document with three checked-in versions and an open
	// draft has something to publish.
	GuardHasCheckedInVersion Guard = "has_checked_in_version"
)

// Guards are the eight, in the order the admin screens list them.
var Guards = []Guard{
	GuardNoteRequired,
	GuardAssigneeOnly,
	GuardApprovalsMet,
	GuardNotLocked,
	GuardCommentsResolved,
	GuardHasSlug,
	GuardHasCoverDate,
	GuardHasCheckedInVersion,
}

// Valid reports whether g is one of the eight.
func (g Guard) Valid() bool { return slices.Contains(Guards, g) }

func (g Guard) String() string { return string(g) }

// Effect is a change a transition makes beyond the state itself
// (DESIGN.md 6.1). Like Guard, the set is closed.
type Effect string

const (
	// EffectClearAssignee unassigns the document.
	EffectClearAssignee Effect = "clear_assignee"

	// EffectAssignToActor gives the document to whoever performed the
	// transition.
	EffectAssignToActor Effect = "assign_to_actor"

	// EffectClearApprovals discards the approvals recorded against the
	// current version, so that a document sent back for changes has to be
	// signed off again.
	EffectClearApprovals Effect = "clear_approvals"

	// EffectSetDueIn sets the due date to a duration from now. It is the one
	// parameterised effect: its value is a Go duration string, "48h".
	EffectSetDueIn Effect = "set_due_in"

	// EffectPublish schedules a publish of the document's current checked-in
	// version, in the same transaction as the move (DESIGN.md 6.4, step four,
	// and PLAN.md M9, "Transition effect: publishing from a publishable
	// state").
	//
	// It is the fifth effect and the one that reaches outside the document
	// row. The job it enqueues pins a version id and never a document id
	// (invariant 8), so a transition into "published" publishes what was
	// approved rather than whatever the draft has become by the time a worker
	// picks the job up.
	//
	// A transition declaring it must enter a state whose Publishable is set;
	// Workflow.Validate refuses one that does not. Publishing out of a state
	// the process does not call publishable is a workflow that contradicts
	// itself, and the contradiction is worth refusing when the row is read
	// rather than discovering at the scheduled hour.
	EffectPublish Effect = "publish"
)

// Effects are the five, in the order the admin screens list them.
var Effects = []Effect{
	EffectClearAssignee,
	EffectAssignToActor,
	EffectClearApprovals,
	EffectSetDueIn,
	EffectPublish,
}

// Valid reports whether e is one of the five.
func (e Effect) Valid() bool { return slices.Contains(Effects, e) }

func (e Effect) String() string { return string(e) }

// Parameterised reports whether the effect carries a value. Only
// EffectSetDueIn does; the others are named by their presence.
func (e Effect) Parameterised() bool { return e == EffectSetDueIn }

// GuardError is a transition refused by a guard (DESIGN.md 12).
//
// It carries the guard's name so that the problem document can name it in the
// "guard" extension member and a client can act on the specific refusal rather
// than parsing prose out of "detail". It answers to ErrGuardFailed, which is a
// 409 at the edge: a guard is a statement about the document, not about the
// person, which is why it is not ErrForbidden.
type GuardError struct {
	// Guard is the guard that refused.
	Guard Guard

	// Reason is what to tell the person: "needs one more approval", not
	// "approvals_met returned false".
	Reason string

	// Transition names the move that was refused, for the message.
	Transition string
}

func (e *GuardError) Error() string {
	if e.Transition != "" {
		return fmt.Sprintf("%s: %s (%s)", e.Transition, e.Reason, e.Guard)
	}
	return fmt.Sprintf("%s (%s)", e.Reason, e.Guard)
}

// Unwrap makes a guard refusal answer to ErrGuardFailed, so the one mapping
// function at the transport edge turns it into a 409 with no new case.
func (e *GuardError) Unwrap() error { return ErrGuardFailed }

// Refused builds a refusal from a guard.
func (g Guard) Refused(transition, reason string) error {
	return &GuardError{Guard: g, Reason: reason, Transition: transition}
}

// GuardOf returns the guard that refused, and whether err was a refusal at
// all. It is how the transport edge fills in the problem document's "guard"
// member without knowing anything else about the error.
func GuardOf(err error) (Guard, bool) {
	var ge *GuardError
	if errors.As(err, &ge) {
		return ge.Guard, true
	}
	return "", false
}

// Workflow is one editorial process: its states and the transitions between
// them (DESIGN.md 5.4).
type Workflow struct {
	ID  int64
	UID string

	// SiteID is 0 for a workflow that applies to every site.
	SiteID int64

	// Kind is the document kind this workflow governs.
	Kind string

	Name string

	// InitialState is the state a new document enters.
	InitialState string

	// States are the workflow's states, in position order.
	States []WorkflowState

	// Transitions are every declared move, in position order.
	Transitions []Transition
}

// WorkflowState is one state of a workflow (DESIGN.md 5.4).
type WorkflowState struct {
	WorkflowID int64
	Slug       string
	Name       string

	// Position is the order the state appears in. It is the column the system
	// we learned from never had: its desk ordering was derived by sorting
	// start-first, publish-last and everything else by primary key, so "the
	// order of the desks" was three buckets.
	Position int

	// Publishable reports whether a document in this state may be published.
	Publishable bool

	// Terminal reports whether this state is an end of the process. Leaving
	// one is what the Recall privilege is for.
	Terminal bool

	// RequiredApprovals is how many distinct approvals of the current version
	// GuardApprovalsMet demands before a document may leave this state.
	RequiredApprovals int
}

// Transition is one declared move between two states (DESIGN.md 5.4).
//
// Nothing outside a declared transition ever changes a document's state, and
// the API models transitions as a subresource for that reason: GET says what
// the state machine permits and why, POST performs one. There is deliberately
// no PATCH that sets state.
type Transition struct {
	ID         int64
	WorkflowID int64

	From string
	To   string
	Name string

	// Privilege is what the actor must hold over the document to make this
	// move.
	Privilege Privilege

	// Guards are the preconditions, in the order they are checked.
	Guards []Guard

	// Effects are the changes beyond the state itself, keyed by effect. The
	// value is the parameter, empty for every effect but EffectSetDueIn.
	Effects map[Effect]string

	Position int
}

// NeedsNote reports whether this transition declares GuardNoteRequired.
//
// The UI reads it to offer a note field rather than greying the action out,
// which is the difference between "you may not do this" and "tell me why".
func (t Transition) NeedsNote() bool { return slices.Contains(t.Guards, GuardNoteRequired) }

// DueIn returns the duration EffectSetDueIn carries, and whether the
// transition declares it.
func (t Transition) DueIn() (time.Duration, bool, error) {
	raw, ok := t.Effects[EffectSetDueIn]
	if !ok {
		return 0, false, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, true, fmt.Errorf("transition %s: %s is %q, which is not a duration: %w",
			t.Name, EffectSetDueIn, raw, ErrInvalid)
	}
	if d <= 0 {
		return 0, true, fmt.Errorf("transition %s: %s is %q, which is not in the future: %w",
			t.Name, EffectSetDueIn, raw, ErrInvalid)
	}
	return d, true, nil
}

// HasEffect reports whether the transition declares e.
func (t Transition) HasEffect(e Effect) bool {
	_, ok := t.Effects[e]
	return ok
}

// Validate reports whether a transition is one the engine will run.
//
// This is invariant 6 made mechanical. A guard or an effect naming something
// outside the vocabulary is refused when the row is read, not ignored: a
// configured guard the engine does not know is a rule that silently does
// nothing, which is the whole failure this design exists to avoid.
func (t Transition) Validate() error {
	if t.From == "" || t.To == "" {
		return fmt.Errorf("transition %q: needs a from state and a to state: %w", t.Name, ErrInvalid)
	}
	if !t.Privilege.Valid() {
		return fmt.Errorf("transition %s: privilege %d is not on the scale: %w",
			t.Name, uint8(t.Privilege), ErrInvalid)
	}
	for _, g := range t.Guards {
		if !g.Valid() {
			return fmt.Errorf("transition %s: %q is not a guard this engine enforces (%s): %w",
				t.Name, g, guardNames(), ErrInvalid)
		}
	}
	for e, param := range t.Effects {
		if !e.Valid() {
			return fmt.Errorf("transition %s: %q is not an effect this engine applies (%s): %w",
				t.Name, e, effectNames(), ErrInvalid)
		}
		if !e.Parameterised() && param != "" {
			return fmt.Errorf("transition %s: effect %s takes no parameter, got %q: %w",
				t.Name, e, param, ErrInvalid)
		}
	}
	if t.HasEffect(EffectClearAssignee) && t.HasEffect(EffectAssignToActor) {
		return fmt.Errorf("transition %s declares both %s and %s, which contradict: %w",
			t.Name, EffectClearAssignee, EffectAssignToActor, ErrInvalid)
	}
	if _, _, err := t.DueIn(); err != nil {
		return err
	}
	return nil
}

// State returns the named state of the workflow.
func (w Workflow) State(slug string) (WorkflowState, bool) {
	for _, s := range w.States {
		if s.Slug == slug {
			return s, true
		}
	}
	return WorkflowState{}, false
}

// From returns every transition out of a state, in position order.
//
// It is the list GET /api/v1/documents/{uid}/transitions renders, including
// the ones the caller may not make: the UI can grey out "Approve" and say
// "needs one more approval", which is far better than the action silently not
// existing.
func (w Workflow) From(state string) []Transition {
	var out []Transition
	for _, t := range w.Transitions {
		if t.From == state {
			out = append(out, t)
		}
	}
	return out
}

// Transition returns the declared move from one state to another.
//
// A move that is not declared is not a move, and no privilege changes that:
// an administrator asking to go from draft straight to published is asking for
// something the state machine does not contain, and the answer is ErrConflict
// rather than a permission check they would pass.
func (w Workflow) Transition(from, to string) (Transition, error) {
	for _, t := range w.Transitions {
		if t.From == from && t.To == to {
			return t, nil
		}
	}
	return Transition{}, fmt.Errorf("%q is not a declared transition from %q in workflow %q: %w",
		to, from, w.Name, ErrConflict)
}

// Validate reports whether a workflow is one the engine will run: an initial
// state it declares, and transitions between states it declares.
func (w Workflow) Validate() error {
	if len(w.States) == 0 {
		return fmt.Errorf("workflow %q: has no states: %w", w.Name, ErrInvalid)
	}
	if _, ok := w.State(w.InitialState); !ok {
		return fmt.Errorf("workflow %q: the initial state %q is not one of its states: %w",
			w.Name, w.InitialState, ErrInvalid)
	}
	for _, t := range w.Transitions {
		if err := t.Validate(); err != nil {
			return err
		}
		if _, ok := w.State(t.From); !ok {
			return fmt.Errorf("workflow %q: transition %s leaves %q, which is not one of its states: %w",
				w.Name, t.Name, t.From, ErrInvalid)
		}
		to, ok := w.State(t.To)
		if !ok {
			return fmt.Errorf("workflow %q: transition %s enters %q, which is not one of its states: %w",
				w.Name, t.Name, t.To, ErrInvalid)
		}
		// The one cross-check between a transition and a state, and the
		// reason Validate needs the whole workflow rather than the transition
		// alone. A transition that publishes into a state the process does
		// not call publishable is a process contradicting itself, and the
		// contradiction is refused when the row is read rather than found out
		// at the scheduled hour (PLAN.md M9).
		if t.HasEffect(EffectPublish) {
			if !to.Publishable {
				return fmt.Errorf("workflow %q: transition %s declares %s and enters %q, which is not a publishable state: %w",
					w.Name, t.Name, EffectPublish, t.To, ErrInvalid)
			}
			// The pairing that keeps invariant 5 true. A publish pins a
			// checked-in version and a document that has never been checked
			// in has none, so a transition that could publish must be able to
			// refuse before check says yes -- which means declaring the guard
			// that asks. Without this rule the engine would have to refuse
			// while applying the effect, and Available would offer a move Do
			// rejects.
			if !slices.Contains(t.Guards, GuardHasCheckedInVersion) {
				return fmt.Errorf("workflow %q: transition %s declares %s and must also declare the %s guard, or it would offer a move the engine refuses: %w",
					w.Name, t.Name, EffectPublish, GuardHasCheckedInVersion, ErrInvalid)
			}
		}
	}
	return nil
}

// ParseGuards reads the guards column: a JSON array of names.
//
// An unknown name is an error rather than a skipped entry. That is the point
// of the closed vocabulary: a database configured with a guard this binary
// does not enforce must refuse to run the transition, not run it unguarded.
func ParseGuards(raw string) ([]Guard, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var names []string
	if err := json.Unmarshal([]byte(raw), &names); err != nil {
		return nil, fmt.Errorf("guards %q: not a JSON array of names: %v: %w", raw, err, ErrInvalid)
	}
	out := make([]Guard, 0, len(names))
	for _, n := range names {
		g := Guard(n)
		if !g.Valid() {
			return nil, fmt.Errorf("guard %q: not one this engine enforces (%s): %w", n, guardNames(), ErrInvalid)
		}
		out = append(out, g)
	}
	return out, nil
}

// ParseEffects reads the effects column: a JSON object of name to parameter.
func ParseEffects(raw string) (map[Effect]string, error) {
	if strings.TrimSpace(raw) == "" {
		return map[Effect]string{}, nil
	}
	var names map[string]string
	if err := json.Unmarshal([]byte(raw), &names); err != nil {
		return nil, fmt.Errorf("effects %q: not a JSON object of name to parameter: %v: %w", raw, err, ErrInvalid)
	}
	out := make(map[Effect]string, len(names))
	for n, param := range names {
		e := Effect(n)
		if !e.Valid() {
			return nil, fmt.Errorf("effect %q: not one this engine applies (%s): %w", n, effectNames(), ErrInvalid)
		}
		out[e] = param
	}
	return out, nil
}

// EffectNames returns the effects a transition declares, sorted, for a log
// line and an event payload. A map has no order and an audit record needs one.
func (t Transition) EffectNames() []string {
	out := make([]string, 0, len(t.Effects))
	for e := range t.Effects {
		out = append(out, string(e))
	}
	sort.Strings(out)
	return out
}

// GuardNames returns the transition's guards as strings, for an event payload.
func (t Transition) GuardNames() []string {
	out := make([]string, 0, len(t.Guards))
	for _, g := range t.Guards {
		out = append(out, string(g))
	}
	return out
}

func guardNames() string {
	out := make([]string, len(Guards))
	for i, g := range Guards {
		out[i] = string(g)
	}
	return strings.Join(out, ", ")
}

func effectNames() string {
	out := make([]string, len(Effects))
	for i, e := range Effects {
		out[i] = string(e)
	}
	return strings.Join(out, ", ")
}
