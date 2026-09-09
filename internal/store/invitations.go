// Copyright (c) 2026 Michael D Henderson.

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/mdhender/bricolage/internal/domain"
	"zombiezen.com/go/sqlite"
)

// All the SQL for invitations (invariant 2, issue #6, migration 0013).
//
// Two methods here are worth reading twice, and both are one transaction for
// the same reason: a half-done invitation is a worse state than a refused one.
//
// CreateInvitation supersedes the row it would collide with before inserting
// its own. The partial unique index invitations_one_pending allows one pending
// row per address, and a lapsed invitation is still pending -- expiry is
// derived, so nothing wrote a status when the 48 hours ran out. Superseding in
// the same statement sequence is what keeps an administrator from ever meeting
// that index, and it gives the right security answer as a side effect: the old
// link stops working the instant a new one exists.
//
// Redeem creates the user, settles the invitation, and writes both events
// together. The account and the record of how it came to exist are one fact,
// and an account created by an invitation that still reads as pending would be
// a link somebody could use twice.

// invitationSelect is the projection every invitation read shares.
//
// It joins both user references rather than leaving integers for a caller to
// resolve, because the API speaks uid (invariant 10) and a listing of twenty
// invitations would otherwise be forty more reads.
const invitationSelect = `
	SELECT i.id, i.uid, i.email, i.token_sha256, i.status, i.expires_at, i.reason,
	       i.invited_by, i.user_id, i.created_at, i.settled_at,
	       inviter.uid  AS inviter_uid,
	       inviter.name AS inviter_name,
	       invitee.uid  AS invitee_uid
	  FROM invitations i
	  LEFT JOIN users inviter ON inviter.id = i.invited_by
	  LEFT JOIN users invitee ON invitee.id = i.user_id`

// CreateInvitation writes an invitation, superseding any pending one for the
// same address, and records both in one transaction (invariant 7).
//
// The supersession is not a convenience. Without it the partial unique index
// refuses every second invitation to an address whose first one lapsed, for
// ever: expiry is derived, so a lapsed row still carries status 'pending', and
// SQLite will not let the index exclude it -- a partial index's WHERE clause
// must be deterministic, and "expires_at > now" is not.
//
// The superseded row's hash is cleared as it settles, which is one of the three
// moments a hash is cleared; the other two are redemption and revocation.
func (db *DB) CreateInvitation(ctx context.Context, inv domain.Invitation, created domain.Event, superseded domain.Event) (domain.Invitation, error) {
	if err := inv.Validate(); err != nil {
		return domain.Invitation{}, err
	}
	var out domain.Invitation
	err := db.Tx(ctx, func(conn *sqlite.Conn) error {
		// Settle whatever holds the index. There is at most one, and usually
		// none; the UPDATE is unconditional rather than read-then-write so
		// that no window exists between finding it and replacing it.
		var supersededIDs []int64
		err := run(conn, fmt.Sprintf("superseding invitations for %q", inv.Email), `
			UPDATE invitations
			   SET status = 'superseded', token_sha256 = NULL, settled_at = :now
			 WHERE email = :email AND status = 'pending'
			 RETURNING id`,
			func(stmt *sqlite.Stmt) {
				stmt.SetText(":now", formatTime(inv.CreatedAt))
				stmt.SetText(":email", inv.Email)
			},
			func(stmt *sqlite.Stmt) error {
				supersededIDs = append(supersededIDs, stmt.GetInt64("id"))
				return nil
			})
		if err != nil {
			return err
		}
		for _, id := range supersededIDs {
			e := superseded
			e.SubjectKind = domain.SubjectInvitation
			e.SubjectID = id
			if _, err := recordEvent(conn, e); err != nil {
				return err
			}
		}

		err = run(conn, fmt.Sprintf("creating an invitation for %q", inv.Email), `
			INSERT INTO invitations (uid, email, token_sha256, status, expires_at,
			                         invited_by, created_at)
			VALUES (:uid, :email, :token_sha256, :status, :expires_at,
			        :invited_by, :created_at)`,
			func(stmt *sqlite.Stmt) {
				stmt.SetText(":uid", inv.UID)
				stmt.SetText(":email", inv.Email)
				stmt.SetText(":token_sha256", inv.TokenHash)
				stmt.SetText(":status", string(inv.Status))
				stmt.SetText(":expires_at", formatTime(inv.ExpiresAt))
				if inv.InvitedBy == 0 {
					stmt.SetNull(":invited_by")
				} else {
					stmt.SetInt64(":invited_by", inv.InvitedBy)
				}
				stmt.SetText(":created_at", formatTime(inv.CreatedAt))
			}, nil)
		if err != nil {
			return err
		}
		id := conn.LastInsertRowID()

		created.SubjectKind = domain.SubjectInvitation
		created.SubjectID = id
		if _, err := recordEvent(conn, created); err != nil {
			return err
		}
		return invitationByID(conn, id, &out)
	})
	return out, err
}

