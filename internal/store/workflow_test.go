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
	"github.com/mdhender/bricolage/internal/ids"
	"github.com/mdhender/bricolage/internal/migrate"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// The store half of M4, against a real in-memory database with every migration
// applied through the same runner cmsdb uses and with foreign keys on
// (DESIGN.md 15, invariant 22). A test that passed with them off would prove
// nothing about the composite foreign key this milestone exists to attach.

// TestDefaultWorkflowIsSeeded is the migration's own acceptance: a freshly
// initialised database carries the process of DESIGN.md 5.4, complete, with
// every guard and every effect drawn from the closed vocabulary.
func TestDefaultWorkflowIsSeeded(t *testing.T) {
	db := memoryDB(t)

	workflows, err := db.ListWorkflows(t.Context())
	if err != nil {
		t.Fatalf("ListWorkflows: %v", err)
	}
	if len(workflows) != 1 {
		t.Fatalf("a fresh database has %d workflows, want the one default story workflow", len(workflows))
	}
	w := workflows[0]

	if w.Kind != domain.KindStory || w.InitialState != "draft" {
		t.Errorf("the default workflow is %+v, want a story workflow starting in draft", w)
	}
	if w.SiteID != 0 {
		t.Errorf("the default workflow is bound to site %d; it applies to every site", w.SiteID)
	}

	wantStates := []string{"draft", "review", "approved", "published", "archived"}
	if len(w.States) != len(wantStates) {
		t.Fatalf("the workflow has %d states, want %d: %+v", len(w.States), len(wantStates), w.States)
	}
	for i, slug := range wantStates {
		if w.States[i].Slug != slug {
			t.Errorf("state %d is %q, want %q; states come back in position order", i, w.States[i].Slug, slug)
		}
		if w.States[i].Position != i+1 {
			t.Errorf("state %q has position %d, want %d", slug, w.States[i].Position, i+1)
		}
	}

	// "published" is a state, not an exit (DESIGN.md 5.4). The system we
	// learned from removed a published document from workflow entirely, so a
	// live document was nowhere.
	published, ok := w.State("published")
	if !ok {
		t.Fatal("there is no published state")
	}
	if published.Terminal {
		t.Error("published is terminal; it is a state, not an exit, and a document sits there while it serves")
	}
	if !published.Publishable {
		t.Error("published is not publishable")
	}
	if archived, _ := w.State("archived"); !archived.Terminal {
		t.Error("archived is not terminal")
	}

	// The whole vocabulary is exercised by the default process, so nothing in
	// it is a name with no worked example.
	seenGuards := map[domain.Guard]bool{}
	seenEffects := map[domain.Effect]bool{}
	for _, tr := range w.Transitions {
		for _, g := range tr.Guards {
			seenGuards[g] = true
		}
		for e := range tr.Effects {
			seenEffects[e] = true
		}
	}
	for _, g := range domain.Guards {
		if !seenGuards[g] {
			t.Errorf("no transition in the default workflow declares %q; a guard with no worked example is a guard nobody has read", g)
		}
	}
	for _, e := range domain.Effects {
		if !seenEffects[e] {
			t.Errorf("no transition in the default workflow declares %q", e)
		}
	}

	// The diagram in DESIGN.md 5.4, edge by edge.
	for _, want := range [][2]string{
		{"draft", "review"}, {"review", "approved"}, {"approved", "published"},
		{"review", "draft"}, {"approved", "draft"}, {"published", "draft"},
		{"draft", "archived"}, {"review", "archived"}, {"published", "archived"},
		{"archived", "draft"},
	} {
		if _, err := w.Transition(want[0], want[1]); err != nil {
			t.Errorf("the default workflow has no %s -> %s: %v", want[0], want[1], err)
		}
	}
	if len(w.Transitions) != 10 {
		t.Errorf("the default workflow has %d transitions, want the ten of DESIGN.md 5.4", len(w.Transitions))
	}
}

