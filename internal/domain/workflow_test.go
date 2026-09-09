// Copyright (c) 2026 Michael D Henderson.

package domain

import (
	"errors"
	"testing"
	"time"
)

// domain is pure, so the workflow vocabulary is tested exhaustively and
// table-driven here (AGENTS.md, "Testing"). What these tests are really about
// is invariant 6: a guard or an effect naming something outside the closed set
// must be refused when it is read, not ignored when it runs.

func TestGuardVocabularyIsClosed(t *testing.T) {
	if len(Guards) != 7 {
		t.Fatalf("there are %d guards, and DESIGN.md 6.1 declares seven: %v", len(Guards), Guards)
	}
	want := map[Guard]bool{
		GuardNoteRequired: true, GuardAssigneeOnly: true, GuardApprovalsMet: true,
		GuardNotLocked: true, GuardCommentsResolved: true, GuardHasSlug: true,
		GuardHasCoverDate: true,
	}
	for _, g := range Guards {
		if !want[g] {
			t.Errorf("%q is in Guards and is not one of the seven", g)
		}
		if !g.Valid() {
			t.Errorf("%q is in Guards and reports itself invalid", g)
		}
		delete(want, g)
	}
	for g := range want {
		t.Errorf("%q is one of the seven and is not in Guards", g)
	}
	for _, not := range []Guard{"", "pre_chk_rules", "note-required", "NOTE_REQUIRED"} {
		if not.Valid() {
			t.Errorf("%q reports itself a guard", not)
		}
	}
}

func TestEffectVocabularyIsClosed(t *testing.T) {
	if len(Effects) != 4 {
		t.Fatalf("there are %d effects, and DESIGN.md 6.1 declares four: %v", len(Effects), Effects)
	}
	for _, e := range Effects {
		if !e.Valid() {
			t.Errorf("%q is in Effects and reports itself invalid", e)
		}
	}
	if !EffectSetDueIn.Parameterised() {
		t.Error("set_due_in takes a duration and reports that it does not")
	}
	for _, e := range []Effect{EffectClearAssignee, EffectAssignToActor, EffectClearApprovals} {
		if e.Parameterised() {
			t.Errorf("%q reports that it takes a parameter; it is named by its presence", e)
		}
	}
	if Effect("post_chk_rules").Valid() {
		t.Error("an effect this engine does not apply reports itself valid")
	}
}

