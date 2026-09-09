// Copyright (c) 2026 Michael D Henderson.

package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mdhender/bricolage/internal/domain"
	"zombiezen.com/go/sqlite"
)

// Queue queries and the assignment writer. All the SQL for M5 is here
// (invariant 2); internal/service decides who may ask and this decides
// nothing.
//
// Two things in this file are load-bearing beyond what they look like:
//
//   - Every clause the query builder can emit is one an index seeks on
//     (PLAN.md M5 acceptance 4). The migration that added those indexes,
//     0007, says which clause each one is for, and TestQueueQueriesUseAnIndex
//     asks SQLite rather than taking either file's word for it.
//   - "now" is a parameter, never datetime('now'). Overdue is computed against
//     the injected clock (invariant 3, PLAN.md M5 acceptance 5), and a SQL
//     function reading the wall clock would be a second, untestable source of
//     the time inside the one query that most needs a fake one.

// DocumentQuery is a queue query as the store performs it: a resolved filter
// and the instant "overdue" is measured against.
type DocumentQuery struct {
	Filter domain.DocumentFilter

	// Now is what Overdue compares due_at to. It is the caller's clock, and
	// it is only read when Filter.Overdue is set.
	Now time.Time
}

// documentQuerySQL builds the statement and the binder for one query.
//
// It is one function returning both halves so that a clause can never be added
// to the text without its parameter: the two drifting apart is how a query
// silently matches everything. It is unexported because the only caller
// outside QueryDocuments is the test that runs EXPLAIN QUERY PLAN over exactly
// what QueryDocuments runs -- asserting on a hand-written approximation of the
// query would prove nothing about the query.
func documentQuerySQL(q DocumentQuery) (string, func(*sqlite.Stmt)) {
	f := q.Filter.Normalize()

	var (
		where []string
		binds []func(*sqlite.Stmt)
	)

	if f.State != "" {
		where = append(where, "d.state = :state")
		binds = append(binds, func(stmt *sqlite.Stmt) { stmt.SetText(":state", f.State) })
	}
	if f.SiteID != 0 {
		where = append(where, "d.site_id = :site_id")
		binds = append(binds, func(stmt *sqlite.Stmt) { stmt.SetInt64(":site_id", f.SiteID) })
	}
	if f.AssignedTo != 0 {
		where = append(where, "d.assigned_to = :assigned_to")
		binds = append(binds, func(stmt *sqlite.Stmt) { stmt.SetInt64(":assigned_to", f.AssignedTo) })
	}
	if f.Unassigned {
		// SQLite treats IS NULL as an equality constraint, so this seeks on
		// documents_assignee rather than scanning. That is the whole reason
		// 0007 replaced the partial index over the assigned rows: a partial
		// index may only be used when the query's WHERE implies the index's,
		// and this clause implies its opposite.
		where = append(where, "d.assigned_to IS NULL")
	}
	if f.Overdue {
		// "IS NOT NULL" is stated rather than implied so that documents_overdue,
		// which is partial on exactly that condition, is usable. A document
		// with no due date is not overdue: the absence of a deadline is not a
		// deadline in the past.
		where = append(where, "d.due_at IS NOT NULL AND d.due_at <= :now")
		binds = append(binds, func(stmt *sqlite.Stmt) { stmt.SetText(":now", formatTime(q.Now)) })
	}

	query := `
		SELECT ` + documentColumns + `
		  FROM documents d
		  JOIN element_types et ON et.id = d.element_type_id`
	if len(where) > 0 {
		query += "\n\t\t WHERE " + strings.Join(where, "\n\t\t   AND ")
	}

	// Soonest deadline first, undated last, newest first within a tie. A
	// queue is read top to bottom by somebody deciding what to do next, and
	// "no due date" is not "due at the beginning of time" -- which is what a
	// plain ORDER BY due_at would make it, since SQLite sorts NULLs first.
	query += `
		 ORDER BY d.due_at IS NULL, d.due_at, d.id DESC
		 LIMIT :limit`
	binds = append(binds, func(stmt *sqlite.Stmt) { stmt.SetInt64(":limit", int64(f.Limit)) })

	return query, func(stmt *sqlite.Stmt) {
		for _, bind := range binds {
			bind(stmt)
		}
	}
}

