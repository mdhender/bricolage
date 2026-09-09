// Copyright (c) 2026 Michael D Henderson.

package workflow

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mdhender/bricolage/internal/clock"
	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/events"
	"github.com/mdhender/bricolage/internal/ids"
	"github.com/mdhender/bricolage/internal/store"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// The engine's tests run against a real in-memory store with the default story
// workflow the migration seeds, and a fake clock (DESIGN.md 15). PLAN.md M4's
// seven acceptance criteria are each named in a test below.

var start = time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)

// harness is an engine over a fresh database, plus the people and the document
// every test needs.
type harness struct {
	engine *Engine
	db     *store.DB
	clock  *clock.Fake

	siteID   int64
	etID     int64
	workflow domain.Workflow
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	db, err := store.OpenMemory(t.Context())
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	c := clock.NewFake(start)
	engine, err := New(db, c)
	if err != nil {
		t.Fatalf("workflow.New: %v", err)
	}
	h := &harness{engine: engine, db: db, clock: c}

	if h.siteID, err = db.CreateSite(t.Context(), store.NewSite{
		UID: ids.MustNew(start), Name: "Default", Domain: "example.com",
	}); err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	et, err := db.CreateElementType(t.Context(), store.NewElementType{
		UID: ids.MustNew(start), KeyName: "story", Name: "Story",
		Kind: domain.KindStory, TopLevel: true, Schema: `{"fields":[]}`, CreatedAt: start,
	})
	if err != nil {
		t.Fatalf("CreateElementType: %v", err)
	}
	h.etID = et.ID
	if h.workflow, err = db.WorkflowFor(t.Context(), domain.KindStory, h.siteID); err != nil {
		t.Fatalf("WorkflowFor: %v", err)
	}
	return h
}

// actor creates a user holding one global grant, which is the shape "cmsdb
// seed" plus a role assignment produces.
func (h *harness) actor(t *testing.T, email string, p domain.Privilege) domain.Identity {
	t.Helper()
	u, err := h.db.CreateUser(t.Context(), store.NewUser{
		UID: ids.MustNew(start), Email: email, Name: email, CreatedAt: start,
	})
	if err != nil {
		t.Fatalf("CreateUser(%q): %v", email, err)
	}
	role, err := h.db.CreateRole(t.Context(), email, "Role for "+email)
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	if err := h.db.AssignRole(t.Context(), u.ID, role.ID); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}
	if p != domain.NoPrivilege {
		if _, err := h.db.CreateGrant(t.Context(), domain.Grant{
			RoleID: role.ID, Privilege: p, CreatedAt: start,
			Scope: domain.Scope{CategoryDeep: true},
		}); err != nil {
			t.Fatalf("CreateGrant: %v", err)
		}
	}
	identity, err := h.db.Identity(t.Context(), u.ID)
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	return identity
}

// doc creates a document with a slug and a cover date, which is the shape that
// satisfies has_slug and has_cover_date.
func (h *harness) doc(t *testing.T, actor domain.Identity, title string) domain.Document {
	t.Helper()
	d, _, err := h.db.CreateDocument(t.Context(), store.NewDocument{
		UID:           ids.MustNew(start),
		SiteID:        h.siteID,
		Kind:          domain.KindStory,
		ElementTypeID: h.etID,
		WorkflowID:    h.workflow.ID,
		State:         h.workflow.InitialState,
		Title:         title,
		Slug:          "a-slug",
		CoverDate:     "2026-03-01",
		Content:       `{"body":"words"}`,
		CreatedBy:     actor.User.ID,
		CreatedAt:     start,
		Event: domain.Event{
			Type: events.DocumentCreated, ActorID: actor.User.ID, OccurredAt: start,
		},
	})
	if err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}
	return d
}

// reload reads the document back, because a transition returns a fresh copy
// and every step after one has to see it.
func (h *harness) reload(t *testing.T, uid string) domain.Document {
	t.Helper()
	d, err := h.db.DocumentByUID(t.Context(), uid)
	if err != nil {
		t.Fatalf("DocumentByUID: %v", err)
	}
	return d
}

