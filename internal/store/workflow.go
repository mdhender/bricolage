// Copyright (c) 2026 Michael D Henderson.

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/mdhender/bricolage/internal/domain"
	"zombiezen.com/go/sqlite"
)

// Workflows and transitions. All the SQL for M4 is here (invariant 2);
// internal/workflow decides what may happen and this decides nothing. The
// tables the two data-driven guards read have files of their own --
// approvals.go and comments.go -- and the helpers this file calls to load
// their counts live there with them.
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

// WorkflowUIDs returns every workflow's external identifier, keyed by its
// internal id (invariant 10).
//
// It is deliberately not ListWorkflows. That loads states and transitions and
// validates the result, so one half-configured process -- a workflows row
// written before its states, a guard this binary does not enforce -- fails the
// whole call. That is right when the caller is about to run the process and
// wrong when it only wants to name it: a document is readable whether or not
// some other workflow is misconfigured, and a document response losing its
// workflow name because of a row it has nothing to do with is collateral
// damage.
func (db *DB) WorkflowUIDs(ctx context.Context) (map[int64]string, error) {
	out := map[int64]string{}
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return run(conn, "listing workflow identifiers",
			`SELECT id, uid FROM workflows ORDER BY id`, nil,
			func(stmt *sqlite.Stmt) error {
				out[stmt.GetInt64("id")] = stmt.GetText("uid")
				return nil
			})
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

	// The validation failure is deliberately not wrapped with %w.
	//
	// domain.Transition.Validate answers to domain.ErrInvalid, which is right
	// when it is judging something a client sent and wrong here: the caller
	// asked a perfectly good question and the answer is that this database is
	// configured with a process this binary cannot run. Letting ErrInvalid
	// through would make the transport edge return 422 and tell the client
	// their request was malformed. Stripping it leaves an unclassified error,
	// which is a 500 -- the server's fault, logged for an operator, generic to
	// everybody else (DESIGN.md 14).
	if err := w.Validate(); err != nil {
		return fmt.Errorf("workflow %q (id %d) is configured in a way this binary cannot run: %v",
			w.Name, w.ID, err)
	}
	return nil
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
	// Neither failure is wrapped with %w, for the reason loadWorkflowParts
	// gives: domain.ErrInvalid is a judgement about something a client sent,
	// and a guards column this binary cannot parse is a judgement about this
	// database. One is a 422, the other is a 500.
	var err error
	if t.Guards, err = domain.ParseGuards(stmt.GetText("guards")); err != nil {
		return domain.Transition{}, fmt.Errorf("transition %q (id %d): %v", t.Name, t.ID, err)
	}
	if t.Effects, err = domain.ParseEffects(stmt.GetText("effects")); err != nil {
		return domain.Transition{}, fmt.Errorf("transition %q (id %d): %v", t.Name, t.ID, err)
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

	// UnresolvedComments is how many open comment threads the document
	// carries. It counts thread roots: resolution is a property of the
	// discussion, so a thread with three replies is one open question rather
	// than four (DESIGN.md 5.5).
	UnresolvedComments int

	// CheckedIn is the newest version that has been checked in, which is what
	// a publish pins (invariant 8, PLAN.md M9). It is separate from Version
	// because the two differ exactly when it matters: while somebody holds the
	// document checked out, the current version is the open working draft, and
	// EffectPublish must never pin that.
	//
	// It is the zero value for a document whose only version is its first
	// draft, and EffectPublish refuses rather than publishing nothing.
	CheckedIn domain.Version
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

	// Jobs are enqueued in the same transaction as the move, which is
	// DESIGN.md 6.4's fourth step. EffectPublish is what fills it today.
	//
	// The uid is minted by the engine rather than here, because a uid is a
	// ULID over an instant and this package has no clock (invariant 3). An
	// empty slice is the ordinary case and costs nothing.
	Jobs []NewJob

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
// Enqueuing jobs is the fourth step DESIGN.md 6.4 names, and M9 is the
// milestone that fills it: a transition declaring EffectPublish hands over a
// publish job in its outcome, and it is written here, inside the same
// transaction. Until something filled it there was deliberately no hook, for
// the reason invariant 6 gives.
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
		if err := loadCheckedIn(conn, req.DocumentID, &facts.CheckedIn); err != nil {
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

		// Step four of DESIGN.md 6.4, in the same transaction as the other
		// three. A move whose publish job was written by a second transaction
		// could commit the move and lose the publish, which is a document
		// that says "published" and a site that does not have it.
		for _, job := range outcome.Jobs {
			if _, err := enqueueJob(conn, job); err != nil {
				return err
			}
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

// TransitionFactsFor loads the same facts ApplyTransition loads, about a
// document the caller has already read.
//
// It is the read half of invariant 5: Available renders from this and Do
// decides from the transaction's copy of it, and both hand the result to one
// check function. A second loader here -- a query that fetched slightly
// different facts for the menu than for the action -- is exactly the defect
// the invariant exists to prevent.
//
// The document is passed in rather than re-read. Re-reading it here would put
// the row on a second connection at a second instant, so a caller could render
// "state: draft" above the transitions out of review; passing it in makes the
// menu and the state it is displayed under one snapshot. Do does not rely on
// that -- it reloads inside its transaction, which is where staleness would
// actually cost something.
func (db *DB) TransitionFactsFor(ctx context.Context, doc domain.Document) (TransitionFacts, error) {
	facts := TransitionFacts{Document: doc}
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		if doc.CurrentVersionID != 0 {
			if err := versionByID(conn, doc.CurrentVersionID, &facts.Version); err != nil {
				return err
			}
		}
		var err error
		if facts.ApprovalsByState, err = approvalsByState(conn, doc.CurrentVersionID); err != nil {
			return err
		}
		if facts.UnresolvedComments, err = unresolvedComments(conn, doc.ID); err != nil {
			return err
		}
		return loadCheckedIn(conn, doc.ID, &facts.CheckedIn)
	})
	return facts, err
}

// loadCheckedIn fills in the newest checked-in version, treating "there is
// none" as the zero value rather than as an error.
//
// A document whose only version is its first draft has never been checked in,
// which is an ordinary state and not a failure: every guard must cope with it,
// and EffectPublish is the one thing that refuses it, with a message about
// publishing rather than about a missing row.
func loadCheckedIn(conn *sqlite.Conn, documentID int64, v *domain.Version) error {
	err := latestCheckedIn(conn, documentID, v)
	if isNotFound(err) {
		*v = domain.Version{}
		return nil
	}
	return err
}