// TestDocumentsRebuildKeptItsRows is the twelve-step procedure's acceptance:
// the rebuild attaches the composite foreign key without losing a document or
// a version.
//
// The risk it guards is specific. With foreign keys on, "DROP TABLE documents"
// performs an implicit DELETE FROM that cascades into document_versions, so a
// rebuild done without the disable-foreign-keys directive would come back
// clean and empty.
func TestDocumentsRebuildKeptItsRows(t *testing.T) {
	dir := t.TempDir()

	// A database at migration 4: documents and versions, and no workflow yet.
	db, err := Create(t.Context(), dir)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	f := &docFixture{db: db, now: fixedClock().Now()}
	if f.siteID, err = db.CreateSite(t.Context(), NewSite{
		UID: ids.MustNew(f.now), Name: "Default", Domain: "example.com",
	}); err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	et, err := db.CreateElementType(t.Context(), NewElementType{
		UID: ids.MustNew(f.now), KeyName: "story", Name: "Story",
		Kind: domain.KindStory, TopLevel: true, Schema: `{"fields":[]}`, CreatedAt: f.now,
	})
	if err != nil {
		t.Fatalf("CreateElementType: %v", err)
	}
	f.etID = et.ID
	f.author = makeUser(t, db, "author@example.com")
	if f.workflow, err = db.WorkflowFor(t.Context(), domain.KindStory, f.siteID); err != nil {
		t.Fatalf("WorkflowFor: %v", err)
	}

	doc, draft := f.create(t, "Survives The Rebuild")
	if _, _, err := db.Checkout(t.Context(), doc.ID, f.author.ID, f.now, f.now.Add(time.Hour),
		f.event(f.author.ID, events.DocumentCheckedOut)); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if _, _, err := db.Checkin(t.Context(), doc.ID, f.author.ID, f.now, "one",
		f.event(f.author.ID, events.DocumentCheckedIn)); err != nil {
		t.Fatalf("Checkin: %v", err)
	}

	// The rebuild has already run -- Create applies every migration -- so what
	// this asserts is that a document written afterwards has both new columns
	// and that its versions are still reachable. The migration's own transfer
	// is covered by the assertion below, which runs the whole schema against a
	// database that had rows at 0004.
	got, err := db.DocumentByUID(t.Context(), doc.UID)
	if err != nil {
		t.Fatalf("DocumentByUID: %v", err)
	}
	if got.WorkflowID == 0 || got.State != "draft" {
		t.Errorf("document = workflow %d state %q, want the default workflow in draft", got.WorkflowID, got.State)
	}
	versions, err := db.VersionsForDocument(t.Context(), doc.ID)
	if err != nil {
		t.Fatalf("VersionsForDocument: %v", err)
	}
	if len(versions) != 1 || versions[0].ID != draft.ID {
		t.Fatalf("the document has %d versions, want the one it was created with", len(versions))
	}

	// The rebuild left referential integrity intact: nothing points at a row
	// that is not there.
	report, err := db.Check(t.Context(), time.Now().UTC())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !report.OK() {
		t.Errorf("the rebuilt database does not check out: %+v", report)
	}
}