// move performs a transition and fails the test if it is refused.
func (h *harness) move(t *testing.T, actor domain.Identity, doc domain.Document, to, note string) domain.Document {
	t.Helper()
	got, err := h.engine.Do(t.Context(), Request{Document: doc, To: to, Note: note, Actor: actor})
	if err != nil {
		t.Fatalf("Do(%s -> %s): %v", doc.State, to, err)
	}
	return got
}

// exec runs one statement, for the tests that need a workflow configured
// differently from the seeded default. Configuration is data (DESIGN.md 5.4),
// and reconfiguring it is what a guard test has to do.
func (h *harness) exec(t *testing.T, query string) {
	t.Helper()
	err := h.db.Write(t.Context(), func(conn *sqlite.Conn) error {
		return sqlitex.ExecuteTransient(conn, query, nil)
	})
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	if h.workflow, err = h.db.WorkflowFor(t.Context(), domain.KindStory, h.siteID); err != nil {
		t.Fatalf("reloading the workflow: %v", err)
	}
}

// allowed finds one entry of the menu by its target state.
func allowed(t *testing.T, menu []Allowed, to string) Allowed {
	t.Helper()
	for _, a := range menu {
		if a.Transition.To == to {
			return a
		}
	}
	t.Fatalf("no transition to %q in the menu: %+v", to, menu)
	return Allowed{}
}

// TestAvailableListsEveryTransitionWithReasons is PLAN.md M4 acceptance 1:
// every transition out of the current state, including the refused ones with a
// reason.
func TestAvailableListsEveryTransitionWithReasons(t *testing.T) {
	h := newHarness(t)
	writer := h.actor(t, "writer@example.com", domain.Edit)
	doc := h.doc(t, writer, "The Quick Brown Fox")

	menu, err := h.engine.Available(t.Context(), doc, writer)
	if err != nil {
		t.Fatalf("Available: %v", err)
	}

	// draft has two ways out: submit and archive.
	if len(menu) != 2 {
		t.Fatalf("draft offers %d transitions, want submit and archive: %+v", len(menu), menu)
	}

	submit := allowed(t, menu, "review")
	if !submit.Permitted {
		t.Errorf("a writer holding Edit may not submit: %s", submit.Reason)
	}

	// Archive needs Create and the writer holds Edit, so it is refused -- and
	// it is still in the list, with a reason. An action that silently does not
	// exist teaches nobody anything.
	archive := allowed(t, menu, "archived")
	if archive.Permitted {
		t.Error("a writer holding only Edit may archive")
	}
	if archive.Reason == "" {
		t.Error("the refusal carries no reason; the UI has nothing to show beside the greyed-out action")
	}
	if archive.Guard != "" {
		t.Errorf("the privilege refusal names guard %q; a privilege is not a guard", archive.Guard)
	}
	if !strings.Contains(archive.Reason, "create") {
		t.Errorf("the reason does not name the privilege needed: %q", archive.Reason)
	}
}

// TestUndeclaredTransitionIsAConflictEvenForAnAdministrator is PLAN.md M4
// acceptance 2.
//
// A move the state machine does not contain is not a permission question. An
// administrator holding Publish over everything gets the same answer as
// anybody else, because a bigger grant is not the thing standing in the way.
func TestUndeclaredTransitionIsAConflictEvenForAnAdministrator(t *testing.T) {
	h := newHarness(t)
	admin := h.actor(t, "admin@example.com", domain.Publish)
	doc := h.doc(t, admin, "Straight To Press")

	_, err := h.engine.Do(t.Context(), Request{Document: doc, To: "published", Actor: admin})
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("draft -> published for an administrator = %v, want ErrConflict", err)
	}
	if errors.Is(err, domain.ErrForbidden) {
		t.Error("the refusal answers to ErrForbidden, which tells an administrator to go and ask for a permission they already hold")
	}
	if _, isGuard := domain.GuardOf(err); isGuard {
		t.Error("the refusal names a guard; no guard was reached, because the move is not in the machine")
	}
	if got := h.reload(t, doc.UID); got.State != "draft" {
		t.Errorf("state = %q after a refused transition, want draft", got.State)
	}
}

