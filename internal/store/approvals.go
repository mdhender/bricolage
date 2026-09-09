// Copyright (c) 2026 Michael D Henderson.

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/mdhender/bricolage/internal/domain"
	"zombiezen.com/go/sqlite"
)

// All the SQL for approvals (invariant 2).
//
// The table landed in 0005 with the guard that reads it and this file's
// writers came with it, because a guard whose data nothing can produce is a
// guard nobody has tested. M11 is the milestone that gives them an API, and
// what changed here is that a write now carries its event: an approval is a
// state change, and a state change without an event in the same transaction is
// a state change with no audit record (invariant 7).

// approvalColumns is the projection every approval read shares.
const approvalColumns = `id, document_id, version_id, state, user_id, created_at`

// NewApproval is what CreateApproval is given.
type NewApproval struct {
	DocumentID int64
	VersionID  int64

	// State is the state the approval was given in. The guard counts the
	// approvals of the state a document is leaving.
	State     string
	UserID    int64
	CreatedAt time.Time
}

// CreateApproval records one person's sign-off of one version in one state,
// and reports whether it was new.
//
// Approving twice as the same person is idempotent: not an error, and not two
// rows (PLAN.md M11 acceptance 1). That is the ON CONFLICT below rather than a
// read-then-write, so two requests racing produce one row and one event rather
// than one row and a constraint violation somebody has to explain. The event
// is written only when a row was, because nothing changed the second time.
//
// The UNIQUE constraint on (version_id, state, user_id) is what makes "two
// distinct people must approve" a plain COUNT(*), and it is the same
// constraint that makes this idempotent. One index, both properties.
func (db *DB) CreateApproval(ctx context.Context, a NewApproval, event domain.Event) (domain.Approval, bool, error) {
	var (
		out     domain.Approval
		created bool
	)
	err := db.Write(ctx, func(conn *sqlite.Conn) error {
		what := fmt.Sprintf("approving version %d in %q", a.VersionID, a.State)
		err := run(conn, what, `
			INSERT INTO approvals (document_id, version_id, state, user_id, created_at)
			VALUES (:document_id, :version_id, :state, :user_id, :created_at)
			ON CONFLICT (version_id, state, user_id) DO NOTHING`,
			func(stmt *sqlite.Stmt) {
				stmt.SetInt64(":document_id", a.DocumentID)
				stmt.SetInt64(":version_id", a.VersionID)
				stmt.SetText(":state", a.State)
				stmt.SetInt64(":user_id", a.UserID)
				stmt.SetText(":created_at", formatTime(a.CreatedAt))
			}, nil)
		if err != nil {
			return err
		}
		created = conn.Changes() == 1

		if created {
			event.SubjectKind = domain.SubjectDocument
			event.SubjectID = a.DocumentID
			if _, err := recordEvent(conn, event); err != nil {
				return err
			}
		}
		// Read back rather than assemble, so that the caller is handed the
		// row that is actually there: on the second approval that is the
		// first one's, timestamp included, which is what "idempotent" has to
		// mean for the client rendering "approved at".
		return one(conn, what,
			`SELECT `+approvalColumns+`
			   FROM approvals
			  WHERE version_id = :version_id AND state = :state AND user_id = :user_id`,
			func(stmt *sqlite.Stmt) {
				stmt.SetInt64(":version_id", a.VersionID)
				stmt.SetText(":state", a.State)
				stmt.SetInt64(":user_id", a.UserID)
			},
			func(stmt *sqlite.Stmt) error {
				var err error
				out, err = scanApproval(stmt)
				return err
			})
	})
	return out, created, err
}

// WithdrawApproval removes one person's sign-off of one version in one state,
// and reports whether there was one to remove.
//
// Withdrawing an approval nobody recorded is not an error. DELETE is
// idempotent by definition and the two callers of this -- a person clicking
// twice and a client retrying a request whose answer was lost -- both mean
// "make sure my approval is not there". Nothing changed when there was nothing
// to remove, so nothing is written: no row, and no event.
func (db *DB) WithdrawApproval(ctx context.Context, documentID, versionID int64, state string, userID int64, event domain.Event) (bool, error) {
	var removed bool
	err := db.Write(ctx, func(conn *sqlite.Conn) error {
		err := run(conn, fmt.Sprintf("withdrawing the approval of version %d in %q", versionID, state), `
			DELETE FROM approvals
			 WHERE version_id = :version_id AND state = :state AND user_id = :user_id`,
			func(stmt *sqlite.Stmt) {
				stmt.SetInt64(":version_id", versionID)
				stmt.SetText(":state", state)
				stmt.SetInt64(":user_id", userID)
			}, nil)
		if err != nil {
			return err
		}
		removed = conn.Changes() == 1
		if !removed {
			return nil
		}
		event.SubjectKind = domain.SubjectDocument
		event.SubjectID = documentID
		_, err = recordEvent(conn, event)
		return err
	})
	return removed, err
}