// TestRebuildTransfersRowsWrittenBeforeIt applies the schema in two halves --
// stop at 0004, write a document, then migrate the rest -- which is the
// situation the twelve-step procedure exists for and the only one that can
// lose data.
func TestRebuildTransfersRowsWrittenBeforeIt(t *testing.T) {
	dir := t.TempDir()

	// Migrate to 0004 only. AllowBehind is what "cmsdb migrate up --to N"
	// leaves behind and what cmsd refuses to serve (invariant 21).
	conn, err := sqlite.OpenConn(Path(dir), createFlags)
	if err != nil {
		t.Fatalf("OpenConn: %v", err)
	}
	if err := prepareConn(conn, DefaultBusyTimeout); err != nil {
		t.Fatal(err)
	}
	if err := applyTo(t, conn, 4); err != nil {
		t.Fatalf("migrating to 0004: %v", err)
	}

	// A site, an element type, a user, a document, and two versions, written
	// through plain SQL because the store's writers now require columns that
	// do not exist yet at this version. This is the only place in this package
	// that writes rows by hand, and it is here because the point is what the
	// schema looked like before.
	seed := []string{
		`INSERT INTO sites (id, uid, name, domain) VALUES (1, 'site', 'Default', 'example.com')`,
		`INSERT INTO element_types (id, uid, key_name, name, kind, schema, created_at)
		 VALUES (1, 'et', 'story', 'Story', 'story', '{}', '2026-02-03T04:05:06.000Z')`,
		`INSERT INTO users (id, uid, email, name, active, created_at)
		 VALUES (1, 'user', 'a@example.com', 'A', 1, '2026-02-03T04:05:06.000Z')`,
		`INSERT INTO documents (id, uid, site_id, kind, element_type_id, assigned_to, created_at, updated_at)
		 VALUES (1, 'doc', 1, 'story', 1, 1, '2026-02-03T04:05:06.000Z', '2026-02-03T04:05:06.000Z')`,
		`INSERT INTO document_versions (id, document_id, version, title, slug, content, created_by, created_at, checked_in_at)
		 VALUES (1, 1, 1, 'One', 'one', '{}', 1, '2026-02-03T04:05:06.000Z', '2026-02-03T04:05:06.000Z')`,
		`INSERT INTO document_versions (id, document_id, version, title, slug, content, created_by, created_at)
		 VALUES (2, 1, 2, 'Two', 'two', '{}', 1, '2026-02-03T04:05:06.000Z')`,
		`UPDATE documents SET current_version_id = 2 WHERE id = 1`,
	}
	for _, stmt := range seed {
		if err := sqlitex.ExecuteTransient(conn, stmt, nil); err != nil {
			t.Fatalf("seeding at 0004: %v\n%s", err, stmt)
		}
	}

	// Now the rebuild.
	if err := applyTo(t, conn, -1); err != nil {
		t.Fatalf("migrating past the rebuild: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(t.Context(), dir, Options{})
	if err != nil {
		t.Fatalf("Open after the rebuild: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	doc, err := db.DocumentByUID(t.Context(), "doc")
	if err != nil {
		t.Fatalf("the document did not survive the rebuild: %v", err)
	}
	if doc.State != "draft" || doc.WorkflowID == 0 {
		t.Errorf("the transferred document is in workflow %d state %q, want the default workflow in draft",
			doc.WorkflowID, doc.State)
	}
	if doc.AssignedTo != 1 || doc.CurrentVersionID != 2 {
		t.Errorf("the rebuild lost a column: assigned_to = %d, current_version_id = %d",
			doc.AssignedTo, doc.CurrentVersionID)
	}

	versions, err := db.VersionsForDocument(t.Context(), doc.ID)
	if err != nil {
		t.Fatalf("VersionsForDocument: %v", err)
	}
	if len(versions) != 2 {
		t.Fatalf("the document has %d versions after the rebuild, want 2; DROP TABLE with foreign keys on would have cascaded them away", len(versions))
	}

	report, err := db.Check(t.Context(), time.Now().UTC())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !report.OK() {
		t.Errorf("the rebuilt database does not check out: %+v", report)
	}
}

// TestCompositeForeignKeyRefusesAnUndeclaredState is why the rebuild happened
// at all: the pair (workflow_id, state) is checked by the database, so a state
// no workflow declares cannot be stored whatever the Go code believes.
func TestCompositeForeignKeyRefusesAnUndeclaredState(t *testing.T) {
	f := newDocFixture(t)

	_, _, err := f.db.CreateDocument(t.Context(), NewDocument{
		UID:           ids.MustNew(f.now),
		SiteID:        f.siteID,
		Kind:          domain.KindStory,
		ElementTypeID: f.etID,
		WorkflowID:    f.workflow.ID,
		State:         "somewhere-else",
		Title:         "Nowhere",
		CreatedBy:     f.author.ID,
		CreatedAt:     f.now,
		Event:         f.event(f.author.ID, events.DocumentCreated),
	})
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a document was created in a state its workflow does not declare: %v", err)
	}
	ce, ok := AsConstraint(err)
	if !ok || !ce.IsForeignKey() {
		t.Errorf("the refusal is %v, want a foreign key violation classified by result code (invariant 11)", err)
	}
}

// TestApplyTransitionWritesStateAndEvent is the store's part of
// DESIGN.md 6.4: one transaction holding the check, the write, and the event.
func TestApplyTransitionWritesStateAndEvent(t *testing.T) {
	f := newDocFixture(t)
	doc, _ := f.create(t, "Moves")
	due := f.now.Add(48 * time.Hour)

	got, err := f.db.ApplyTransition(t.Context(), TransitionRequest{
		DocumentID: doc.ID,
		Now:        f.now,
		Decide: func(facts TransitionFacts) (TransitionOutcome, error) {
			if facts.Document.State != "draft" {
				t.Errorf("Decide was given state %q, want the row as it stands", facts.Document.State)
			}
			if facts.Version.Title != "Moves" {
				t.Errorf("Decide was given version %q, want the current one", facts.Version.Title)
			}
			return TransitionOutcome{
				State:    "review",
				SetDueAt: domain.Ref(due),
				Event: domain.Event{
					Type: events.DocumentTransitioned, ActorID: f.author.ID,
					Payload: map[string]any{"from": "draft", "to": "review"}, OccurredAt: f.now,
				},
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("ApplyTransition: %v", err)
	}
	if got.State != "review" {
		t.Errorf("state = %q, want review", got.State)
	}
	if !got.DueAt.Equal(due) {
		t.Errorf("due_at = %v, want %v", got.DueAt, due)
	}

	history, err := f.db.EventsForSubject(t.Context(), domain.SubjectDocument, doc.ID, 10)
	if err != nil {
		t.Fatalf("EventsForSubject: %v", err)
	}
	if len(history) == 0 || history[0].Type != events.DocumentTransitioned {
		t.Fatalf("the newest event is %+v, want the transition", history)
	}
	if history[0].SubjectID != doc.ID {
		t.Errorf("the event's subject is %d, want the document %d", history[0].SubjectID, doc.ID)
	}
}

// TestApplyTransitionRollsBackOnRefusal is PLAN.md M4 acceptance 6 at the
// store level: the state, the events, and everything else are unchanged.
func TestApplyTransitionRollsBackOnRefusal(t *testing.T) {
	f := newDocFixture(t)
	doc, _ := f.create(t, "Refused")

	before, err := f.db.EventsForSubject(t.Context(), domain.SubjectDocument, doc.ID, 100)
	if err != nil {
		t.Fatal(err)
	}

	refusal := domain.GuardHasSlug.Refused("Submit", "this document has no slug")
	_, err = f.db.ApplyTransition(t.Context(), TransitionRequest{
		DocumentID: doc.ID, Now: f.now,
		Decide: func(TransitionFacts) (TransitionOutcome, error) {
			return TransitionOutcome{}, refusal
		},
	})
	if !errors.Is(err, refusal) {
		t.Fatalf("ApplyTransition = %v, want the refusal returned unchanged", err)
	}

	after, err := f.db.DocumentByUID(t.Context(), doc.UID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "draft" {
		t.Errorf("state = %q after a refusal, want draft", after.State)
	}
	events, err := f.db.EventsForSubject(t.Context(), domain.SubjectDocument, doc.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != len(before) {
		t.Errorf("a refused transition wrote %d events", len(events)-len(before))
	}
}

// TestApplyTransitionClearsApprovals covers EffectClearApprovals, and with it
// the counter GuardApprovalsMet reads.
func TestApplyTransitionClearsApprovals(t *testing.T) {
	f := newDocFixture(t)
	doc, draft := f.create(t, "Approved Then Not")

	if _, created, err := f.db.CreateApproval(t.Context(), NewApproval{
		DocumentID: doc.ID, VersionID: draft.ID, State: "review",
		UserID: f.author.ID, CreatedAt: f.now,
	}, f.event(f.author.ID, events.DocumentApproved)); err != nil || !created {
		t.Fatalf("CreateApproval: created = %t, %v", created, err)
	}
	if n, err := f.db.CountApprovals(t.Context(), draft.ID, "review"); err != nil || n != 1 {
		t.Fatalf("CountApprovals = %d, %v, want 1", n, err)
	}

	// A second approval by the same person in the same state is one approval,
	// which is what makes "two distinct people must approve" a plain COUNT --
	// and, since M11, it is not an error either (PLAN.md M11 acceptance 1).
	_, created, err := f.db.CreateApproval(t.Context(), NewApproval{
		DocumentID: doc.ID, VersionID: draft.ID, State: "review",
		UserID: f.author.ID, CreatedAt: f.now,
	}, f.event(f.author.ID, events.DocumentApproved))
	if err != nil || created {
		t.Errorf("a duplicate approval reported created = %t, %v; want false, nil", created, err)
	}
	if n, err := f.db.CountApprovals(t.Context(), draft.ID, "review"); err != nil || n != 1 {
		t.Errorf("CountApprovals = %d, %v after approving twice, want 1", n, err)
	}

	if _, err := f.db.ApplyTransition(t.Context(), TransitionRequest{
		DocumentID: doc.ID, Now: f.now,
		Decide: func(TransitionFacts) (TransitionOutcome, error) {
			return TransitionOutcome{
				State: "review", ClearApprovals: true,
				Event: domain.Event{Type: events.DocumentTransitioned, OccurredAt: f.now},
			}, nil
		},
	}); err != nil {
		t.Fatalf("ApplyTransition: %v", err)
	}

	if n, err := f.db.CountApprovals(t.Context(), draft.ID, "review"); err != nil || n != 0 {
		t.Errorf("CountApprovals = %d, %v after clear_approvals, want 0", n, err)
	}
}

// TestApprovalsAreScopedToOneVersion is PLAN.md M4 acceptance 7 at the store
// level. Approvals attach to a version, not to a document, so a new version
// starts with none.
func TestApprovalsAreScopedToOneVersion(t *testing.T) {
	f := newDocFixture(t)
	doc, v1 := f.create(t, "Signed Off")

	if _, _, err := f.db.CreateApproval(t.Context(), NewApproval{
		DocumentID: doc.ID, VersionID: v1.ID, State: "review",
		UserID: f.author.ID, CreatedAt: f.now,
	}, f.event(f.author.ID, events.DocumentApproved)); err != nil {
		t.Fatalf("CreateApproval: %v", err)
	}
	if _, _, err := f.db.Checkout(t.Context(), doc.ID, f.author.ID, f.now, f.now.Add(time.Hour),
		f.event(f.author.ID, events.DocumentCheckedOut)); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if _, _, err := f.db.Checkin(t.Context(), doc.ID, f.author.ID, f.now, "one",
		f.event(f.author.ID, events.DocumentCheckedIn)); err != nil {
		t.Fatalf("Checkin: %v", err)
	}

	// A new version: the approval of the old one is still on the old one, and
	// the new one carries none.
	if _, _, err := f.db.Checkout(t.Context(), doc.ID, f.author.ID, f.now, f.now.Add(time.Hour),
		f.event(f.author.ID, events.DocumentCheckedOut)); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	after, err := f.db.DocumentByUID(t.Context(), doc.UID)
	if err != nil {
		t.Fatal(err)
	}
	if after.CurrentVersionID == v1.ID {
		t.Fatal("checking out did not open a new version, so this test proves nothing")
	}

	if n, err := f.db.CountApprovals(t.Context(), v1.ID, "review"); err != nil || n != 1 {
		t.Errorf("the old version's approval count is %d, %v, want 1: history is not rewritten", n, err)
	}
	if n, err := f.db.CountApprovals(t.Context(), after.CurrentVersionID, "review"); err != nil || n != 0 {
		t.Errorf("the new version carries %d approvals, %v, want 0", n, err)
	}
}

// TestComments covers the data GuardCommentsResolved reads.
func TestComments(t *testing.T) {
	f := newDocFixture(t)
	doc, ver := f.create(t, "Discussed")

	if n, err := f.db.CountUnresolvedComments(t.Context(), doc.ID); err != nil || n != 0 {
		t.Fatalf("a new document has %d open comments, %v, want 0", n, err)
	}

	c, err := f.db.CreateComment(t.Context(), NewComment{
		UID: ids.MustNew(f.now), DocumentID: doc.ID, VersionID: ver.ID,
		AuthorID: f.author.ID, Body: "the lede is buried", CreatedAt: f.now,
	}, f.event(f.author.ID, events.DocumentCommented))
	if err != nil {
		t.Fatalf("CreateComment: %v", err)
	}
	if n, err := f.db.CountUnresolvedComments(t.Context(), doc.ID); err != nil || n != 1 {
		t.Fatalf("CountUnresolvedComments = %d, %v, want 1", n, err)
	}

	if _, resolved, err := f.db.ResolveComment(t.Context(), c.ID, f.other.ID, f.now,
		f.event(f.other.ID, events.DocumentCommentResolved)); err != nil || !resolved {
		t.Fatalf("ResolveComment: resolved = %t, %v", resolved, err)
	}
	if n, err := f.db.CountUnresolvedComments(t.Context(), doc.ID); err != nil || n != 0 {
		t.Errorf("CountUnresolvedComments = %d, %v after resolving, want 0", n, err)
	}
}

// TestGrantScopeCarriesWorkflowAndState is the other half of 0005's promise to
// 0003: the scope column arrives with the table it points at, and the resolver
// that has carried the dimension since M2 now has something to match.
func TestGrantScopeCarriesWorkflowAndState(t *testing.T) {
	f := newDocFixture(t)

	role, err := f.db.CreateRole(t.Context(), "reviewer", "Reviewer")
	if err != nil {
		t.Fatal(err)
	}
	written, err := f.db.CreateGrant(t.Context(), domain.Grant{
		RoleID:    role.ID,
		Privilege: domain.Edit,
		Scope: domain.Scope{
			WorkflowID:   domain.Ref(f.workflow.ID),
			State:        domain.Ref("review"),
			CategoryDeep: true,
		},
		CreatedAt: f.now,
	})
	if err != nil {
		t.Fatalf("CreateGrant: %v", err)
	}

	if err := f.db.AssignRole(t.Context(), f.other.ID, role.ID); err != nil {
		t.Fatal(err)
	}
	grants, err := f.db.GrantsForUser(t.Context(), f.other.ID)
	if err != nil {
		t.Fatal(err)
	}
	var got *domain.Grant
	for i := range grants {
		if grants[i].ID == written.ID {
			got = &grants[i]
		}
	}
	if got == nil {
		t.Fatal("the grant did not come back for the user holding its role")
	}
	if got.Scope.WorkflowID == nil || *got.Scope.WorkflowID != f.workflow.ID {
		t.Errorf("the workflow constraint did not survive the round trip: %+v", got.Scope)
	}
	if got.Scope.State == nil || *got.Scope.State != "review" {
		t.Errorf("the state constraint did not survive the round trip: %+v", got.Scope)
	}

	// A grant naming a workflow that does not exist is refused by the foreign
	// key the column arrived with, which is the whole reason it waited for
	// this migration.
	_, err = f.db.CreateGrant(t.Context(), domain.Grant{
		RoleID: role.ID, Privilege: domain.Edit,
		Scope:     domain.Scope{WorkflowID: domain.Ref(int64(9999)), CategoryDeep: true},
		CreatedAt: f.now,
	})
	if !errors.Is(err, domain.ErrConflict) {
		t.Errorf("a grant naming a workflow that does not exist was accepted: %v", err)
	}
}

// TestWorkflowForRefusesAKindWithNoProcess covers the resolver a new document
// goes through. A kind with no workflow is a clean refusal naming the kind
// rather than a placement in somebody else's process.
func TestWorkflowForRefusesAKindWithNoProcess(t *testing.T) {
	f := newDocFixture(t)

	if _, err := f.db.WorkflowFor(t.Context(), domain.KindStory, f.siteID); err != nil {
		t.Errorf("WorkflowFor(story): %v", err)
	}
	_, err := f.db.WorkflowFor(t.Context(), domain.KindMedia, f.siteID)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("WorkflowFor(media) = %v, want ErrNotFound", err)
	}
	if err != nil && !strings.Contains(err.Error(), domain.KindMedia) {
		t.Errorf("the refusal does not name the kind: %v", err)
	}
}

// TestUnknownGuardRefusesTheWorkflow is invariant 6 where it bites: a database
// configured with a guard this binary does not enforce must refuse to hand the
// workflow over, not hand it over with the guard quietly dropped.
func TestUnknownGuardRefusesTheWorkflow(t *testing.T) {
	f := newDocFixture(t)

	err := f.db.Write(t.Context(), func(conn *sqlite.Conn) error {
		return sqlitex.ExecuteTransient(conn,
			`UPDATE workflow_transitions SET guards = '["pre_chk_rules"]' WHERE from_state = 'draft' AND to_state = 'review'`, nil)
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = f.db.WorkflowByID(t.Context(), f.workflow.ID)
	if err == nil {
		t.Fatal("a workflow configured with an unenforced guard loaded cleanly")
	}
	if !strings.Contains(err.Error(), "pre_chk_rules") {
		t.Errorf("the refusal does not name the guard: %v", err)
	}

	// It is not domain.ErrInvalid. The caller asked a good question; the
	// database is configured with a process this binary cannot run, and
	// telling the client their request was malformed would send them looking
	// in the wrong place (a 422 rather than a 500).
	for _, sentinel := range []error{domain.ErrInvalid, domain.ErrNotFound, domain.ErrConflict, domain.ErrForbidden} {
		if errors.Is(err, sentinel) {
			t.Errorf("the refusal answers to %v; a misconfigured process is the server's fault", sentinel)
		}
	}
}

// memoryDB is an in-memory store for a test that needs nothing else.
func memoryDB(t *testing.T) *DB {
	t.Helper()
	db, err := OpenMemory(t.Context())
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// applyTo runs migrations up to n on a connection the test holds, so that a
// test can stand a database up at a version that is not the current one.
func applyTo(t *testing.T, conn *sqlite.Conn, n int) error {
	t.Helper()
	if err := migrate.Apply(t.Context(), conn, n); err != nil {
		return fmt.Errorf("applying migrations to %d: %w", n, err)
	}
	return nil
}

// TestOneWorkflowGovernsOneKindPerSite is 0006. WorkflowFor resolves a
// site-specific workflow over the general one, and these indexes are what make
// that a rule rather than a tie-break: without them two workflows for the same
// kind and site are accepted, the lower id silently wins, and every document
// created afterwards enters one of them with nothing to notice.
func TestOneWorkflowGovernsOneKindPerSite(t *testing.T) {
	f := newDocFixture(t)

	newWorkflow := func(uid, kind string, siteID *int64) error {
		return f.db.Write(t.Context(), func(conn *sqlite.Conn) error {
			return run(conn, "adding workflow "+uid, `
				INSERT INTO workflows (uid, site_id, kind, name, initial_state)
				VALUES (:uid, :site_id, :kind, :name, 'draft')`,
				func(stmt *sqlite.Stmt) {
					stmt.SetText(":uid", uid)
					bindNullInt64(stmt, ":site_id", siteID)
					stmt.SetText(":kind", kind)
					stmt.SetText(":name", uid)
				}, nil)
		})
	}

	// A second workflow governing stories on every site is refused: the one
	// the migration seeded already does.
	if err := newWorkflow(ids.MustNew(f.now), domain.KindStory, nil); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("a second default story workflow was accepted: %v", err)
	}

	// A different kind is fine.
	if err := newWorkflow(ids.MustNew(f.now), domain.KindMedia, nil); err != nil {
		t.Fatalf("a default media workflow was refused: %v", err)
	}

	// A story workflow for one site is fine, and wins over the general one.
	// It gets a state, because WorkflowFor validates what it hands back and a
	// workflow with no states is not one the engine can run.
	siteUID := ids.MustNew(f.now)
	if err := newWorkflow(siteUID, domain.KindStory, &f.siteID); err != nil {
		t.Fatalf("a site-specific story workflow was refused: %v", err)
	}
	err := f.db.Write(t.Context(), func(conn *sqlite.Conn) error {
		return sqlitex.ExecuteTransient(conn, `
			INSERT INTO workflow_states (workflow_id, slug, name, position)
			SELECT id, 'draft', 'Draft', 1 FROM workflows WHERE uid = '`+siteUID+`'`, nil)
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := f.db.WorkflowFor(t.Context(), domain.KindStory, f.siteID)
	if err != nil {
		t.Fatalf("WorkflowFor: %v", err)
	}
	if got.UID != siteUID {
		t.Errorf("WorkflowFor chose %q, want the site-specific %q", got.UID, siteUID)
	}

	// A second one for that same site is not.
	if err := newWorkflow(ids.MustNew(f.now), domain.KindStory, &f.siteID); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("a second story workflow for one site was accepted: %v", err)
	}
}

// TestWorkflowUIDsIgnoresAMisconfiguredProcess is why naming a workflow is a
// different query from loading one.
//
// ListWorkflows validates, so one half-configured process fails it -- which is
// right when the caller is about to run the process. Naming the workflow a
// document is in must not depend on some other workflow being well formed.
func TestWorkflowUIDsIgnoresAMisconfiguredProcess(t *testing.T) {
	f := newDocFixture(t)

	err := f.db.Write(t.Context(), func(conn *sqlite.Conn) error {
		return sqlitex.ExecuteTransient(conn,
			`INSERT INTO workflows (uid, site_id, kind, name, initial_state)
			 VALUES ('half-configured', NULL, 'media', 'Half Configured', 'draft')`, nil)
	})
	if err != nil {
		t.Fatal(err)
	}

	// The new workflow has no states, so it does not validate.
	if _, err := f.db.ListWorkflows(t.Context()); err == nil {
		t.Fatal("ListWorkflows accepted a workflow with no states")
	}

	uids, err := f.db.WorkflowUIDs(t.Context())
	if err != nil {
		t.Fatalf("WorkflowUIDs: %v", err)
	}
	if uids[f.workflow.ID] != f.workflow.UID {
		t.Errorf("the story workflow lost its name to an unrelated misconfigured row: %v", uids)
	}
}
