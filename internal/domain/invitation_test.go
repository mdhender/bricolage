// Copyright (c) 2026 Michael D Henderson.

package domain

import (
	"errors"
	"testing"
	"time"
)

var invitationNow = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// TestExpiryIsDerived is the decision this type is built around: there is no
// "expired" status, so a lapsed invitation is a pending row whose deadline has
// passed and which nothing has touched.
func TestExpiryIsDerived(t *testing.T) {
	for _, s := range InvitationStatuses {
		if s == "expired" {
			t.Fatal("there is an \"expired\" status; expiry is derived from ExpiresAt so that nothing has to run at the 48-hour mark to write one down")
		}
	}

	inv := Invitation{
		Status:    InvitationPending,
		ExpiresAt: invitationNow.Add(InvitationTTL),
	}
	if inv.Expired(invitationNow) {
		t.Error("a fresh invitation reports itself expired")
	}
	if !inv.Redeemable(invitationNow) {
		t.Error("a fresh invitation is not redeemable")
	}

	// One instant past the deadline. The status has not changed, because
	// nothing changed it.
	late := inv.ExpiresAt
	if !inv.Expired(late) {
		t.Error("an invitation is not expired at the instant it expires; the deadline is exclusive, as a session's is")
	}
	if inv.Redeemable(late) {
		t.Error("a lapsed invitation is still redeemable")
	}
	if inv.Status != InvitationPending {
		t.Errorf("status = %q; lapsing writes nothing, so it stays pending", inv.Status)
	}
}

// TestTerminalStatuses is what the partial unique index depends on: pending is
// the only status that holds the index, and the other three release it.
func TestTerminalStatuses(t *testing.T) {
	if InvitationPending.Terminal() {
		t.Error("pending is terminal, which would mean an invitation nobody can act on")
	}
	for _, s := range []InvitationStatus{InvitationRedeemed, InvitationRevoked, InvitationSuperseded} {
		if !s.Terminal() {
			t.Errorf("%q is not terminal", s)
		}
		if !s.Valid() {
			t.Errorf("%q is not a valid status", s)
		}
	}
	if InvitationStatus("expired").Valid() {
		t.Error("\"expired\" is a valid status; it is derived and must not be storable")
	}
}

func TestParseInvitationStatus(t *testing.T) {
	for _, s := range InvitationStatuses {
		got, err := ParseInvitationStatus(string(s))
		if err != nil || got != s {
			t.Errorf("ParseInvitationStatus(%q) = %q, %v", s, got, err)
		}
	}
	for _, bad := range []string{"", "expired", "Pending", "gone"} {
		if _, err := ParseInvitationStatus(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("ParseInvitationStatus(%q) accepted it or gave the wrong error: %v", bad, err)
		}
	}
}

// TestRedeemableIgnoresTheTokenHash guards the bug this predicate had before it
// was written down: the hash is stripped on the way out of internal/service, so
// a predicate that required one answered "no" for every invitation a client can
// see.
func TestRedeemableIgnoresTheTokenHash(t *testing.T) {
	inv := Invitation{
		Status:    InvitationPending,
		ExpiresAt: invitationNow.Add(time.Hour),
	}
	if !inv.Redeemable(invitationNow) {
		t.Error("an invitation with no hash on it is not redeemable; the hash never reaches a caller and is not what decides this")
	}
}

func TestInvitationValidate(t *testing.T) {
	valid := Invitation{
		UID:       "01J000000000000000000000",
		Email:     "someone@example.com",
		TokenHash: "abc123",
		Status:    InvitationPending,
		ExpiresAt: invitationNow.Add(InvitationTTL),
		CreatedAt: invitationNow,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("a well-formed invitation was refused: %v", err)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*Invitation)
	}{
		{"no uid", func(i *Invitation) { i.UID = "" }},
		{"no address", func(i *Invitation) { i.Email = "" }},
		{"a bad address", func(i *Invitation) { i.Email = "not an address" }},
		{"no status", func(i *Invitation) { i.Status = "" }},
		{"a derived status", func(i *Invitation) { i.Status = "expired" }},
		{"pending with no token", func(i *Invitation) { i.TokenHash = "" }},
		{"no deadline", func(i *Invitation) { i.ExpiresAt = time.Time{} }},
		{"no creation time", func(i *Invitation) { i.CreatedAt = time.Time{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv := valid
			tc.mutate(&inv)
			if err := inv.Validate(); !errors.Is(err, ErrInvalid) {
				t.Errorf("Validate() = %v, want an ErrInvalid", err)
			}
		})
	}
}

// TestInvitationTTL pins the window issue #6 states, because it is the number
// the whole design leans on: an administrator cannot see the credential, so the
// only thing bounding its exposure is how long it lives.
func TestInvitationTTL(t *testing.T) {
	if InvitationTTL != 48*time.Hour {
		t.Errorf("InvitationTTL = %v, want 48h", InvitationTTL)
	}
}
