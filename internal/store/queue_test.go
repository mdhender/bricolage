// Copyright (c) 2026 Michael D Henderson.

package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/events"
	"zombiezen.com/go/sqlite"
)

// The queue queries and the assignment writer, against a real in-memory
// database with every migration applied through the same runner cmsdb uses and
// with foreign keys on (DESIGN.md 15, invariant 22).

// assign is the shape every test here uses to hand a document over.
func (f *docFixture) assign(t *testing.T, doc domain.Document, userID int64, due time.Time) domain.Document {
	t.Helper()
	u := AssignmentUpdate{
		DocumentID:  doc.ID,
		SetAssignee: domain.Ref(userID),
		Now:         f.now,
		Event:       f.event(f.author.ID, events.DocumentAssigned),
	}
	if !due.IsZero() {
		u.SetDueAt = domain.Ref(due)
	}
	got, err := f.db.UpdateAssignment(t.Context(), u)
	if err != nil {
		t.Fatalf("UpdateAssignment: %v", err)
	}
	return got
}

// uids renders a result set for a failure message, since the ids mean nothing
// to a person reading one.
func uids(docs []domain.Document) []string {
	out := make([]string, 0, len(docs))
	for _, d := range docs {
		out = append(out, d.UID)
	}
	return out
}

// TestQueueFilters is PLAN.md M5 acceptance 1 at the store level, plus every
// other combination the filter can express.
//
// The fixture is four documents: one nobody has, one assigned and late, one
// assigned and not yet due, and one moved out of the initial state.
func TestQueueFilters(t *testing.T) {
	f := newDocFixture(t)
	now := f.now

	loose, _ := f.create(t, "Nobody's")
	late, _ := f.create(t, "Late")
	soon, _ := f.create(t, "Soon")
	other, _ := f.create(t, "Somebody else's")

	late = f.assign(t, late, f.author.ID, now.Add(-time.Hour))
	soon = f.assign(t, soon, f.author.ID, now.Add(time.Hour))
	other = f.assign(t, other, f.other.ID, time.Time{})

	// One document is moved out of the initial state, through the one path
	// that may move it: the whole point of a state filter is that documents
	// are in different states.
	moveTo(t, f, other, "review")

	for _, tc := range []struct {
		name   string
		filter domain.DocumentFilter
		want   []string
	}{
		{
			name:   "everything",
			filter: domain.DocumentFilter{},
			want:   []string{late.UID, soon.UID, other.UID, loose.UID},
		},
		{
			name:   "by state",
			filter: domain.DocumentFilter{State: "draft"},
			want:   []string{late.UID, soon.UID, loose.UID},
		},
		{
			name:   "by assignee",
			filter: domain.DocumentFilter{AssignedTo: f.author.ID},
			want:   []string{late.UID, soon.UID},
		},
		{
			name:   "unassigned",
			filter: domain.DocumentFilter{Unassigned: true},
			want:   []string{loose.UID},
		},
		{
			name:   "overdue",
			filter: domain.DocumentFilter{Overdue: true},
			want:   []string{late.UID},
		},
		{
			name:   "by site",
			filter: domain.DocumentFilter{SiteID: f.siteID},
			want:   []string{late.UID, soon.UID, other.UID, loose.UID},
		},
		{
			// PLAN.md M5 acceptance 1: the query the system we learned from
			// could not express.
			name:   "in review and unassigned",
			filter: domain.DocumentFilter{State: "review", Unassigned: true},
			want:   nil,
		},
		{
			name:   "in draft and unassigned",
			filter: domain.DocumentFilter{State: "draft", Unassigned: true},
			want:   []string{loose.UID},
		},
		{
			name:   "mine and overdue",
			filter: domain.DocumentFilter{AssignedTo: f.author.ID, Overdue: true},
			want:   []string{late.UID},
		},
		{
			name:   "a state nothing is in",
			filter: domain.DocumentFilter{State: "published"},
			want:   nil,
		},
		{
			name:   "a site nothing is on",
			filter: domain.DocumentFilter{SiteID: f.siteID + 1000},
			want:   nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := f.db.QueryDocuments(t.Context(), DocumentQuery{Filter: tc.filter, Now: now})
			if err != nil {
				t.Fatalf("QueryDocuments: %v", err)
			}
			if !equalStrings(uids(got), tc.want) {
				t.Errorf("got %v, want %v", uids(got), tc.want)
			}
		})
	}
}

