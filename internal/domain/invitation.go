// Copyright (c) 2026 Michael D Henderson.

package domain

import (
	"fmt"
	"time"
)

// Invitations are how an account comes into existence (issue #6). Registration
// is invite-only: there is no self-service sign-up, and the only other way a
// user row appears is "cmsdb bootstrap admin".
//
// This file is the pure half -- the statuses, the derived expiry, and the
// validation. It does no I/O and reads no clock: every question about time
// takes the instant as an argument (invariant 3).

// SubjectInvitation is the events subject kind for an invitation.
//
// An invitation can be an event subject precisely because no invitation row is
// ever deleted. That is the opposite of the situation at publish.go:44, where
// there is deliberately no SubjectResource: a row that vanishes cannot anchor
// an audit trail, and "who invited this person, when, and did they ever
// accept" is a question this system has to keep answering.
const SubjectInvitation = "invitation"

// InvitationTTL is how long a link works: 48 hours from the moment it was
// created (issue #6).
//
// It is a constant rather than configuration, and the absence of a way to
// extend it is deliberate. An administrator cannot see the token -- it is
// shown once and what is stored is a hash -- so extending one would be a
// decision taken blind about a credential that may be sitting in a forwarded
// mail; and the invitee has to be told the window moved either way, so the
// saving is pasting a link into a message that has to be sent regardless.
// "48 hours, single use" stays a property of the system rather than of an
// administrator's habits. Re-inviting is how a lapsed invitation is replaced.
const InvitationTTL = 48 * time.Hour

// InvitationStatus is what has become of an invitation.
//
// There are four and one of them is not here: there is no "expired". Expiry is
// derived from ExpiresAt wherever it is needed, so that nothing has to run at
// the 48-hour mark to write it down, and so that there is no second source of
// truth for a fact the timestamp already carries.
type InvitationStatus string

const (
	// InvitationPending is the only non-terminal status. A pending invitation
	// may still have lapsed: ask Expired, not this.
	InvitationPending InvitationStatus = "pending"

	// InvitationRedeemed is an invitation somebody used. The account it
	// created is in UserID.
	InvitationRedeemed InvitationStatus = "redeemed"

	// InvitationRevoked is an invitation an administrator cancelled.
	InvitationRevoked InvitationStatus = "revoked"

	// InvitationSuperseded is an invitation replaced by a later one to the
	// same address. It is distinct from revoked because it says something true
	// that revoked would not: nobody decided against this person.
	InvitationSuperseded InvitationStatus = "superseded"
)

// InvitationStatuses are the four a row may carry, in the order the listing
// offers them as filters.
var InvitationStatuses = []InvitationStatus{
	InvitationPending, InvitationRedeemed, InvitationRevoked, InvitationSuperseded,
}

// Valid reports whether s is one of the four. The CHECK constraint in
// migration 0013 says the same thing to the database; this is what says it to a
// filter that came off the wire.
func (s InvitationStatus) Valid() bool {
	for _, known := range InvitationStatuses {
		if s == known {
			return true
		}
	}
	return false
}

// Terminal reports whether the status is one nothing moves out of.
func (s InvitationStatus) Terminal() bool {
	return s.Valid() && s != InvitationPending
}

func (s InvitationStatus) String() string { return string(s) }

// ParseInvitationStatus reads a status somebody typed, for the listing filter.
func ParseInvitationStatus(s string) (InvitationStatus, error) {
	status := InvitationStatus(s)
	if !status.Valid() {
		return "", fmt.Errorf("status %q: not one of pending, redeemed, revoked, superseded: %w",
			s, ErrInvalid)
	}
	return status, nil
}

// Invitation is one outstanding or historical invitation.
//
// TokenHash is the SHA-256 of the token in the link and is the only trace of
// it this system keeps; the token itself exists once, in the response that
// created the row. The hash is cleared when the row settles, so a terminal
// invitation carries no sensitive column at all.
type Invitation struct {
	ID    int64
	UID   string
	Email string

	// TokenHash is empty on a settled row, and on every row this system hands
	// outward: it is never serialised and never logged.
	TokenHash string

	Status    InvitationStatus
	ExpiresAt time.Time

	// Reason is why it was revoked, when whoever revoked it said so.
	Reason string

	// InvitedBy is the administrator who created it; UserID is the account
	// redemption created, zero until then. Both are internal keys and neither
	// leaves the process (invariant 10); the projection carries the uids and
	// the names beside them.
	InvitedBy     int64
	InvitedByUID  string
	InvitedByName string
	UserID        int64
	UserUID       string

	CreatedAt time.Time

	// SettledAt is when it stopped being pending, zero while it still is. It
	// is deliberately not set by expiry: nothing runs at the 48-hour mark, so
	// a lapsed invitation is pending with a past ExpiresAt and no SettledAt.
	SettledAt time.Time
}

// Expired reports whether the link has stopped working because time passed.
//
// The instant is an argument because time.Now belongs to main and
// internal/clock (invariant 3), and because an expiry that cannot be tested at
// an arbitrary instant is an expiry nobody tests.
func (i Invitation) Expired(now time.Time) bool { return !now.Before(i.ExpiresAt) }

// Redeemable reports whether this invitation would accept a redemption now.
//
// It is the whole of the decision, in one place, so that the route, the UI and
// any test ask the same question. What it deliberately does not do is say
// which clause failed: every redemption failure answers identically, because a
// form that distinguished them would be an oracle for who has an account here
// and the anti-forwarding check would become a way to enumerate who was
// invited. The reason goes to the log, where an operator can read it.
//
// It asks nothing about TokenHash, and that is deliberate rather than an
// oversight. The hash is stripped from every invitation on its way out of
// internal/service, so a value that has been anywhere near a transport has none
// -- and the redemption path found its row *by* hashing the token it was given,
// so it has one by construction. Testing the field here would make this
// predicate answer "no" for every invitation anybody can see.
func (i Invitation) Redeemable(now time.Time) bool {
	return i.Status == InvitationPending && !i.Expired(now)
}

// Validate checks an invitation about to be written.
func (i Invitation) Validate() error {
	if err := ValidateEmail(i.Email); err != nil {
		return err
	}
	if i.UID == "" {
		return fmt.Errorf("invitation: uid: required: %w", ErrInvalid)
	}
	if !i.Status.Valid() {
		return fmt.Errorf("invitation %q: status %q: %w", i.UID, i.Status, ErrInvalid)
	}
	if i.Status == InvitationPending && i.TokenHash == "" {
		return fmt.Errorf("invitation %q: a pending invitation needs a token: %w", i.UID, ErrInvalid)
	}
	if i.ExpiresAt.IsZero() {
		return fmt.Errorf("invitation %q: expires_at: required: %w", i.UID, ErrInvalid)
	}
	if i.CreatedAt.IsZero() {
		return fmt.Errorf("invitation %q: created_at: required: %w", i.UID, ErrInvalid)
	}
	return nil
}

// Redemption is what somebody redeeming a link supplies.
//
// The e-mail address is in it deliberately, and it is the reason a forwarded
// invitation is not usable by whoever received it: the link alone is not
// enough, and the person redeeming has to know which address was invited.
type Redemption struct {
	Token    string
	Email    string
	Name     string
	Password string
}