// InvitationByUID reads one invitation by the identifier the API speaks
// (invariant 10).
func (db *DB) InvitationByUID(ctx context.Context, uid string) (domain.Invitation, error) {
	var out domain.Invitation
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return one(conn, fmt.Sprintf("invitation %q", uid),
			invitationSelect+` WHERE i.uid = :uid`,
			func(stmt *sqlite.Stmt) { stmt.SetText(":uid", uid) },
			func(stmt *sqlite.Stmt) error {
				var err error
				out, err = scanInvitation(stmt)
				return err
			})
	})
	return out, err
}

// InvitationByTokenHash reads the invitation a link points at.
//
// It matches on the hash and not on the token, which this database never holds:
// the caller hashes what it was given (authz.HashToken) and looks that up, so
// read access to the file cannot mint a working link.
//
// A miss is domain.ErrNotFound, and the caller must not pass that distinction
// on: every redemption failure answers identically, or the form becomes an
// oracle for which addresses have been invited.
func (db *DB) InvitationByTokenHash(ctx context.Context, hash string) (domain.Invitation, error) {
	var out domain.Invitation
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return one(conn, "invitation by token",
			invitationSelect+` WHERE i.token_sha256 = :hash`,
			func(stmt *sqlite.Stmt) { stmt.SetText(":hash", hash) },
			func(stmt *sqlite.Stmt) error {
				var err error
				out, err = scanInvitation(stmt)
				return err
			})
	})
	return out, err
}

// ListInvitations returns invitations with the given status, newest first, or
// every one of them when status is empty.
//
// Pending by default is the caller's decision and not this method's, for the
// reason the listing wants it: a list that accretes every invitation ever sent
// stops being a work queue. What this method will not do is filter on expiry --
// a lapsed invitation is pending and has to appear in the pending list, because
// it is the row an administrator has to act on.
func (db *DB) ListInvitations(ctx context.Context, status domain.InvitationStatus) ([]domain.Invitation, error) {
	query := invitationSelect
	var bind func(*sqlite.Stmt)
	if status != "" {
		query += ` WHERE i.status = :status`
		bind = func(stmt *sqlite.Stmt) { stmt.SetText(":status", string(status)) }
	}
	query += ` ORDER BY i.id DESC`

	var out []domain.Invitation
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return run(conn, "listing invitations", query, bind, func(stmt *sqlite.Stmt) error {
			inv, err := scanInvitation(stmt)
			if err != nil {
				return err
			}
			out = append(out, inv)
			return nil
		})
	})
	return out, err
}

