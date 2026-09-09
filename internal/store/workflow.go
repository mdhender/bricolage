// Copyright (c) 2026 Michael D Henderson.

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/mdhender/bricolage/internal/domain"
	"zombiezen.com/go/sqlite"
)

// Workflows, transitions, approvals, and comments. All the SQL for M4 is here
// (invariant 2); internal/workflow decides what may happen and this decides
// nothing.
//
// This file holds the one statement in the repository that writes
// documents.state, and it holds it inside ApplyTransition, which cannot be
// reached without supplying the decision function that authorised the move.
// DESIGN.md 6.3 says to keep the state-writing statement inside
// internal/workflow and to give the store no exported method that sets state
// alone; invariant 2 says no SQL string appears outside this package, ever.
// Both are satisfied by the shape below rather than by choosing between them:
// the SQL is here, the decision is in internal/workflow, and there is no way
// to move a document without one. "make lint" asserts both halves --
// documents.state is written only in this file, and ApplyTransition has
// exactly one non-test caller, in internal/workflow.

// workflowColumns is the projection every workflow read shares.
const workflowColumns = `id, uid, site_id, kind, name, initial_state`

// CreateApproval and the comment writers below have no API in front of them
// until M11 (PLAN.md M4, "Schema"). They are here because the guards that read
// their tables are enforced now, and a guard whose data nothing can produce is
// a guard nobody has tested (invariant 6).

// WorkflowByUID reads one workflow with its states and transitions.
func (db *DB) WorkflowByUID(ctx context.Context, uid string) (domain.Workflow, error) {
	var w domain.Workflow
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		if err := one(conn, fmt.Sprintf("workflow %q", uid),
			`SELECT `+workflowColumns+` FROM workflows WHERE uid = :uid`,
			func(stmt *sqlite.Stmt) { stmt.SetText(":uid", uid) },
			func(stmt *sqlite.Stmt) error {
				w = scanWorkflow(stmt)
				return nil
			}); err != nil {
			return err
		}
		return loadWorkflowParts(conn, &w)
	})
	return w, err
}

// WorkflowByID reads one workflow with its states and transitions.
func (db *DB) WorkflowByID(ctx context.Context, id int64) (domain.Workflow, error) {
	var w domain.Workflow
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return workflowByID(conn, id, &w)
	})
	return w, err
}

func workflowByID(conn *sqlite.Conn, id int64, w *domain.Workflow) error {
	if err := one(conn, fmt.Sprintf("workflow %d", id),
		`SELECT `+workflowColumns+` FROM workflows WHERE id = :id`,
		func(stmt *sqlite.Stmt) { stmt.SetInt64(":id", id) },
		func(stmt *sqlite.Stmt) error {
			*w = scanWorkflow(stmt)
			return nil
		}); err != nil {
		return err
	}
	return loadWorkflowParts(conn, w)
}

// WorkflowFor finds the workflow that governs a new document of this kind on
// this site.
//
// A workflow with no site applies to every site, so a site-specific workflow
// wins over the general one and the general one is the fallback -- which is
// the ORDER BY below and the reason it is not "site_id = :site OR site_id IS
// NULL" with no order at all. There is no default beyond that: a kind with no
// workflow is a hard failure naming the kind, because placing a document in
// somebody else's process is worse than refusing to place it.
func (db *DB) WorkflowFor(ctx context.Context, kind string, siteID int64) (domain.Workflow, error) {
	var w domain.Workflow
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		err := one(conn, fmt.Sprintf("a workflow for %s documents on site %d", kind, siteID), `
			SELECT `+workflowColumns+`
			  FROM workflows
			 WHERE kind = :kind AND (site_id = :site_id OR site_id IS NULL)
			 ORDER BY site_id IS NULL, id
			 LIMIT 1`,
			func(stmt *sqlite.Stmt) {
				stmt.SetText(":kind", kind)
				stmt.SetInt64(":site_id", siteID)
			},
			func(stmt *sqlite.Stmt) error {
				w = scanWorkflow(stmt)
				return nil
			})
		if err != nil {
			return err
		}
		return loadWorkflowParts(conn, &w)
	})
	return w, err
}