// QueryDocuments returns the documents matching a filter (PLAN.md M5).
//
// It performs no authorization. The rule is authz.Resolve, a pure function
// over grants the caller already holds, and a second implementation of it in
// SQL is a second implementation that will disagree; internal/service filters
// what this returns.
func (db *DB) QueryDocuments(ctx context.Context, q DocumentQuery) ([]domain.Document, error) {
	if err := q.Filter.Validate(); err != nil {
		return nil, err
	}
	query, bind := documentQuerySQL(q)

	var out []domain.Document
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return run(conn, "listing "+q.Filter.Describe(), query, bind, func(stmt *sqlite.Stmt) error {
			d, err := scanDocument(stmt)
			if err != nil {
				return err
			}
			out = append(out, d)
			return nil
		})
	})
	return out, err
}

// AssignmentUpdate is a change to who has a document and when it is due.
//
// Both fields carry three states, the way TransitionOutcome's do: nil leaves
// the column alone, a non-nil value writes it, and the zero value of what is
// pointed at -- user 0, the zero time -- writes NULL. "Nobody" and "no
// deadline" are outcomes somebody chose rather than fields somebody forgot.
type AssignmentUpdate struct {
	DocumentID int64

	SetAssignee *int64
	SetDueAt    *time.Time

	Now time.Time

	// Event is recorded in the same transaction as the change (invariant 7).
	// Its subject is filled in here.
	Event domain.Event
}

// UpdateAssignment writes an assignment, a due date, or both, and records the
// event, in one transaction.
//
// It writes both columns in one statement for the reason writeState does: a
// document handed to somebody with a deadline is one change, and two
// statements would leave a window in which it is theirs and not yet due, with
// a crash in that window making the window permanent.
//
// It does not touch documents.state and there is no way to make it: the state
// machine is internal/workflow's (invariant 4), and assigning work is not a
// move through it. An editor may hand a draft to a writer without the document
// going anywhere, which is exactly what makes assignment a separate operation
// rather than an effect somebody has to find a transition for.
func (db *DB) UpdateAssignment(ctx context.Context, u AssignmentUpdate) (domain.Document, error) {
	if u.SetAssignee == nil && u.SetDueAt == nil {
		return domain.Document{}, fmt.Errorf("assignment: nothing to change: %w", domain.ErrInvalid)
	}
	var out domain.Document
	err := db.Tx(ctx, func(conn *sqlite.Conn) error {
		err := run(conn, fmt.Sprintf("assigning document %d", u.DocumentID), `
			UPDATE documents
			   SET assigned_to = CASE WHEN :set_assignee THEN :assigned_to ELSE assigned_to END,
			       due_at      = CASE WHEN :set_due      THEN :due_at      ELSE due_at      END,
			       updated_at  = :now
			 WHERE id = :id`,
			func(stmt *sqlite.Stmt) {
				stmt.SetBool(":set_assignee", u.SetAssignee != nil)
				if u.SetAssignee != nil && *u.SetAssignee != 0 {
					stmt.SetInt64(":assigned_to", *u.SetAssignee)
				} else {
					stmt.SetNull(":assigned_to")
				}
				stmt.SetBool(":set_due", u.SetDueAt != nil)
				if u.SetDueAt != nil && !u.SetDueAt.IsZero() {
					stmt.SetText(":due_at", formatTime(*u.SetDueAt))
				} else {
					stmt.SetNull(":due_at")
				}
				stmt.SetText(":now", formatTime(u.Now))
				stmt.SetInt64(":id", u.DocumentID)
			}, nil)
		if err != nil {
			return err
		}
		if conn.Changes() != 1 {
			return notFound(fmt.Sprintf("document %d", u.DocumentID))
		}

		u.Event.SubjectKind = domain.SubjectDocument
		u.Event.SubjectID = u.DocumentID
		if _, err := recordEvent(conn, u.Event); err != nil {
			return err
		}
		return documentByID(conn, u.DocumentID, &out)
	})
	return out, err
}