// SettleInvitation moves a pending invitation to a terminal status, clears its
// hash, and records the event, in one transaction.
//
// It is how revocation happens, and it is the only write that does: there is no
// method here that extends an invitation, renews an expired one, or forces one
// to expire. The first two would give a credential nobody can see an unbounded
// lifetime, and the third is this method with a different word in the audit
// trail, which is what the reason column is for.
//
// The WHERE names the status it expects, so settling a row somebody else just
// settled changes nothing and reports it: a revocation racing a redemption
// loses rather than overwriting what the redemption recorded.
func (db *DB) SettleInvitation(ctx context.Context, id int64, status domain.InvitationStatus, reason string, at time.Time, event domain.Event) (domain.Invitation, bool, error) {
	if !status.Terminal() {
		return domain.Invitation{}, false, fmt.Errorf(
			"settling invitation %d: %q is not a terminal status: %w", id, status, domain.ErrInvalid)
	}
	var (
		out     domain.Invitation
		settled bool
	)
	err := db.Tx(ctx, func(conn *sqlite.Conn) error {
		err := run(conn, fmt.Sprintf("settling invitation %d", id), `
			UPDATE invitations
			   SET status = :status, token_sha256 = NULL, settled_at = :at,
			       reason = :reason
			 WHERE id = :id AND status = 'pending'`,
			func(stmt *sqlite.Stmt) {
				stmt.SetText(":status", string(status))
				stmt.SetText(":at", formatTime(at))
				if reason == "" {
					stmt.SetNull(":reason")
				} else {
					stmt.SetText(":reason", reason)
				}
				stmt.SetInt64(":id", id)
			}, nil)
		if err != nil {
			return err
		}
		settled = conn.Changes() > 0
		if settled {
			event.SubjectKind = domain.SubjectInvitation
			event.SubjectID = id
			if _, err := recordEvent(conn, event); err != nil {
				return err
			}
		}
		return invitationByID(conn, id, &out)
	})
	return out, settled, err
}

// ClearInvitationToken drops the hash of a row that has lapsed, without
// settling it.
//
// This is the "opportunistically when an expired row is next touched" half of
// issue #6's hash-clearing rule, and it exists because deriving expiry means
// nothing runs at the 48-hour mark: there is no scheduler for "clear on
// expiry" to hang from. An invitation that lapses and is never visited again
// keeps its hash until something looks at it.
//
// It is tidiness and defence in depth rather than the thing standing between an
// attacker and an account: the derived expiry check runs before the hash is
// ever compared, so the hash on a lapsed row is already inert. It writes no
// event, because nothing happened that anybody decided.
func (db *DB) ClearInvitationToken(ctx context.Context, id int64) error {
	return db.Write(ctx, func(conn *sqlite.Conn) error {
		return run(conn, fmt.Sprintf("clearing the token of invitation %d", id), `
			UPDATE invitations SET token_sha256 = NULL
			 WHERE id = :id AND status = 'pending' AND token_sha256 IS NOT NULL`,
			func(stmt *sqlite.Stmt) { stmt.SetInt64(":id", id) }, nil)
	})
}