// TestQueueOrdersBySoonestDeadline: a queue is read top to bottom by somebody
// deciding what to do next, so the deadline that has passed comes first and a
// document with no deadline comes last. SQLite sorts NULLs first, which would
// put "no due date" where "due at the beginning of time" belongs.
func TestQueueOrdersBySoonestDeadline(t *testing.T) {
	f := newDocFixture(t)

	undated, _ := f.create(t, "Undated")
	later, _ := f.create(t, "Later")
	sooner, _ := f.create(t, "Sooner")
	f.assign(t, later, f.author.ID, f.now.Add(48*time.Hour))
	f.assign(t, sooner, f.author.ID, f.now.Add(time.Hour))

	got, err := f.db.QueryDocuments(t.Context(), DocumentQuery{Now: f.now})
	if err != nil {
		t.Fatalf("QueryDocuments: %v", err)
	}
	want := []string{sooner.UID, later.UID, undated.UID}
	if !equalStrings(uids(got), want) {
		t.Errorf("order = %v, want %v", uids(got), want)
	}
}

// TestQueueLimit: the limit bounds the read, and the default applies when
// nobody asks.
func TestQueueLimit(t *testing.T) {
	f := newDocFixture(t)
	for i := range 3 {
		f.create(t, fmt.Sprintf("Document %d", i))
	}

	got, err := f.db.QueryDocuments(t.Context(), DocumentQuery{
		Filter: domain.DocumentFilter{Limit: 2}, Now: f.now,
	})
	if err != nil {
		t.Fatalf("QueryDocuments: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("got %d documents, want the 2 that were asked for", len(got))
	}

	if _, err := f.db.QueryDocuments(t.Context(), DocumentQuery{
		Filter: domain.DocumentFilter{Limit: -1}, Now: f.now,
	}); !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("a negative limit returned %v, want ErrInvalid", err)
	}
}

// TestOverdueIsMeasuredAgainstTheInstantItIsGiven is PLAN.md M5 acceptance 5
// at the level the SQL decides it: the query binds a parameter, never
// datetime('now'), so moving the caller's clock moves the answer.
func TestOverdueIsMeasuredAgainstTheInstantItIsGiven(t *testing.T) {
	f := newDocFixture(t)
	doc, _ := f.create(t, "Due tomorrow")
	due := f.now.Add(24 * time.Hour)
	f.assign(t, doc, f.author.ID, due)

	overdue := func(at time.Time) int {
		t.Helper()
		got, err := f.db.QueryDocuments(t.Context(), DocumentQuery{
			Filter: domain.DocumentFilter{Overdue: true}, Now: at,
		})
		if err != nil {
			t.Fatalf("QueryDocuments: %v", err)
		}
		return len(got)
	}

	if n := overdue(due.Add(-time.Second)); n != 0 {
		t.Errorf("%d overdue a second before the deadline, want 0", n)
	}
	if n := overdue(due); n != 1 {
		t.Errorf("%d overdue at the deadline, want 1", n)
	}
	if n := overdue(due.Add(365 * 24 * time.Hour)); n != 1 {
		t.Errorf("%d overdue a year later, want 1", n)
	}

	// The SQL must not read a clock of its own. datetime('now') here would
	// make every assertion above a coincidence of when the test ran.
	query, _ := documentQuerySQL(DocumentQuery{Filter: domain.DocumentFilter{Overdue: true}})
	if strings.Contains(query, "'now'") || strings.Contains(strings.ToLower(query), "current_timestamp") {
		t.Errorf("the overdue query reads SQLite's clock (invariant 3):\n%s", query)
	}
}

// TestQueueQueriesUseAnIndex is PLAN.md M5 acceptance 4.
//
// It asks SQLite rather than taking the migration's word for it: every filter
// combination the API can express is planned, and every line of the plan that
// touches "documents" must be a SEARCH using an index. A SCAN of that table is
// the failure, whether or not an index is named -- a covering-index scan still
// reads every row, and a queue that got slower as the archive grew is exactly
// what this is meant to catch.
//
// The unfiltered list is deliberately not in the table. It has nothing to seek
// on, and it is bounded by the LIMIT rather than by a WHERE clause; it is a
// list, not a queue, which is what DocumentFilter.IsQueue distinguishes.
func TestQueueQueriesUseAnIndex(t *testing.T) {
	f := newDocFixture(t)

	// A handful of rows so that the planner has statistics to prefer nothing
	// in particular. ANALYZE is deliberately not run: what is asserted is that
	// an index is available for the shape of the query, which is a property of
	// the schema rather than of the data.
	doc, _ := f.create(t, "One")
	f.assign(t, doc, f.author.ID, f.now.Add(time.Hour))
	f.create(t, "Two")

	filters := map[string]domain.DocumentFilter{
		"by state":                {State: "review"},
		"by site":                 {SiteID: f.siteID},
		"by assignee":             {AssignedTo: f.author.ID},
		"unassigned":              {Unassigned: true},
		"overdue":                 {Overdue: true},
		"state and unassigned":    {State: "review", Unassigned: true},
		"state and assignee":      {State: "review", AssignedTo: f.author.ID},
		"state and overdue":       {State: "review", Overdue: true},
		"site and state":          {SiteID: f.siteID, State: "review"},
		"assignee and overdue":    {AssignedTo: f.author.ID, Overdue: true},
		"unassigned and overdue":  {Unassigned: true, Overdue: true},
		"everything at once":      {SiteID: f.siteID, State: "review", Unassigned: true, Overdue: true},
		"site, assignee, overdue": {SiteID: f.siteID, AssignedTo: f.author.ID, Overdue: true},
	}

	for name, filter := range filters {
		t.Run(name, func(t *testing.T) {
			if !filter.IsQueue() {
				t.Fatalf("%+v does not constrain anything; this case proves nothing", filter)
			}
			plan := explain(t, f.db, DocumentQuery{Filter: filter, Now: f.now})
			for _, line := range plan {
				if scansDocuments(line) {
					t.Errorf("the plan reads every document row:\n%s", strings.Join(plan, "\n"))
					break
				}
			}
		})
	}
}