// ListWorkflows returns every workflow, with its states and transitions, in id
// order. It is what "cmsdb seed" reports and what the admin screens list.
func (db *DB) ListWorkflows(ctx context.Context) ([]domain.Workflow, error) {
	var out []domain.Workflow
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		err := run(conn, "listing workflows",
			`SELECT `+workflowColumns+` FROM workflows ORDER BY id`, nil,
			func(stmt *sqlite.Stmt) error {
				out = append(out, scanWorkflow(stmt))
				return nil
			})
		if err != nil {
			return err
		}
		for i := range out {
			if err := loadWorkflowParts(conn, &out[i]); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

// loadWorkflowParts fills in a workflow's states and transitions and validates
// the result.
//
// The validation is not decoration. A guard or an effect naming something
// outside the closed vocabulary is refused here, when the row is read, rather
// than ignored when the transition runs: a configured guard this binary does
// not enforce must stop the transition, not run it unguarded (invariant 6).
func loadWorkflowParts(conn *sqlite.Conn, w *domain.Workflow) error {
	err := run(conn, fmt.Sprintf("states of workflow %d", w.ID), `
		SELECT workflow_id, slug, name, position, publishable, terminal, required_approvals
		  FROM workflow_states
		 WHERE workflow_id = :workflow_id
		 ORDER BY position, slug`,
		func(stmt *sqlite.Stmt) { stmt.SetInt64(":workflow_id", w.ID) },
		func(stmt *sqlite.Stmt) error {
			w.States = append(w.States, domain.WorkflowState{
				WorkflowID:        stmt.GetInt64("workflow_id"),
				Slug:              stmt.GetText("slug"),
				Name:              stmt.GetText("name"),
				Position:          int(stmt.GetInt64("position")),
				Publishable:       stmt.GetInt64("publishable") != 0,
				Terminal:          stmt.GetInt64("terminal") != 0,
				RequiredApprovals: int(stmt.GetInt64("required_approvals")),
			})
			return nil
		})
	if err != nil {
		return err
	}

	err = run(conn, fmt.Sprintf("transitions of workflow %d", w.ID), `
		SELECT id, workflow_id, from_state, to_state, name, privilege, guards, effects, position
		  FROM workflow_transitions
		 WHERE workflow_id = :workflow_id
		 ORDER BY position, id`,
		func(stmt *sqlite.Stmt) { stmt.SetInt64(":workflow_id", w.ID) },
		func(stmt *sqlite.Stmt) error {
			t, err := scanTransition(stmt)
			if err != nil {
				return err
			}
			w.Transitions = append(w.Transitions, t)
			return nil
		})
	if err != nil {
		return err
	}
	return w.Validate()
}

func scanWorkflow(stmt *sqlite.Stmt) domain.Workflow {
	w := domain.Workflow{
		ID:           stmt.GetInt64("id"),
		UID:          stmt.GetText("uid"),
		Kind:         stmt.GetText("kind"),
		Name:         stmt.GetText("name"),
		InitialState: stmt.GetText("initial_state"),
	}
	if v := nullInt64(stmt, "site_id"); v != nil {
		w.SiteID = *v
	}
	return w
}

func scanTransition(stmt *sqlite.Stmt) (domain.Transition, error) {
	t := domain.Transition{
		ID:         stmt.GetInt64("id"),
		WorkflowID: stmt.GetInt64("workflow_id"),
		From:       stmt.GetText("from_state"),
		To:         stmt.GetText("to_state"),
		Name:       stmt.GetText("name"),
		Privilege:  domain.Privilege(stmt.GetInt64("privilege")),
		Position:   int(stmt.GetInt64("position")),
	}
	var err error
	if t.Guards, err = domain.ParseGuards(stmt.GetText("guards")); err != nil {
		return domain.Transition{}, fmt.Errorf("transition %d: %w", t.ID, err)
	}
	if t.Effects, err = domain.ParseEffects(stmt.GetText("effects")); err != nil {
		return domain.Transition{}, fmt.Errorf("transition %d: %w", t.ID, err)
	}
	return t, nil
}

// TransitionFacts is everything a guard reads, loaded in one place so that
// Available and Do decide from the same information (invariant 5).
//
// Do loads it inside the transaction that performs the move, so the facts a
// guard was checked against are the facts the write is applied to.
type TransitionFacts struct {
	// Document is the row as it stands, freshly read.
	Document domain.Document

	// Version is the current version, which is what has a slug and a cover
	// date. It is the zero value for a document with no version, which cannot
	// happen today -- a document is created with its first draft -- and which
	// a guard must still not panic on.
	Version domain.Version

	// ApprovalsByState counts the approvals recorded against the current
	// version, keyed by the state they were given in. It is of the current
	// version only: adding a version drops every count to zero, which is what
	// makes "changes invalidate sign-off" true (DESIGN.md 5.5).
	ApprovalsByState map[string]int

	// UnresolvedComments is how many open comments the document carries.
	UnresolvedComments int
}

// TransitionOutcome is what the engine decided: the new state, the effects to
// apply, and the event to record.
//
// The two pointer fields carry three states each. Nil leaves the column alone;
// a non-nil value writes it; and the zero value of what is pointed at -- user
// 0, the zero time -- writes NULL, because "unassigned" and "no due date" are
// real outcomes rather than missing ones.
type TransitionOutcome struct {
	// State is the state to move to. It is validated against the composite
	// foreign key, so a state the workflow does not declare is refused by the
	// database as well as by the engine.
	State string

	// SetAssignee is EffectAssignToActor and EffectClearAssignee.
	SetAssignee *int64

	// SetDueAt is EffectSetDueIn.
	SetDueAt *time.Time

	// ClearApprovals is EffectClearApprovals: discard the approvals recorded
	// against the current version.
	ClearApprovals bool

	// Event is recorded in the same transaction as the move (invariant 7). Its
	// subject is filled in here.
	Event domain.Event
}

// TransitionRequest is what ApplyTransition is given.
//
// There is no actor here. Who performed the move is the event's business, and
// the event is built by the engine and handed over in the outcome; a second
// copy of the actor on this struct would be a field nothing reads, which is the
// shape invariant 6 is about.
type TransitionRequest struct {
	DocumentID int64
	Now        time.Time

	// Decide is the engine's check, run against facts loaded inside the
	// transaction. Returning an error rolls everything back and the error is
	// returned unchanged, which is what makes PLAN.md M4 acceptance 6 true:
	// a failed guard leaves the state, the events, and the jobs as they were.
	//
	// This is the whole of the store's part in the decision. It reads rows,
	// hands them over, and writes what it is told; the rule lives in
	// internal/workflow and nowhere else.
	Decide func(f TransitionFacts) (TransitionOutcome, error)
}

// ApplyTransition performs one transition in one transaction (DESIGN.md 6.4):
// load, check, update, event.
//
// The UPDATE is conditional on the state the check was made against, so two
// concurrent transitions of the same document are two statements against one
// row and SQLite decides which of them finds it in the expected state. A
// read-then-write would let both reads see "draft" and both writes succeed,
// with the second silently overwriting the first's move.
//
// Enqueuing jobs is the fourth step DESIGN.md 6.4 names, and it arrives with
// the queue in M6. There is deliberately no empty hook for it here: a
// parameter nothing fills is the rule column nothing reads (invariant 6).
func (db *DB) ApplyTransition(ctx context.Context, req TransitionRequest) (domain.Document, error) {
	var out domain.Document
	err := db.Tx(ctx, func(conn *sqlite.Conn) error {
		var facts TransitionFacts
		if err := documentByID(conn, req.DocumentID, &facts.Document); err != nil {
			return err
		}
		if facts.Document.CurrentVersionID != 0 {
			if err := versionByID(conn, facts.Document.CurrentVersionID, &facts.Version); err != nil {
				return err
			}
		}
		var err error
		if facts.ApprovalsByState, err = approvalsByState(conn, facts.Document.CurrentVersionID); err != nil {
			return err
		}
		if facts.UnresolvedComments, err = unresolvedComments(conn, req.DocumentID); err != nil {
			return err
		}

		outcome, err := req.Decide(facts)
		if err != nil {
			return err
		}

		from := facts.Document.State
		changed, err := writeState(conn, req.DocumentID, from, outcome, req.Now)
		if err != nil {
			return err
		}
		if !changed {
			// The row moved between the read and the write, inside this
			// transaction's snapshot only if another writer committed first.
			// Either way the check was made against a state the document is
			// no longer in.
			return fmt.Errorf("document %d moved out of %q while the transition was being checked: %w",
				req.DocumentID, from, domain.ErrConflict)
		}

		if outcome.ClearApprovals && facts.Document.CurrentVersionID != 0 {
			if err := clearApprovals(conn, facts.Document.CurrentVersionID); err != nil {
				return err
			}
		}

		outcome.Event.SubjectKind = domain.SubjectDocument
		outcome.Event.SubjectID = req.DocumentID
		if _, err := recordEvent(conn, outcome.Event); err != nil {
			return err
		}
		return documentByID(conn, req.DocumentID, &out)
	})
	return out, err
}

// writeState is the one statement in this repository that writes
// documents.state.
//
// It writes the effects in the same UPDATE, because a state change and the
// assignment that goes with it are one change: two statements would leave a
// window in which a document is in review and still assigned to the person who
// submitted it, and a crash in that window would make the window permanent.
func writeState(conn *sqlite.Conn, documentID int64, from string, out TransitionOutcome, now time.Time) (bool, error) {
	query := `
		UPDATE documents
		   SET state = :state,
		       assigned_to = CASE WHEN :set_assignee THEN :assigned_to ELSE assigned_to END,
		       due_at = CASE WHEN :set_due THEN :due_at ELSE due_at END,
		       updated_at = :now
		 WHERE id = :id AND state = :from`
	err := run(conn, fmt.Sprintf("moving document %d from %q to %q", documentID, from, out.State), query,
		func(stmt *sqlite.Stmt) {
			stmt.SetText(":state", out.State)
			stmt.SetBool(":set_assignee", out.SetAssignee != nil)
			if out.SetAssignee != nil && *out.SetAssignee != 0 {
				stmt.SetInt64(":assigned_to", *out.SetAssignee)
			} else {
				stmt.SetNull(":assigned_to")
			}
			stmt.SetBool(":set_due", out.SetDueAt != nil)
			if out.SetDueAt != nil && !out.SetDueAt.IsZero() {
				stmt.SetText(":due_at", formatTime(*out.SetDueAt))
			} else {
				stmt.SetNull(":due_at")
			}
			stmt.SetText(":now", formatTime(now))
			stmt.SetInt64(":id", documentID)
			stmt.SetText(":from", from)
		}, nil)
	if err != nil {
		return false, err
	}
	return conn.Changes() == 1, nil
}

// versionByID reads one version by its primary key, on a connection the caller
// holds. The id always comes from a row this process already read
// (invariant 10).
func versionByID(conn *sqlite.Conn, id int64, v *domain.Version) error {
	return one(conn, fmt.Sprintf("version %d", id),
		`SELECT `+versionColumns+` FROM document_versions WHERE id = :id`,
		func(stmt *sqlite.Stmt) { stmt.SetInt64(":id", id) },
		func(stmt *sqlite.Stmt) error {
			var err error
			*v, err = scanVersion(stmt)
			return err
		})
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

// unresolvedComments counts the document's open comments.
func unresolvedComments(conn *sqlite.Conn, documentID int64) (int, error) {
	var n int
	err := one(conn, fmt.Sprintf("open comments on document %d", documentID),
		`SELECT COUNT(*) AS n FROM comments WHERE document_id = :document_id AND resolved_at IS NULL`,
		func(stmt *sqlite.Stmt) { stmt.SetInt64(":document_id", documentID) },
		func(stmt *sqlite.Stmt) error {
			n = int(stmt.GetInt64("n"))
			return nil
		})
	return n, err
}

func clearApprovals(conn *sqlite.Conn, versionID int64) error {
	return run(conn, fmt.Sprintf("clearing the approvals of version %d", versionID),
		`DELETE FROM approvals WHERE version_id = :version_id`,
		func(stmt *sqlite.Stmt) { stmt.SetInt64(":version_id", versionID) }, nil)
}

// TransitionFactsFor loads the same facts ApplyTransition loads, for a caller
// that is only asking what would happen.
//
// It is the read half of invariant 5: Available renders from this and Do
// decides from the transaction's copy of it, and both hand the result to one
// check function. A second loader here -- a query that fetched slightly
// different facts for the menu than for the action -- is exactly the defect
// the invariant exists to prevent.
func (db *DB) TransitionFactsFor(ctx context.Context, documentID int64) (TransitionFacts, error) {
	var facts TransitionFacts
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		if err := documentByID(conn, documentID, &facts.Document); err != nil {
			return err
		}
		if facts.Document.CurrentVersionID != 0 {
			if err := versionByID(conn, facts.Document.CurrentVersionID, &facts.Version); err != nil {
				return err
			}
		}
		var err error
		if facts.ApprovalsByState, err = approvalsByState(conn, facts.Document.CurrentVersionID); err != nil {
			return err
		}
		facts.UnresolvedComments, err = unresolvedComments(conn, documentID)
		return err
	})
	return facts, err
}

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

// CreateApproval records one person's sign-off of one version in one state.
//
// The UNIQUE constraint on (version_id, state, user_id) makes "two distinct
// people must approve" a plain COUNT(*), and it makes a second approval by the
// same person a *ConstraintError answering to domain.ErrConflict rather than a
// silently doubled count.
//
// The API that writes one is M11 (PLAN.md M4, "Schema"). This is here because
// GuardApprovalsMet is enforced now, and a guard whose data nothing can produce
// is a guard nobody has tested.
func (db *DB) CreateApproval(ctx context.Context, a NewApproval) (int64, error) {
	var id int64
	err := db.Write(ctx, func(conn *sqlite.Conn) error {
		err := run(conn, fmt.Sprintf("approving version %d in %q", a.VersionID, a.State), `
			INSERT INTO approvals (document_id, version_id, state, user_id, created_at)
			VALUES (:document_id, :version_id, :state, :user_id, :created_at)`,
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
		id = conn.LastInsertRowID()
		return nil
	})
	return id, err
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

// NewComment is what CreateComment is given.
type NewComment struct {
	UID        string
	DocumentID int64

	// VersionID is the version the comment is about, or 0 for a comment about
	// the document as a whole.
	VersionID int64

	AuthorID  int64
	Body      string
	CreatedAt time.Time
}

// CreateComment opens a comment thread on a document.
//
// Comments are a thread; the system we learned from had one overwritten note
// per version for its entire collaboration story. The thread API is M11
// (DESIGN.md 5.5). This writer is here for the same reason CreateApproval is:
// GuardCommentsResolved is enforced now, and PLAN.md M4 acceptance 3 asks for
// a test in which each of the seven guards refuses.
func (db *DB) CreateComment(ctx context.Context, c NewComment) (int64, error) {
	var id int64
	err := db.Write(ctx, func(conn *sqlite.Conn) error {
		err := run(conn, fmt.Sprintf("commenting on document %d", c.DocumentID), `
			INSERT INTO comments (uid, document_id, version_id, author_id, body, created_at)
			VALUES (:uid, :document_id, :version_id, :author_id, :body, :created_at)`,
			func(stmt *sqlite.Stmt) {
				stmt.SetText(":uid", c.UID)
				stmt.SetInt64(":document_id", c.DocumentID)
				if c.VersionID == 0 {
					stmt.SetNull(":version_id")
				} else {
					stmt.SetInt64(":version_id", c.VersionID)
				}
				stmt.SetInt64(":author_id", c.AuthorID)
				stmt.SetText(":body", c.Body)
				stmt.SetText(":created_at", formatTime(c.CreatedAt))
			}, nil)
		if err != nil {
			return err
		}
		id = conn.LastInsertRowID()
		return nil
	})
	return id, err
}

// ResolveComment closes a comment thread.
func (db *DB) ResolveComment(ctx context.Context, id, userID int64, now time.Time) error {
	return db.Write(ctx, func(conn *sqlite.Conn) error {
		return run(conn, fmt.Sprintf("resolving comment %d", id), `
			UPDATE comments SET resolved_at = :now, resolved_by = :user_id
			 WHERE id = :id AND resolved_at IS NULL`,
			func(stmt *sqlite.Stmt) {
				stmt.SetText(":now", formatTime(now))
				stmt.SetInt64(":user_id", userID)
				stmt.SetInt64(":id", id)
			}, nil)
	})
}

// CountUnresolvedComments returns how many open comments a document carries.
func (db *DB) CountUnresolvedComments(ctx context.Context, documentID int64) (int, error) {
	var n int
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		var err error
		n, err = unresolvedComments(conn, documentID)
		return err
	})
	return n, err
}
