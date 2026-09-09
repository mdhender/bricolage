// Copyright (c) 2026 Michael D Henderson.

package store

import (
	"errors"
	"testing"
	"time"

	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/events"
	"zombiezen.com/go/sqlite"
)

var invitedAt = time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

func newInvitation(uid, email, hash string, at time.Time) domain.Invitation {
	return domain.Invitation{
		UID:       uid,
		Email:     email,
		TokenHash: hash,
		Status:    domain.InvitationPending,
		ExpiresAt: at.Add(domain.InvitationTTL),
		CreatedAt: at,
	}
}

func createdEvent(at time.Time) domain.Event {
	return domain.Event{Type: events.InvitationCreated, OccurredAt: at, Payload: map[string]any{"t": "x"}}
}

func supersededEvent(at time.Time) domain.Event {
	return domain.Event{Type: events.InvitationSuperseded, OccurredAt: at, Payload: map[string]any{"t": "x"}}
}

// TestOnePendingInvitationPerAddress is the index, and the thing that makes it
// survivable: a lapsed invitation still holds it, because expiry is derived and
// nothing wrote a status when the 48 hours ran out. Creating a second
// invitation supersedes the first rather than colliding with it.
func TestOnePendingInvitationPerAddress(t *testing.T) {
	db := memoryDB(t)

	first, err := db.CreateInvitation(t.Context(),
		newInvitation("inv-1", "twice@example.com", "hash-1", invitedAt),
		createdEvent(invitedAt), supersededEvent(invitedAt))
	if err != nil {
		t.Fatalf("the first invitation: %v", err)
	}

	// Two days later, with nothing having run in between.
	later := invitedAt.Add(domain.InvitationTTL + time.Hour)
	second, err := db.CreateInvitation(t.Context(),
		newInvitation("inv-2", "twice@example.com", "hash-2", later),
		createdEvent(later), supersededEvent(later))
	if err != nil {
		t.Fatalf("re-inviting a lapsed address was refused, which is the partial index winning: %v", err)
	}

	settled, err := db.InvitationByUID(t.Context(), first.UID)
	if err != nil {
		t.Fatalf("the first invitation is gone; no invitation row is ever deleted: %v", err)
	}
	if settled.Status != domain.InvitationSuperseded {
		t.Errorf("the first invitation is %q, want superseded", settled.Status)
	}
	if settled.TokenHash != "" {
		t.Error("the superseded invitation kept its token hash; superseding is one of the three moments a hash is cleared")
	}
	if settled.SettledAt.IsZero() {
		t.Error("the superseded invitation has no settled_at")
	}
	if second.Status != domain.InvitationPending {
		t.Errorf("the replacement is %q, want pending", second.Status)
	}

	// The old hash finds nothing, which is what makes the old link dead at
	// once rather than at its own expiry.
	if _, err := db.InvitationByTokenHash(t.Context(), "hash-1"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("the superseded token still resolves: %v", err)
	}
	if _, err := db.InvitationByTokenHash(t.Context(), "hash-2"); err != nil {
		t.Errorf("the replacement token does not resolve: %v", err)
	}

	// Two live pending rows for one address is what the index forbids, and the
	// only way to reach it now is to go around CreateInvitation. Assert the
	// index is really there, by doing exactly that.
	err = db.Write(t.Context(), func(conn *sqlite.Conn) error {
		return run(conn, "forcing a second pending invitation", `
			INSERT INTO invitations (uid, email, token_sha256, status, expires_at, created_at)
			VALUES ('inv-3', 'twice@example.com', 'hash-3', 'pending', :expires, :created)`,
			func(stmt *sqlite.Stmt) {
				stmt.SetText(":expires", formatTime(later.Add(domain.InvitationTTL)))
				stmt.SetText(":created", formatTime(later))
			}, nil)
	})
	var ce *ConstraintError
	if !errors.As(err, &ce) || !ce.IsUnique() {
		t.Errorf("a second pending invitation for one address was accepted (%v); invitations_one_pending is not doing its job", err)
	}
}

// TestSettleInvitationIsTheOnlyWayOut covers revocation and the race it has to
// lose: the UPDATE names 'pending', so settling a row somebody else settled
// changes nothing rather than overwriting what they recorded.
func TestSettleInvitationIsTheOnlyWayOut(t *testing.T) {
	db := memoryDB(t)

	inv, err := db.CreateInvitation(t.Context(),
		newInvitation("inv-1", "gone@example.com", "hash-1", invitedAt),
		createdEvent(invitedAt), supersededEvent(invitedAt))
	if err != nil {
		t.Fatal(err)
	}

	revoked := domain.Event{Type: events.InvitationRevoked, OccurredAt: invitedAt, Payload: map[string]any{"t": "x"}}
	out, settled, err := db.SettleInvitation(t.Context(), inv.ID, domain.InvitationRevoked,
		"changed our minds", invitedAt, revoked)
	if err != nil {
		t.Fatalf("SettleInvitation: %v", err)
	}
	if !settled {
		t.Error("settling a pending invitation reported that nothing changed")
	}
	if out.Status != domain.InvitationRevoked || out.Reason != "changed our minds" {
		t.Errorf("settled to %+v, want revoked with the reason", out)
	}
	if out.TokenHash != "" {
		t.Error("revocation left the token hash behind")
	}

	// Again, which is the race and the idempotence at once.
	_, settled, err = db.SettleInvitation(t.Context(), inv.ID, domain.InvitationRevoked, "", invitedAt, revoked)
	if err != nil {
		t.Fatalf("settling twice: %v", err)
	}
	if settled {
		t.Error("settling an already-settled invitation reported a change; the UPDATE names 'pending' so that a race loses rather than overwrites")
	}

	// A status nothing moves out of is the only thing this method writes.
	if _, _, err := db.SettleInvitation(t.Context(), inv.ID, domain.InvitationPending, "", invitedAt, revoked); !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("settling to pending was accepted: %v", err)
	}
}