func TestParseGuards(t *testing.T) {
	for _, tc := range []struct {
		name    string
		raw     string
		want    []Guard
		wantErr bool
	}{
		{name: "empty column", raw: "", want: nil},
		{name: "empty array", raw: `[]`, want: []Guard{}},
		{
			name: "the ones the default workflow uses",
			raw:  `["not_locked","assignee_only","has_slug"]`,
			want: []Guard{GuardNotLocked, GuardAssigneeOnly, GuardHasSlug},
		},
		{name: "a name outside the vocabulary", raw: `["pre_chk_rules"]`, wantErr: true},
		{name: "not an array", raw: `{"not_locked":true}`, wantErr: true},
		{name: "not JSON", raw: `not_locked`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseGuards(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseGuards(%q) = %v, want an error; a guard this engine does not enforce must refuse the transition, not run it unguarded", tc.raw, got)
				}
				if !errors.Is(err, ErrInvalid) {
					t.Errorf("ParseGuards(%q) error does not wrap ErrInvalid: %v", tc.raw, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseGuards(%q): %v", tc.raw, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("ParseGuards(%q) = %v, want %v", tc.raw, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("guard %d = %q, want %q; the order is the order they are checked in", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestParseEffects(t *testing.T) {
	got, err := ParseEffects(`{"clear_assignee":"","set_due_in":"48h"}`)
	if err != nil {
		t.Fatalf("ParseEffects: %v", err)
	}
	if len(got) != 2 || got[EffectSetDueIn] != "48h" {
		t.Fatalf("ParseEffects = %v", got)
	}

	if _, err := ParseEffects(`{"post_chk_rules":""}`); !errors.Is(err, ErrInvalid) {
		t.Errorf("an effect outside the vocabulary was accepted: %v", err)
	}
	if _, err := ParseEffects(`["clear_assignee"]`); !errors.Is(err, ErrInvalid) {
		t.Errorf("an effects array was accepted; the column is an object of name to parameter: %v", err)
	}
	if got, err := ParseEffects(""); err != nil || len(got) != 0 {
		t.Errorf(`ParseEffects("") = %v, %v; want an empty map`, got, err)
	}
}

func TestTransitionValidate(t *testing.T) {
	base := func() Transition {
		return Transition{
			Name: "Submit", From: "draft", To: "review",
			Privilege: Edit, Effects: map[Effect]string{},
		}
	}

	for _, tc := range []struct {
		name    string
		mutate  func(*Transition)
		wantErr bool
	}{
		{name: "the default workflow's submit", mutate: func(*Transition) {}},
		{
			name:   "every guard in the vocabulary",
			mutate: func(t *Transition) { t.Guards = Guards },
		},
		{
			name:    "a guard outside the vocabulary",
			mutate:  func(t *Transition) { t.Guards = []Guard{"pre_chk_rules"} },
			wantErr: true,
		},
		{
			name:    "an effect outside the vocabulary",
			mutate:  func(t *Transition) { t.Effects = map[Effect]string{"post_chk_rules": ""} },
			wantErr: true,
		},
		{
			name:    "a parameter on an effect that takes none",
			mutate:  func(t *Transition) { t.Effects = map[Effect]string{EffectClearAssignee: "48h"} },
			wantErr: true,
		},
		{
			name:   "a duration on the one effect that takes one",
			mutate: func(t *Transition) { t.Effects = map[Effect]string{EffectSetDueIn: "48h"} },
		},
		{
			name:    "a due-in that is not a duration",
			mutate:  func(t *Transition) { t.Effects = map[Effect]string{EffectSetDueIn: "soon"} },
			wantErr: true,
		},
		{
			name:    "a due-in in the past",
			mutate:  func(t *Transition) { t.Effects = map[Effect]string{EffectSetDueIn: "-1h"} },
			wantErr: true,
		},
		{
			name:    "a privilege off the scale",
			mutate:  func(t *Transition) { t.Privilege = Privilege(9) },
			wantErr: true,
		},
		{
			name:    "no target state",
			mutate:  func(t *Transition) { t.To = "" },
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := base()
			tc.mutate(&tr)
			err := tr.Validate()
			if tc.wantErr && err == nil {
				t.Fatal("Validate accepted a transition the engine cannot run")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if tc.wantErr && !errors.Is(err, ErrInvalid) {
				t.Errorf("the refusal does not wrap ErrInvalid: %v", err)
			}
		})
	}
}

func TestTransitionDueIn(t *testing.T) {
	none := Transition{Effects: map[Effect]string{}}
	if d, ok, err := none.DueIn(); ok || d != 0 || err != nil {
		t.Errorf("DueIn on a transition with no due effect = %v %v %v", d, ok, err)
	}

	tr := Transition{Effects: map[Effect]string{EffectSetDueIn: "48h"}}
	d, ok, err := tr.DueIn()
	if err != nil || !ok || d != 48*time.Hour {
		t.Errorf("DueIn = %v %v %v, want 48h true nil", d, ok, err)
	}
}

func TestTransitionNeedsNote(t *testing.T) {
	if (Transition{Guards: []Guard{GuardNotLocked}}).NeedsNote() {
		t.Error("a transition with no note_required reports that it needs a note")
	}
	if !(Transition{Guards: []Guard{GuardNoteRequired}}).NeedsNote() {
		t.Error("a transition declaring note_required reports that it does not need a note")
	}
}

// storyWorkflow is the shape the migration seeds, small enough to reason about.
func storyWorkflow() Workflow {
	return Workflow{
		ID: 1, Name: "Story", Kind: KindStory, InitialState: "draft",
		States: []WorkflowState{
			{Slug: "draft", Name: "Draft", Position: 1},
			{Slug: "review", Name: "In review", Position: 2, RequiredApprovals: 1},
			{Slug: "approved", Name: "Approved", Position: 3, Publishable: true},
			{Slug: "archived", Name: "Archived", Position: 4, Terminal: true},
		},
		Transitions: []Transition{
			{Name: "Submit", From: "draft", To: "review", Privilege: Edit, Effects: map[Effect]string{}},
			{Name: "Approve", From: "review", To: "approved", Privilege: Create, Effects: map[Effect]string{}},
			{Name: "Reject", From: "review", To: "draft", Privilege: Create, Effects: map[Effect]string{}},
			{Name: "Archive", From: "draft", To: "archived", Privilege: Create, Effects: map[Effect]string{}},
		},
	}
}

func TestWorkflowFromAndTransition(t *testing.T) {
	w := storyWorkflow()

	if got := w.From("review"); len(got) != 2 {
		t.Errorf("From(review) returned %d transitions, want 2", len(got))
	}
	if got := w.From("archived"); len(got) != 0 {
		t.Errorf("From(archived) returned %d transitions, want none: nothing leaves it in this fixture", len(got))
	}

	if _, err := w.Transition("draft", "review"); err != nil {
		t.Errorf("a declared transition was refused: %v", err)
	}

	// The move a person with every privilege still may not make: it is not in
	// the machine, so it is a conflict rather than a permission question
	// (PLAN.md M4 acceptance 2).
	_, err := w.Transition("draft", "approved")
	if !errors.Is(err, ErrConflict) {
		t.Errorf("an undeclared transition returned %v, want ErrConflict", err)
	}
}

func TestWorkflowValidate(t *testing.T) {
	if err := storyWorkflow().Validate(); err != nil {
		t.Fatalf("the story workflow does not validate: %v", err)
	}

	noInitial := storyWorkflow()
	noInitial.InitialState = "nowhere"
	if err := noInitial.Validate(); !errors.Is(err, ErrInvalid) {
		t.Errorf("a workflow whose initial state it does not declare was accepted: %v", err)
	}

	danglingTo := storyWorkflow()
	danglingTo.Transitions[0].To = "nowhere"
	if err := danglingTo.Validate(); !errors.Is(err, ErrInvalid) {
		t.Errorf("a transition into a state the workflow does not declare was accepted: %v", err)
	}
}

func TestGuardError(t *testing.T) {
	err := GuardApprovalsMet.Refused("Approve", "not enough approvals: 1 of 2")

	if !errors.Is(err, ErrGuardFailed) {
		t.Error("a guard refusal does not answer to ErrGuardFailed; the edge maps it to a 409 through that")
	}
	if errors.Is(err, ErrForbidden) {
		t.Error("a guard refusal answers to ErrForbidden; a guard is a statement about the document, not about the person")
	}
	g, ok := GuardOf(err)
	if !ok || g != GuardApprovalsMet {
		t.Errorf("GuardOf = %q %v, want approvals_met true", g, ok)
	}
	if got := err.Error(); got != "Approve: not enough approvals: 1 of 2 (approvals_met)" {
		t.Errorf("the message reads %q", got)
	}

	if _, ok := GuardOf(errors.New("something else")); ok {
		t.Error("GuardOf found a guard in an error that is not a refusal")
	}
}

func TestDocumentSubjectCarriesWorkflowAndState(t *testing.T) {
	d := Document{ID: 7, SiteID: 2, Kind: KindStory, WorkflowID: 3, State: "review"}
	got := d.Subject()

	if got.WorkflowID != 3 || got.State != "review" {
		t.Errorf("Subject() = %+v; a grant scoped to a workflow or a state has nothing to match without these", got)
	}
	if got.SiteID != 2 || got.DocKind != KindStory || got.DocumentID != 7 {
		t.Errorf("Subject() lost a dimension it already carried: %+v", got)
	}
}

// TestContradictoryAssignmentEffectsAreRefused keeps a workflow from being
// configured with two effects that fight over the same column. It is refused
// when the row is read rather than resolved by whichever the engine applies
// second, because "whichever runs last wins" is a rule nobody wrote down.
func TestContradictoryAssignmentEffectsAreRefused(t *testing.T) {
	tr := Transition{
		Name: "Submit", From: "draft", To: "review", Privilege: Edit,
		Effects: map[Effect]string{EffectClearAssignee: "", EffectAssignToActor: ""},
	}
	if err := tr.Validate(); !errors.Is(err, ErrInvalid) {
		t.Errorf("a transition declaring both clear_assignee and assign_to_actor was accepted: %v", err)
	}
}