// ApprovalsForVersion returns every approval recorded against one version,
// oldest first.
func (db *DB) ApprovalsForVersion(ctx context.Context, versionID int64) ([]domain.Approval, error) {
	var out []domain.Approval
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return run(conn, fmt.Sprintf("approvals of version %d", versionID),
			`SELECT `+approvalColumns+` FROM approvals WHERE version_id = :version_id ORDER BY id`,
			func(stmt *sqlite.Stmt) { stmt.SetInt64(":version_id", versionID) },
			func(stmt *sqlite.Stmt) error {
				a, err := scanApproval(stmt)
				if err != nil {
					return err
				}
				out = append(out, a)
				return nil
			})
	})
	return out, err
}

// ApprovalsForDocument returns every approval a document has ever collected,
// oldest first, whichever version it was given against.
//
// It is what makes "the old version's approvals remain queryable" a fact
// rather than a hope (PLAN.md M11 acceptance 3). Checking in a new version
// drops the guard's count to zero because the guard counts one version's
// approvals; the rows themselves are history and history does not move.
func (db *DB) ApprovalsForDocument(ctx context.Context, documentID int64) ([]domain.Approval, error) {
	var out []domain.Approval
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return run(conn, fmt.Sprintf("approvals of document %d", documentID),
			`SELECT `+approvalColumns+` FROM approvals WHERE document_id = :document_id ORDER BY id`,
			func(stmt *sqlite.Stmt) { stmt.SetInt64(":document_id", documentID) },
			func(stmt *sqlite.Stmt) error {
				a, err := scanApproval(stmt)
				if err != nil {
					return err
				}
				out = append(out, a)
				return nil
			})
	})
	return out, err
}

// CountApprovals returns how many approvals a version carries in one state.
func (db *DB) CountApprovals(ctx context.Context, versionID int64, state string) (int, error) {
	var n int
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		byState, err := approvalsByState(conn, versionID)
		n = byState[state]
		return err
	})
	return n, err
}

// approvalsByState counts a version's approvals, keyed by the state they were
// given in. A version id of 0 has none.
func approvalsByState(conn *sqlite.Conn, versionID int64) (map[string]int, error) {
	out := map[string]int{}
	if versionID == 0 {
		return out, nil
	}
	err := run(conn, fmt.Sprintf("approvals of version %d", versionID), `
		SELECT state AS state, COUNT(*) AS n
		  FROM approvals
		 WHERE version_id = :version_id
		 GROUP BY state`,
		func(stmt *sqlite.Stmt) { stmt.SetInt64(":version_id", versionID) },
		func(stmt *sqlite.Stmt) error {
			out[stmt.GetText("state")] = int(stmt.GetInt64("n"))
			return nil
		})
	return out, err
}

// clearApprovals is EffectClearApprovals: discard the approvals recorded
// against one version (PLAN.md M11 acceptance 4).
func clearApprovals(conn *sqlite.Conn, versionID int64) error {
	return run(conn, fmt.Sprintf("clearing the approvals of version %d", versionID),
		`DELETE FROM approvals WHERE version_id = :version_id`,
		func(stmt *sqlite.Stmt) { stmt.SetInt64(":version_id", versionID) }, nil)
}

func scanApproval(stmt *sqlite.Stmt) (domain.Approval, error) {
	id := stmt.GetInt64("id")
	at, err := parseTime(stmt.GetText("created_at"))
	if err != nil {
		return domain.Approval{}, fmt.Errorf("approval %d: created_at: %w", id, err)
	}
	return domain.Approval{
		ID:         id,
		DocumentID: stmt.GetInt64("document_id"),
		VersionID:  stmt.GetInt64("version_id"),
		State:      stmt.GetText("state"),
		UserID:     stmt.GetInt64("user_id"),
		CreatedAt:  at,
	}, nil
}