// TestEachGuardPassesAndRefuses is PLAN.md M4 acceptance 3: for each of the
// seven guards, one case where it passes, one where it refuses, and a refusal
// that names the guard.
//
// Each case reconfigures the seeded workflow to put exactly the guard under
// test on one transition, so that nothing else can be what refused. Transitions
// are data; that is what makes this possible and it is why they are data.
func TestEachGuardPassesAndRefuses(t *testing.T) {
	// setup arranges the document and the actor so that the guard passes;
	// breaks arranges the same pair so that it refuses.
	for _, tc := range []struct {
		guard  domain.Guard
		note   string
		setup  func(t *testing.T, h *harness, doc domain.Document, actor domain.Identity)
		breaks func(t *testing.T, h *harness, doc domain.Document, actor domain.Identity)
	}{
		{
			guard: domain.GuardNoteRequired,
			note:  "sending this back",
			breaks: func(*testing.T, *harness, domain.Document, domain.Identity) {
				// The note is the request's, so "breaking" it is running the
				// same case with no note. handled below by noteFor.
			},
		},
		{
			guard: domain.GuardAssigneeOnly,
			setup: func(t *testing.T, h *harness, doc domain.Document, actor domain.Identity) {
				// Assigned to the actor: they may act. An unassigned document
				// also passes, which the case below covers by leaving it.
				h.exec(t, fmt.Sprintf("UPDATE documents SET assigned_to = %d WHERE id = %d", actor.User.ID, doc.ID))
			},
			breaks: func(t *testing.T, h *harness, doc domain.Document, actor domain.Identity) {
				other := h.actor(t, "somebody-else@example.com", domain.Read)
				h.exec(t, fmt.Sprintf("UPDATE documents SET assigned_to = %d WHERE id = %d", other.User.ID, doc.ID))
			},
		},
		{
			guard: domain.GuardApprovalsMet,
			setup: func(t *testing.T, h *harness, doc domain.Document, actor domain.Identity) {
				h.exec(t, `UPDATE workflow_states SET required_approvals = 1 WHERE slug = 'draft'`)
				if _, err := h.db.CreateApproval(t.Context(), store.NewApproval{
					DocumentID: doc.ID, VersionID: doc.CurrentVersionID, State: "draft",
					UserID: actor.User.ID, CreatedAt: start,
				}); err != nil {
					t.Fatalf("CreateApproval: %v", err)
				}
			},
			breaks: func(t *testing.T, h *harness, doc domain.Document, actor domain.Identity) {
				h.exec(t, `UPDATE workflow_states SET required_approvals = 2 WHERE slug = 'draft'`)
			},
		},
		{
			guard: domain.GuardNotLocked,
			breaks: func(t *testing.T, h *harness, doc domain.Document, actor domain.Identity) {
				if _, _, err := h.db.Checkout(t.Context(), doc.ID, actor.User.ID, start, start.Add(time.Hour),
					domain.Event{Type: events.DocumentCheckedOut, ActorID: actor.User.ID, OccurredAt: start}); err != nil {
					t.Fatalf("Checkout: %v", err)
				}
			},
		},
		{
			guard: domain.GuardCommentsResolved,
			breaks: func(t *testing.T, h *harness, doc domain.Document, actor domain.Identity) {
				if _, err := h.db.CreateComment(t.Context(), store.NewComment{
					UID: ids.MustNew(start), DocumentID: doc.ID, AuthorID: actor.User.ID,
					Body: "the lede is buried", CreatedAt: start,
				}); err != nil {
					t.Fatalf("CreateComment: %v", err)
				}
			},
		},
		{
			guard: domain.GuardHasSlug,
			breaks: func(t *testing.T, h *harness, doc domain.Document, actor domain.Identity) {
				h.exec(t, fmt.Sprintf("UPDATE document_versions SET slug = '' WHERE id = %d", doc.CurrentVersionID))
			},
		},
		{
			guard: domain.GuardHasCoverDate,
			breaks: func(t *testing.T, h *harness, doc domain.Document, actor domain.Identity) {
				h.exec(t, fmt.Sprintf("UPDATE document_versions SET cover_date = NULL WHERE id = %d", doc.CurrentVersionID))
			},
		},
	} {
		t.Run(string(tc.guard), func(t *testing.T) {
			// build stands up a document whose one route out of draft carries
			// exactly this guard.
			build := func(t *testing.T) (*harness, domain.Identity, domain.Document) {
				t.Helper()
				h := newHarness(t)
				h.exec(t, fmt.Sprintf(
					`UPDATE workflow_transitions SET guards = '["%s"]' WHERE from_state = 'draft' AND to_state = 'review'`,
					tc.guard))
				actor := h.actor(t, "editor@example.com", domain.Publish)
				return h, actor, h.doc(t, actor, "Guarded")
			}

			t.Run("passes", func(t *testing.T) {
				h, actor, doc := build(t)
				if tc.setup != nil {
					tc.setup(t, h, doc, actor)
				}
				doc = h.reload(t, doc.UID)

				menu, err := h.engine.Available(t.Context(), doc, actor)
				if err != nil {
					t.Fatalf("Available: %v", err)
				}
				a := allowed(t, menu, "review")
				switch tc.guard {
				case domain.GuardNoteRequired:
					// Available is asked before anybody has typed a note, so
					// this one comes back refused, marked as a prompt rather
					// than as a final answer. Do with the note succeeds, which
					// is the next assertion.
					if a.Permitted {
						t.Error("note_required was satisfied by a menu asked with no note")
					}
					if !a.NeedsNote {
						t.Error("Available does not report that the transition needs a note")
					}
				default:
					if !a.Permitted {
						t.Fatalf("%s refused a case it should pass: %s", tc.guard, a.Reason)
					}
				}
				if got := h.move(t, actor, doc, "review", tc.note); got.State != "review" {
					t.Errorf("state = %q, want review", got.State)
				}
			})

			t.Run("refuses, naming the guard", func(t *testing.T) {
				h, actor, doc := build(t)
				if tc.setup != nil {
					tc.setup(t, h, doc, actor)
				}
				if tc.breaks != nil {
					tc.breaks(t, h, doc, actor)
				}
				doc = h.reload(t, doc.UID)

				// note_required is broken by withholding the note, which is a
				// property of the request rather than of the document.
				note := tc.note
				if tc.guard == domain.GuardNoteRequired {
					note = ""
				}

				_, err := h.engine.Do(t.Context(), Request{Document: doc, To: "review", Note: note, Actor: actor})
				if err == nil {
					t.Fatalf("%s let a transition through that it should refuse", tc.guard)
				}
				if !errors.Is(err, domain.ErrGuardFailed) {
					t.Errorf("the refusal does not answer to ErrGuardFailed: %v", err)
				}
				got, ok := domain.GuardOf(err)
				if !ok || got != tc.guard {
					t.Errorf("the refusal names guard %q, want %q: %v", got, tc.guard, err)
				}
				if state := h.reload(t, doc.UID).State; state != "draft" {
					t.Errorf("state = %q after a refused transition, want draft", state)
				}
			})
		})
	}
}