// Redeem creates the user an invitation was for and settles the invitation, in
// one transaction.
//
// Three writes and two events, all or nothing. A user row without the
// invitation settled is a link that works twice; a settled invitation without
// the user is an address nobody can be invited to again, since the row is no
// longer pending and the account does not exist.
//
// The UPDATE names 'pending', so two redemptions of the same link race on it
// and exactly one wins -- the loser sees no change and is refused, with the
// same answer every other failure gets. That is the property this being one
// statement buys: there is no window between checking that the invitation is
// open and taking it.
//
// It issues no session. Redemption leaves the person at the login form, which
// is what keeps this unauthenticated route from having login CSRF (issue #6).
func (db *DB) Redeem(ctx context.Context, id int64, u NewUser, redeemed domain.Event, userCreated domain.Event) (domain.User, domain.Invitation, error) {
	var (
		out domain.User
		inv domain.Invitation
	)
	err := db.Tx(ctx, func(conn *sqlite.Conn) error {
		err := run(conn, fmt.Sprintf("redeeming invitation %d", id), `
			UPDATE invitations
			   SET status = 'redeemed', token_sha256 = NULL, settled_at = :at
			 WHERE id = :id AND status = 'pending'`,
			func(stmt *sqlite.Stmt) {
				stmt.SetText(":at", formatTime(u.CreatedAt))
				stmt.SetInt64(":id", id)
			}, nil)
		if err != nil {
			return err
		}
		if conn.Changes() == 0 {
			// Somebody else redeemed or revoked it between the read and here.
			// The message is for the log; the caller replaces it with the one
			// answer every redemption failure gets.
			return fmt.Errorf("invitation %d is no longer open: %w", id, domain.ErrConflict)
		}

		err = run(conn, "creating user "+u.Email, `
			INSERT INTO users (uid, email, name, active, created_at, password_hash)
			VALUES (:uid, :email, :name, 1, :created_at, :password_hash)`,
			func(stmt *sqlite.Stmt) {
				stmt.SetText(":uid", u.UID)
				stmt.SetText(":email", u.Email)
				stmt.SetText(":name", u.Name)
				stmt.SetText(":created_at", formatTime(u.CreatedAt))
				stmt.SetText(":password_hash", u.PasswordHash)
			}, nil)
		if err != nil {
			return err
		}
		userID := conn.LastInsertRowID()

		err = run(conn, fmt.Sprintf("recording the account for invitation %d", id),
			`UPDATE invitations SET user_id = :user_id WHERE id = :id`,
			func(stmt *sqlite.Stmt) {
				stmt.SetInt64(":user_id", userID)
				stmt.SetInt64(":id", id)
			}, nil)
		if err != nil {
			return err
		}

		// The account first, then how it came to exist: an event's actor is
		// the new user in both cases, because nobody else was present.
		userCreated.ActorID = userID
		userCreated.SubjectKind = domain.SubjectUser
		userCreated.SubjectID = userID
		if _, err := recordEvent(conn, userCreated); err != nil {
			return err
		}
		redeemed.ActorID = userID
		redeemed.SubjectKind = domain.SubjectInvitation
		redeemed.SubjectID = id
		if _, err := recordEvent(conn, redeemed); err != nil {
			return err
		}

		if err := userByID(conn, userID, &out); err != nil {
			return err
		}
		return invitationByID(conn, id, &inv)
	})
	return out, inv, err
}

func invitationByID(conn *sqlite.Conn, id int64, inv *domain.Invitation) error {
	return one(conn, fmt.Sprintf("invitation %d", id),
		invitationSelect+` WHERE i.id = :id`,
		func(stmt *sqlite.Stmt) { stmt.SetInt64(":id", id) },
		func(stmt *sqlite.Stmt) error {
			var err error
			*inv, err = scanInvitation(stmt)
			return err
		})
}

func scanInvitation(stmt *sqlite.Stmt) (domain.Invitation, error) {
	id := stmt.GetInt64("id")
	expires, err := parseTime(stmt.GetText("expires_at"))
	if err != nil {
		return domain.Invitation{}, fmt.Errorf("invitation %d: expires_at: %w", id, err)
	}
	created, err := parseTime(stmt.GetText("created_at"))
	if err != nil {
		return domain.Invitation{}, fmt.Errorf("invitation %d: created_at: %w", id, err)
	}
	settled, err := parseTime(stmt.GetText("settled_at"))
	if err != nil {
		return domain.Invitation{}, fmt.Errorf("invitation %d: settled_at: %w", id, err)
	}
	inv := domain.Invitation{
		ID:        id,
		UID:       stmt.GetText("uid"),
		Email:     stmt.GetText("email"),
		Status:    domain.InvitationStatus(stmt.GetText("status")),
		ExpiresAt: expires,
		CreatedAt: created,
		SettledAt: settled,
	}
	if v := nullText(stmt, "token_sha256"); v != nil {
		inv.TokenHash = *v
	}
	if v := nullText(stmt, "reason"); v != nil {
		inv.Reason = *v
	}
	if v := nullInt64(stmt, "invited_by"); v != nil {
		inv.InvitedBy = *v
	}
	if v := nullText(stmt, "inviter_uid"); v != nil {
		inv.InvitedByUID = *v
	}
	if v := nullText(stmt, "inviter_name"); v != nil {
		inv.InvitedByName = *v
	}
	if v := nullInt64(stmt, "user_id"); v != nil {
		inv.UserID = *v
	}
	if v := nullText(stmt, "invitee_uid"); v != nil {
		inv.UserUID = *v
	}
	return inv, nil
}