// scansDocuments reports whether one line of a query plan reads the documents
// table in full.
//
// EXPLAIN QUERY PLAN names the table by its alias, which is "d" in every query
// documentQuerySQL builds. "SCAN d" is the failure; "SEARCH d USING INDEX ..."
// is the pass. A SCAN that names an index is still a scan: it reads the whole
// index rather than the whole table, which is cheaper and still linear in the
// archive.
func scansDocuments(line string) bool {
	line = strings.TrimSpace(strings.TrimLeft(line, "|-`"))
	return strings.HasPrefix(line, "SCAN d ") || line == "SCAN d"
}

// explain runs EXPLAIN QUERY PLAN over exactly the statement QueryDocuments
// runs. Asserting on a hand-written approximation of the query would prove
// nothing about the query.
func explain(t *testing.T, db *DB, q DocumentQuery) []string {
	t.Helper()
	query, bind := documentQuerySQL(q)

	var plan []string
	err := db.Read(t.Context(), func(conn *sqlite.Conn) error {
		return run(conn, "explaining the queue query", "EXPLAIN QUERY PLAN "+query, bind,
			func(stmt *sqlite.Stmt) error {
				plan = append(plan, stmt.GetText("detail"))
				return nil
			})
	})
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	if len(plan) == 0 {
		t.Fatal("the query plan is empty; nothing was asserted")
	}
	return plan
}

// TestAssignmentWritesTheEventInOneTransaction is invariant 7 for the two
// operations M5 adds: the change and the event commit together.
func TestUpdateAssignment(t *testing.T) {
	f := newDocFixture(t)
	doc, _ := f.create(t, "Unassigned")

	if doc.AssignedTo != 0 || !doc.DueAt.IsZero() {
		t.Fatalf("a new document is assigned or due: %+v", doc)
	}

	due := f.now.Add(48 * time.Hour)
	got := f.assign(t, doc, f.other.ID, due)
	if got.AssignedTo != f.other.ID {
		t.Errorf("assigned_to = %d, want %d", got.AssignedTo, f.other.ID)
	}
	if !got.DueAt.Equal(due) {
		t.Errorf("due_at = %v, want %v", got.DueAt, due)
	}

	// The event landed in the same transaction.
	got2, err := f.db.EventsForSubject(t.Context(), domain.SubjectDocument, doc.ID, 10)
	if err != nil {
		t.Fatalf("EventsForSubject: %v", err)
	}
	if len(got2) == 0 || got2[0].Type != events.DocumentAssigned {
		t.Errorf("the newest event is %v, want %s", got2, events.DocumentAssigned)
	}

	// The zero value of what is pointed at writes NULL: "nobody" and "no
	// deadline" are outcomes somebody chose, not fields somebody forgot.
	cleared, err := f.db.UpdateAssignment(t.Context(), AssignmentUpdate{
		DocumentID:  doc.ID,
		SetAssignee: domain.Ref(int64(0)),
		SetDueAt:    domain.Ref(time.Time{}),
		Now:         f.now,
		Event:       f.event(f.author.ID, events.DocumentUnassigned),
	})
	if err != nil {
		t.Fatalf("UpdateAssignment: %v", err)
	}
	if cleared.AssignedTo != 0 {
		t.Errorf("assigned_to = %d after clearing, want nobody", cleared.AssignedTo)
	}
	if !cleared.DueAt.IsZero() {
		t.Errorf("due_at = %v after clearing, want no deadline", cleared.DueAt)
	}
}

