// Copyright (c) 2026 Michael D Henderson.

package domain

import (
	"testing"
	"time"
)

func TestCountApprovalsIn(t *testing.T) {
	at := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	approvals := []Approval{
		{VersionID: 1, State: "review", UserID: 10, CreatedAt: at},
		{VersionID: 1, State: "review", UserID: 11, CreatedAt: at},
		{VersionID: 1, State: "legal", UserID: 10, CreatedAt: at},
	}

	if n := CountApprovalsIn(approvals, "review"); n != 2 {
		t.Errorf("CountApprovalsIn(review) = %d, want 2", n)
	}
	if n := CountApprovalsIn(approvals, "legal"); n != 1 {
		t.Errorf("CountApprovalsIn(legal) = %d, want 1", n)
	}
	// A state nobody approved in is zero rather than an error: the guard
	// compares a count, and "nobody has signed off" is a count.
	if n := CountApprovalsIn(approvals, "draft"); n != 0 {
		t.Errorf("CountApprovalsIn(draft) = %d, want 0", n)
	}

	if !ApprovedBy(approvals, "review", 11) {
		t.Error("ApprovedBy did not find an approval that is there")
	}
	if ApprovedBy(approvals, "review", 12) {
		t.Error("ApprovedBy found an approval nobody recorded")
	}
	if ApprovedBy(approvals, "draft", 10) {
		t.Error("ApprovedBy matched across states; an approval is given in one state")
	}
}

// TestApprovalPrivilege is the rule an approval is authorised by: the person
// whose sign-off the process counts is the person the process would let make
// the move (PLAN.md M11).
func TestApprovalPrivilege(t *testing.T) {
	wf := Workflow{
		Name:         "Story",
		InitialState: "draft",
		States: []WorkflowState{
			{Slug: "draft"}, {Slug: "review"}, {Slug: "legal"}, {Slug: "approved"},
		},
		Transitions: []Transition{
			// Out of review: two moves count approvals, at two levels, and
			// one does not count them at all.
			{From: "review", To: "approved", Name: "Approve", Privilege: Create,
				Guards: []Guard{GuardApprovalsMet}},
			{From: "review", To: "legal", Name: "Refer", Privilege: Publish,
				Guards: []Guard{GuardApprovalsMet}},
			{From: "review", To: "draft", Name: "Reject", Privilege: Edit},
			// Out of draft: nothing counts approvals.
			{From: "draft", To: "review", Name: "Submit", Privilege: Edit,
				Guards: []Guard{GuardHasSlug}},
		},
	}

	for _, tc := range []struct {
		state string
		want  Privilege
		ok    bool
	}{
		// The lowest of the two, because holding enough for either one means
		// the approval can matter to that one.
		{state: "review", want: Create, ok: true},
		// A state no transition asks approvals of: the service refuses rather
		// than recording a mark nothing will ever read.
		{state: "draft", want: NoPrivilege},
		{state: "approved", want: NoPrivilege},
		{state: "nowhere", want: NoPrivilege},
	} {
		t.Run(tc.state, func(t *testing.T) {
			got, ok := wf.ApprovalPrivilege(tc.state)
			if ok != tc.ok {
				t.Fatalf("ApprovalPrivilege(%q) reported ok = %t, want %t", tc.state, ok, tc.ok)
			}
			if got != tc.want {
				t.Errorf("ApprovalPrivilege(%q) = %s, want %s", tc.state, got, tc.want)
			}
		})
	}
}