// TestAssigneeOnlyPassesWhenNobodyIsAssigned is the case the guard's
// documentation turns on: an unassigned document is unclaimed, so anybody with
// the privilege may take the step.
//
// The alternative -- refusing until somebody is assigned -- makes the guard
// unusable on the first transition of any workflow, which is exactly where a
// process wants it.
func TestAssigneeOnlyPassesWhenNobodyIsAssigned(t *testing.T) {
	h := newHarness(t)
	h.exec(t, `UPDATE workflow_transitions SET guards = '["assignee_only"]' WHERE from_state = 'draft' AND to_state = 'review'`)
	actor := h.actor(t, "editor@example.com", domain.Publish)
	doc := h.doc(t, actor, "Unclaimed")

	if doc.AssignedTo != 0 {
		t.Fatal("a new document is assigned to somebody, so this test proves nothing")
	}
	if got := h.move(t, actor, doc, "review", ""); got.State != "review" {
		t.Errorf("state = %q, want review", got.State)
	}
}

// TestAvailableAndDoNeverDisagree is PLAN.md M4 acceptance 5, as a property
// over every state of the default workflow and every actor privilege.
//
// For each pair it asks Available, then performs each offered transition with
// the same note Available was asked with -- the empty one -- and requires that
// permitted means it succeeded and refused means Do returned the same guard.
func TestAvailableAndDoNeverDisagree(t *testing.T) {
	states := []string{"draft", "review", "approved", "published", "archived"}
	privileges := []domain.Privilege{domain.Read, domain.Edit, domain.Recall, domain.Create, domain.Publish}

	for _, from := range states {
		for _, p := range privileges {
			t.Run(fmt.Sprintf("%s/%s", from, p), func(t *testing.T) {
				// Each iteration gets its own database, because performing a
				// transition changes the state the next one would be asked
				// about.
				base := newHarness(t)
				actor := base.actor(t, "actor@example.com", p)
				doc := base.doc(t, actor, "Property")
				base.exec(t, fmt.Sprintf("UPDATE documents SET state = '%s' WHERE id = %d", from, doc.ID))
				doc = base.reload(t, doc.UID)

				menu, err := base.engine.Available(t.Context(), doc, actor)
				if err != nil {
					t.Fatalf("Available: %v", err)
				}

				for _, a := range menu {
					// A fresh database per transition, arranged identically,
					// so that the one Do performs is the one Available was
					// asked about.
					h := newHarness(t)
					actor := h.actor(t, "actor@example.com", p)
					d := h.doc(t, actor, "Property")
					h.exec(t, fmt.Sprintf("UPDATE documents SET state = '%s' WHERE id = %d", from, d.ID))
					d = h.reload(t, d.UID)

					_, err := h.engine.Do(t.Context(), Request{Document: d, To: a.Transition.To, Actor: actor})
					if a.Permitted {
						if err != nil {
							t.Errorf("Available permitted %s -> %s and Do refused it: %v",
								from, a.Transition.To, err)
						}
						continue
					}
					if err == nil {
						t.Errorf("Available refused %s -> %s (%s) and Do performed it",
							from, a.Transition.To, a.Reason)
						continue
					}
					got, _ := domain.GuardOf(err)
					if got != a.Guard {
						t.Errorf("Available refused %s -> %s with guard %q and Do refused it with %q: %v",
							from, a.Transition.To, a.Guard, got, err)
					}
				}
			})
		}
	}
}