// TestUpdateAssignmentLeavesTheOtherColumnAlone: nil means "leave it", which
// is what lets SetDue move a deadline without taking the work off somebody.
func TestUpdateAssignmentLeavesTheOtherColumnAlone(t *testing.T) {
	f := newDocFixture(t)
	doc, _ := f.create(t, "Assigned")
	due := f.now.Add(24 * time.Hour)
	f.assign(t, doc, f.other.ID, due)

	moved, err := f.db.UpdateAssignment(t.Context(), AssignmentUpdate{
		DocumentID: doc.ID,
		SetDueAt:   domain.Ref(f.now.Add(72 * time.Hour)),
		Now:        f.now,
		Event:      f.event(f.author.ID, events.DocumentDueChanged),
	})
	if err != nil {
		t.Fatalf("UpdateAssignment: %v", err)
	}
	if moved.AssignedTo != f.other.ID {
		t.Errorf("assigned_to = %d after moving only the due date, want %d", moved.AssignedTo, f.other.ID)
	}

	kept, err := f.db.UpdateAssignment(t.Context(), AssignmentUpdate{
		DocumentID:  doc.ID,
		SetAssignee: domain.Ref(f.author.ID),
		Now:         f.now,
		Event:       f.event(f.author.ID, events.DocumentAssigned),
	})
	if err != nil {
		t.Fatalf("UpdateAssignment: %v", err)
	}
	if !kept.DueAt.Equal(moved.DueAt) {
		t.Errorf("due_at = %v after reassigning, want the %v it already had", kept.DueAt, moved.DueAt)
	}
}

// TestUpdateAssignmentRefusesNothing: a request that changes neither column is
// a request nobody meant to make, and writing an event for it would put a line
// in the history saying something happened when nothing did.
func TestUpdateAssignmentRefusesNothing(t *testing.T) {
	f := newDocFixture(t)
	doc, _ := f.create(t, "Untouched")

	_, err := f.db.UpdateAssignment(t.Context(), AssignmentUpdate{
		DocumentID: doc.ID,
		Now:        f.now,
		Event:      f.event(f.author.ID, events.DocumentAssigned),
	})
	if !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("UpdateAssignment with nothing to change returned %v, want ErrInvalid", err)
	}
}

// TestUpdateAssignmentRefusesAUserThatIsNotThere: assigned_to is a real
// foreign key, and it is detected by result code rather than by matching a
// message (invariant 11).
func TestUpdateAssignmentRefusesAUserThatIsNotThere(t *testing.T) {
	f := newDocFixture(t)
	doc, _ := f.create(t, "Assigned to nobody real")

	_, err := f.db.UpdateAssignment(t.Context(), AssignmentUpdate{
		DocumentID:  doc.ID,
		SetAssignee: domain.Ref(int64(999999)),
		Now:         f.now,
		Event:       f.event(f.author.ID, events.DocumentAssigned),
	})
	ce, ok := AsConstraint(err)
	if !ok {
		t.Fatalf("UpdateAssignment returned %v, want a constraint violation", err)
	}
	if !ce.IsForeignKey() {
		t.Errorf("the violated constraint was %v, want a foreign key (invariant 22)", ce.Code)
	}
}

// TestUpdateAssignmentRefusesADocumentThatIsNotThere: the UPDATE matches no
// row, and that is a 404 rather than a silent success.
func TestUpdateAssignmentRefusesADocumentThatIsNotThere(t *testing.T) {
	f := newDocFixture(t)
	_, err := f.db.UpdateAssignment(t.Context(), AssignmentUpdate{
		DocumentID:  999999,
		SetAssignee: domain.Ref(f.author.ID),
		Now:         f.now,
		Event:       f.event(f.author.ID, events.DocumentAssigned),
	})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("UpdateAssignment on a missing document returned %v, want ErrNotFound", err)
	}
}

// moveTo walks a document to a state through ApplyTransition, which is the
// only thing that may write documents.state (invariant 4). The check it is
// given accepts, because what is under test here is the queue query and not
// the engine.
func moveTo(t *testing.T, f *docFixture, doc domain.Document, state string) {
	t.Helper()
	if _, err := f.db.ApplyTransition(t.Context(), TransitionRequest{
		DocumentID: doc.ID,
		Now:        f.now,
		Decide: func(TransitionFacts) (TransitionOutcome, error) {
			return TransitionOutcome{
				State: state,
				Event: f.event(f.author.ID, events.DocumentTransitioned),
			}, nil
		},
	}); err != nil {
		t.Fatalf("ApplyTransition to %q: %v", state, err)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