// TestRedeemIsOneTransaction is the property the whole method exists for: the
// account and the settled invitation arrive together or not at all.
func TestRedeemIsOneTransaction(t *testing.T) {
	db := memoryDB(t)

	inv, err := db.CreateInvitation(t.Context(),
		newInvitation("inv-1", "rose@example.com", "hash-1", invitedAt),
		createdEvent(invitedAt), supersededEvent(invitedAt))
	if err != nil {
		t.Fatal(err)
	}

	redeemed := domain.Event{Type: events.InvitationRedeemed, OccurredAt: invitedAt, Payload: map[string]any{"t": "x"}}
	userCreated := domain.Event{Type: events.UserCreated, OccurredAt: invitedAt, Payload: map[string]any{"t": "x"}}

	u, out, err := db.Redeem(t.Context(), inv.ID, NewUser{
		UID: "user-1", Email: "rose@example.com", Name: "Rose", PasswordHash: "bcrypt", CreatedAt: invitedAt,
	}, redeemed, userCreated)
	if err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	if u.Email != "rose@example.com" || out.Status != domain.InvitationRedeemed {
		t.Errorf("Redeem produced %+v / %q", u, out.Status)
	}
	if out.UserID != u.ID {
		t.Errorf("the invitation names user %d, want %d", out.UserID, u.ID)
	}
	if out.TokenHash != "" {
		t.Error("redemption left the token hash behind")
	}

	// A second redemption of the same row is refused by the UPDATE rather than
	// by a read that raced it, and it leaves no second account.
	_, _, err = db.Redeem(t.Context(), inv.ID, NewUser{
		UID: "user-2", Email: "rose2@example.com", Name: "Rose Again", PasswordHash: "bcrypt", CreatedAt: invitedAt,
	}, redeemed, userCreated)
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a second redemption returned %v, want a conflict", err)
	}
	if _, err := db.UserByEmail(t.Context(), "rose2@example.com"); !errors.Is(err, domain.ErrNotFound) {
		t.Error("the refused redemption left an account behind; the whole method is one transaction so that it cannot")
	}
}

// TestListInvitationsShowsLapsedRowsAsPending is the listing rule that follows
// from deriving expiry: a lapsed invitation is in the pending list, because it
// is the row an administrator has to act on.
func TestListInvitationsShowsLapsedRowsAsPending(t *testing.T) {
	db := memoryDB(t)

	if _, err := db.CreateInvitation(t.Context(),
		newInvitation("inv-1", "lapsed@example.com", "hash-1", invitedAt),
		createdEvent(invitedAt), supersededEvent(invitedAt)); err != nil {
		t.Fatal(err)
	}

	pending, err := db.ListInvitations(t.Context(), domain.InvitationPending)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("the pending listing returned %d rows, want 1", len(pending))
	}
	if !pending[0].Expired(invitedAt.Add(domain.InvitationTTL + time.Hour)) {
		t.Error("the row does not report itself expired two days on")
	}

	all, err := db.ListInvitations(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Errorf("the unfiltered listing returned %d rows, want 1", len(all))
	}
}

// TestClearInvitationTokenTouchesNothingElse covers the opportunistic half of
// the hash-clearing rule: it drops the hash and leaves the row pending, because
// nothing decided anything -- time merely passed.
func TestClearInvitationTokenTouchesNothingElse(t *testing.T) {
	db := memoryDB(t)

	inv, err := db.CreateInvitation(t.Context(),
		newInvitation("inv-1", "lapsed@example.com", "hash-1", invitedAt),
		createdEvent(invitedAt), supersededEvent(invitedAt))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ClearInvitationToken(t.Context(), inv.ID); err != nil {
		t.Fatalf("ClearInvitationToken: %v", err)
	}

	out, err := db.InvitationByUID(t.Context(), inv.UID)
	if err != nil {
		t.Fatal(err)
	}
	if out.TokenHash != "" {
		t.Error("the hash is still there")
	}
	if out.Status != domain.InvitationPending {
		t.Errorf("status = %q, want pending; clearing a hash is not a decision anybody made", out.Status)
	}
	if !out.SettledAt.IsZero() {
		t.Error("clearing the hash set settled_at; nothing settled the row")
	}
}
