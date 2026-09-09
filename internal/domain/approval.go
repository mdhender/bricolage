// Copyright (c) 2026 Michael D Henderson.

package domain

import "time"

// Approvals attach to a version, not a document (DESIGN.md 5.5, PLAN.md M11).
//
// Edit the document, a new version exists, and the old approvals no longer
// satisfy the guard: "changes invalidate sign-off" costs one foreign key
// rather than a rule somebody has to remember. The old version's approvals are
// still there afterwards, because who signed off on what is a fact about last
// Tuesday and not a counter.
//
// The state is recorded with the approval because GuardApprovalsMet counts the
// approvals given in the state a document is leaving. A document that goes
// back to draft and returns to review is being looked at a second time, and
// the approvals of the first look are not the approvals of the second.

// Approval is one person's sign-off of one version in one state.
type Approval struct {
	ID         int64
	DocumentID int64
	VersionID  int64

	// State is the state the approval was given in.
	State string

	UserID    int64
	CreatedAt time.Time
}

// CountApprovalsIn counts the approvals given in one state.
//
// It is here rather than in the store because the caller that needs it -- the
// "2 of 2" a client shows beside an Approve button -- already holds the rows,
// and a second query to count what is in a slice is a round trip that buys
// nothing.
func CountApprovalsIn(approvals []Approval, state string) int {
	n := 0
	for _, a := range approvals {
		if a.State == state {
			n++
		}
	}
	return n
}

// ApprovedBy reports whether a user is among the approvals of one state.
//
// A client renders "Approve" or "Withdraw" from this, and the service reads it
// to answer the second approval of the same person: approving twice is
// idempotent, which is a property of the UNIQUE constraint rather than of a
// check, and this is how the answer is *described* rather than how it is
// enforced (PLAN.md M11 acceptance 1).
func ApprovedBy(approvals []Approval, state string, userID int64) bool {
	for _, a := range approvals {
		if a.State == state && a.UserID == userID {
			return true
		}
	}
	return false
}