// TestFailedGuardRollsBackCleanly is PLAN.md M4 acceptance 6: state, events,
// and jobs are all unchanged.
//
// Jobs are M6 and there is no queue to inspect yet; what stands in for it is
// the assertion that nothing at all was written, which is the same
// transaction's promise.
func TestFailedGuardRollsBackCleanly(t *testing.T) {
	h := newHarness(t)
	h.exec(t, `UPDATE workflow_transitions SET guards = '["has_slug"]' WHERE from_state = 'draft' AND to_state = 'review'`)
	actor := h.actor(t, "editor@example.com", domain.Publish)
	doc := h.doc(t, actor, "Rolled Back")
	h.exec(t, fmt.Sprintf("UPDATE document_versions SET slug = '' WHERE id = %d", doc.CurrentVersionID))
	doc = h.reload(t, doc.UID)

	before, err := h.db.EventsForSubject(t.Context(), domain.SubjectDocument, doc.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	beforeUpdated := doc.UpdatedAt

	// The clock moves, so that an updated_at written by a rolled-back
	// transaction would be visible rather than coincidentally equal.
	h.clock.Advance(time.Hour)

	if _, err := h.engine.Do(t.Context(), Request{Document: doc, To: "review", Actor: actor}); err == nil {
		t.Fatal("the guard let the transition through")
	}

	after := h.reload(t, doc.UID)
	if after.State != "draft" {
		t.Errorf("state = %q, want draft", after.State)
	}
	if !after.UpdatedAt.Equal(beforeUpdated) {
		t.Errorf("updated_at moved to %v; a refused transition wrote to the row", after.UpdatedAt)
	}
	if after.AssignedTo != doc.AssignedTo || !after.DueAt.Equal(doc.DueAt) {
		t.Errorf("an effect was applied by a refused transition: assigned_to %d, due_at %v",
			after.AssignedTo, after.DueAt)
	}
	got, err := h.db.EventsForSubject(t.Context(), domain.SubjectDocument, doc.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(before) {
		t.Errorf("a refused transition wrote %d events", len(got)-len(before))
	}
}

// TestApprovalsMetCountsTheCurrentVersionOnly is PLAN.md M4 acceptance 7.
//
// Approvals attach to a version, not a document (DESIGN.md 5.5). Edit the
// document, a new version exists, and the sign-off that satisfied the guard a
// moment ago no longer does.
func TestApprovalsMetCountsTheCurrentVersionOnly(t *testing.T) {
	h := newHarness(t)
	h.exec(t, `UPDATE workflow_transitions SET guards = '["approvals_met"]' WHERE from_state = 'draft' AND to_state = 'review'`)
	h.exec(t, `UPDATE workflow_states SET required_approvals = 1 WHERE slug = 'draft'`)

	actor := h.actor(t, "editor@example.com", domain.Publish)
	doc := h.doc(t, actor, "Signed Off")

	// Unapproved: refused, and the reason counts.
	_, err := h.engine.Do(t.Context(), Request{Document: doc, To: "review", Actor: actor})
	if g, ok := domain.GuardOf(err); !ok || g != domain.GuardApprovalsMet {
		t.Fatalf("an unapproved document moved, or was refused by something else: %v", err)
	}
	if !strings.Contains(err.Error(), "0 of 1") {
		t.Errorf("the reason does not say how far short it is: %v", err)
	}

	if _, err := h.db.CreateApproval(t.Context(), store.NewApproval{
		DocumentID: doc.ID, VersionID: doc.CurrentVersionID, State: "draft",
		UserID: actor.User.ID, CreatedAt: start,
	}); err != nil {
		t.Fatalf("CreateApproval: %v", err)
	}

	menu, err := h.engine.Available(t.Context(), h.reload(t, doc.UID), actor)
	if err != nil {
		t.Fatal(err)
	}
	if a := allowed(t, menu, "review"); !a.Permitted {
		t.Fatalf("an approved document is still refused: %s", a.Reason)
	}

	// Now a new version. The approval stays on the old one, and the guard
	// counts the current one.
	if _, _, err := h.db.Checkout(t.Context(), doc.ID, actor.User.ID, start, start.Add(time.Hour),
		domain.Event{Type: events.DocumentCheckedOut, ActorID: actor.User.ID, OccurredAt: start}); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if _, _, err := h.db.Checkin(t.Context(), doc.ID, actor.User.ID, start, "one",
		domain.Event{Type: events.DocumentCheckedIn, ActorID: actor.User.ID, OccurredAt: start}); err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	// The first checkout adopted the draft the document was created with, so
	// it is this second one that opens a new version -- which is the change
	// the guard has to notice.
	if _, _, err := h.db.Checkout(t.Context(), doc.ID, actor.User.ID, start, start.Add(time.Hour),
		domain.Event{Type: events.DocumentCheckedOut, ActorID: actor.User.ID, OccurredAt: start}); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	reloaded := h.reload(t, doc.UID)
	if reloaded.CurrentVersionID == doc.CurrentVersionID {
		t.Fatal("no new version was created, so this test proves nothing")
	}

	menu, err = h.engine.Available(t.Context(), reloaded, actor)
	if err != nil {
		t.Fatal(err)
	}
	a := allowed(t, menu, "review")
	if a.Permitted {
		t.Error("the approval of the previous version still satisfies the guard; changes must invalidate sign-off")
	}
	if a.Guard != domain.GuardApprovalsMet {
		t.Errorf("the refusal names %q, want approvals_met", a.Guard)
	}
}

// TestEffectsAreApplied covers all four effects through the transitions the
// default workflow declares them on.
func TestEffectsAreApplied(t *testing.T) {
	h := newHarness(t)
	editor := h.actor(t, "editor@example.com", domain.Publish)
	other := h.actor(t, "other@example.com", domain.Read)
	doc := h.doc(t, editor, "Effects")

	// clear_assignee and set_due_in, both on submit.
	h.exec(t, fmt.Sprintf("UPDATE documents SET assigned_to = %d WHERE id = %d", editor.User.ID, doc.ID))
	doc = h.reload(t, doc.UID)

	moved := h.move(t, editor, doc, "review", "")
	if moved.AssignedTo != 0 {
		t.Errorf("assigned_to = %d after clear_assignee, want nobody", moved.AssignedTo)
	}
	if want := start.Add(48 * time.Hour); !moved.DueAt.Equal(want) {
		t.Errorf("due_at = %v after set_due_in 48h, want %v; the due date is computed from the injected clock", moved.DueAt, want)
	}

	// clear_approvals, on reject. An approval of the current version is
	// discarded by the move back to draft.
	if _, err := h.db.CreateApproval(t.Context(), store.NewApproval{
		DocumentID: doc.ID, VersionID: doc.CurrentVersionID, State: "review",
		UserID: editor.User.ID, CreatedAt: start,
	}); err != nil {
		t.Fatalf("CreateApproval: %v", err)
	}
	moved = h.move(t, editor, moved, "draft", "needs work")
	if n, err := h.db.CountApprovals(t.Context(), doc.CurrentVersionID, "review"); err != nil || n != 0 {
		t.Errorf("CountApprovals = %d, %v after clear_approvals, want 0", n, err)
	}

	// assign_to_actor, on revise. The document is walked to published first,
	// and handed to somebody else there, so that what the revise picks up is
	// visibly a change of assignee rather than the absence of one.
	moved = h.move(t, editor, moved, "review", "")
	moved = h.move(t, editor, moved, "approved", "")
	moved = h.move(t, editor, moved, "published", "")
	h.exec(t, fmt.Sprintf("UPDATE documents SET assigned_to = %d WHERE id = %d", other.User.ID, doc.ID))
	moved = h.reload(t, doc.UID)
	moved = h.move(t, editor, moved, "draft", "")
	if moved.AssignedTo != editor.User.ID {
		t.Errorf("assigned_to = %d after assign_to_actor, want the actor %d", moved.AssignedTo, editor.User.ID)
	}
}

// TestTransitionWritesOneEvent is invariant 7: every state-changing operation
// writes an event carrying enough to reconstruct what happened.
func TestTransitionWritesOneEvent(t *testing.T) {
	h := newHarness(t)
	editor := h.actor(t, "editor@example.com", domain.Publish)
	doc := h.doc(t, editor, "Audited")

	h.move(t, editor, doc, "review", "")

	got, err := h.db.EventsForSubject(t.Context(), domain.SubjectDocument, doc.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("the document has %d events, want the creation and the move: %+v", len(got), got)
	}
	e := got[0]
	if e.Type != events.DocumentTransitioned {
		t.Fatalf("the newest event is %q, want %q", e.Type, events.DocumentTransitioned)
	}
	if e.ActorID != editor.User.ID {
		t.Errorf("the event names actor %d, want %d", e.ActorID, editor.User.ID)
	}
	for _, key := range []string{"uid", "transition", "from", "to", "guards", "effects", "due_at"} {
		if _, ok := e.Payload[key]; !ok {
			t.Errorf("the payload has no %q: %+v", key, e.Payload)
		}
	}
	if e.Payload["from"] != "draft" || e.Payload["to"] != "review" {
		t.Errorf("the payload does not reconstruct the move: %+v", e.Payload)
	}
}

// TestNoteIsRecordedAndRequired covers the one guard that is a property of the
// request, and the NeedsNote hint Available carries so that a client offers a
// note field rather than treating the refusal as final.
func TestNoteIsRecordedAndRequired(t *testing.T) {
	h := newHarness(t)
	editor := h.actor(t, "editor@example.com", domain.Publish)
	doc := h.doc(t, editor, "Rejected")
	doc = h.move(t, editor, doc, "review", "")

	menu, err := h.engine.Available(t.Context(), doc, editor)
	if err != nil {
		t.Fatal(err)
	}
	reject := allowed(t, menu, "draft")
	if !reject.NeedsNote {
		t.Error("reject declares note_required and Available does not say so")
	}
	if reject.Permitted {
		t.Error("Available permitted a note-requiring transition when it was asked with no note")
	}
	if reject.Guard != domain.GuardNoteRequired {
		t.Errorf("the refusal names %q, want note_required", reject.Guard)
	}

	moved := h.move(t, editor, doc, "draft", "the lede is buried")
	if moved.State != "draft" {
		t.Errorf("state = %q, want draft", moved.State)
	}

	got, err := h.db.EventsForSubject(t.Context(), domain.SubjectDocument, doc.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Payload["note"] != "the lede is buried" {
		t.Errorf("the note is not in the event payload: %+v", got[0].Payload)
	}
}

// TestPrivilegeIsResolvedAgainstTheDocument checks that a transition's
// privilege is checked against this document rather than in the abstract, so
// that a grant scoped to one state or one workflow does what it says.
func TestPrivilegeIsResolvedAgainstTheDocument(t *testing.T) {
	h := newHarness(t)

	// An actor whose only grant is Publish over documents in "approved". They
	// may publish, and nothing else.
	u, err := h.db.CreateUser(t.Context(), store.NewUser{
		UID: ids.MustNew(start), Email: "publisher@example.com", Name: "Publisher",
		CreatedAt: start,
	})
	if err != nil {
		t.Fatal(err)
	}
	role, err := h.db.CreateRole(t.Context(), "publisher", "Publisher")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.db.AssignRole(t.Context(), u.ID, role.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.CreateGrant(t.Context(), domain.Grant{
		RoleID: role.ID, Privilege: domain.Publish, CreatedAt: start,
		Scope: domain.Scope{State: domain.Ref("approved"), CategoryDeep: true},
	}); err != nil {
		t.Fatal(err)
	}
	publisher, err := h.db.Identity(t.Context(), u.ID)
	if err != nil {
		t.Fatal(err)
	}

	editor := h.actor(t, "editor@example.com", domain.Publish)
	doc := h.doc(t, editor, "Scoped")

	// In draft the publisher's grant does not match, so they may do nothing.
	menu, err := h.engine.Available(t.Context(), doc, publisher)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range menu {
		if a.Permitted {
			t.Errorf("a grant scoped to state=approved permitted %s from draft", a.Transition.Name)
		}
	}

	// In approved it does.
	doc = h.move(t, editor, doc, "review", "")
	doc = h.move(t, editor, doc, "approved", "")
	if got := h.move(t, publisher, doc, "published", ""); got.State != "published" {
		t.Errorf("state = %q, want published", got.State)
	}
}

// TestPublishedIsAStateNotAnExit is DESIGN.md 5.4's correction of the system we
// learned from, which removed a published document from workflow entirely so
// that a live document was nowhere.
func TestPublishedIsAStateNotAnExit(t *testing.T) {
	h := newHarness(t)
	editor := h.actor(t, "editor@example.com", domain.Publish)
	doc := h.doc(t, editor, "Live")

	doc = h.move(t, editor, doc, "review", "")
	doc = h.move(t, editor, doc, "approved", "")
	doc = h.move(t, editor, doc, "published", "")

	menu, err := h.engine.Available(t.Context(), doc, editor)
	if err != nil {
		t.Fatal(err)
	}
	if len(menu) == 0 {
		t.Fatal("a published document has nowhere to go; it is a state, not an exit")
	}
	if a := allowed(t, menu, "draft"); !a.Permitted {
		t.Errorf("a published document cannot be revised: %s", a.Reason)
	}
}
