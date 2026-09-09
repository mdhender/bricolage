// Copyright (c) 2026 Michael D Henderson.

package service

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/events"
)

// Assignment, due dates, and queues at the use-case level (PLAN.md M5).
//
// These assert on emitted events as well as returned values: an operation that
// does not write its event is not finished (invariant 7, AGENTS.md).

// queueHarness is a service with an element type, an editor, and a writer.
type queueHarness struct {
	*harness
	editor domain.Identity
	writer domain.Identity
}

func newQueueHarness(t *testing.T) *queueHarness {
	t.Helper()
	h := newHarness(t)
	h.elementType(t, "story", domain.KindStory)

	// Publish over everything, which is what "cmsdb bootstrap admin" produces
	// and what the top of the default workflow needs.
	editor := h.admin(t, "editor@example.com", "correct horse battery", domain.Publish)
	writer := h.userWithGrant(t, "writer@example.com", "correct horse battery",
		domain.Grant{Privilege: domain.Edit})
	return &queueHarness{harness: h, editor: editor, writer: writer}
}

// newDoc creates a document that can actually be submitted: the default
// workflow's submit transition declares has_slug, so a fixture without one
// would be a fixture whose every transition test failed for the wrong reason.
func (h *queueHarness) newDoc(t *testing.T, actor domain.Identity, title, slug string) DocumentView {
	t.Helper()
	view, err := h.CreateDocument(t.Context(), actor, domain.NewDocument{
		SiteID: h.siteID, Kind: domain.KindStory, ElementTypeKey: "story",
		Title: title, Slug: slug, CoverDate: "2026-03-01",
	})
	if err != nil {
		t.Fatalf("CreateDocument(%q): %v", title, err)
	}
	return view
}

// TestAssignWritesAnEvent is PLAN.md M5 acceptance 2 for the assignment half.
func TestAssignWritesAnEvent(t *testing.T) {
	h := newQueueHarness(t)
	doc := h.newDoc(t, h.editor, "Assign me", "assign-me")

	due := h.Now().Add(48 * time.Hour)
	got, err := h.Assign(t.Context(), h.editor, doc.Document.UID, h.writer.User.UID, &due)
	if err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if got.Document.AssignedTo != h.writer.User.ID {
		t.Errorf("assigned to %d, want the writer %d", got.Document.AssignedTo, h.writer.User.ID)
	}
	if !got.Document.DueAt.Equal(due) {
		t.Errorf("due %v, want %v", got.Document.DueAt, due)
	}

	// The event carries enough to reconstruct what happened (invariant 7):
	// who it went to, and when it was wanted.
	written := h.eventsOfType(t, events.DocumentAssigned)
	if len(written) != 1 {
		t.Fatalf("%d assignment events, want 1", len(written))
	}
	if written[0].Payload["assignee"] != h.writer.User.UID {
		t.Errorf("the event names %v as the assignee, want %q",
			written[0].Payload["assignee"], h.writer.User.UID)
	}
	if written[0].Payload["due_at"] == nil {
		t.Error("the event does not record the due date the same request set")
	}
	if written[0].ActorID != h.editor.User.ID {
		t.Errorf("the event's actor is %d, want the editor %d", written[0].ActorID, h.editor.User.ID)
	}
}

// TestAssignToSelf: "me" saves a client a round trip to /api/v1/me, and it is
// what the saved queue "mine" resolves per request.
func TestAssignToSelf(t *testing.T) {
	h := newQueueHarness(t)
	doc := h.newDoc(t, h.editor, "Mine", "mine")

	got, err := h.Assign(t.Context(), h.editor, doc.Document.UID, SelfAssignee, nil)
	if err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if got.Document.AssignedTo != h.editor.User.ID {
		t.Errorf(`"me" resolved to %d, want the caller %d`, got.Document.AssignedTo, h.editor.User.ID)
	}
	if !got.Document.DueAt.IsZero() {
		t.Errorf("due %v, want no deadline: none was given", got.Document.DueAt)
	}
}

// TestUnassignKeepsTheDueDate: a deadline belongs to the work rather than to
// whoever happens to be holding it. Clearing one because somebody put the
// document down is how a story quietly stops being late.
func TestUnassignKeepsTheDueDate(t *testing.T) {
	h := newQueueHarness(t)
	doc := h.newDoc(t, h.editor, "Put down", "put-down")
	due := h.Now().Add(24 * time.Hour)
	if _, err := h.Assign(t.Context(), h.editor, doc.Document.UID, h.writer.User.UID, &due); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	got, err := h.Unassign(t.Context(), h.editor, doc.Document.UID)
	if err != nil {
		t.Fatalf("Unassign: %v", err)
	}
	if got.Document.AssignedTo != 0 {
		t.Errorf("assigned to %d after unassigning, want nobody", got.Document.AssignedTo)
	}
	if !got.Document.DueAt.Equal(due) {
		t.Errorf("due %v after unassigning, want the %v it already had", got.Document.DueAt, due)
	}

	written := h.eventsOfType(t, events.DocumentUnassigned)
	if len(written) != 1 {
		t.Fatalf("%d unassignment events, want 1", len(written))
	}
	if written[0].Payload["was"] != h.writer.User.UID {
		t.Errorf("the event says it was taken from %v, want %q", written[0].Payload["was"], h.writer.User.UID)
	}
}

// TestSetDueWritesAnEvent is PLAN.md M5 acceptance 2 for the due-date half.
func TestSetDueWritesAnEvent(t *testing.T) {
	h := newQueueHarness(t)
	doc := h.newDoc(t, h.editor, "Due", "due")
	if _, err := h.Assign(t.Context(), h.editor, doc.Document.UID, h.writer.User.UID, nil); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	due := h.Now().Add(72 * time.Hour)
	got, err := h.SetDue(t.Context(), h.editor, doc.Document.UID, &due)
	if err != nil {
		t.Fatalf("SetDue: %v", err)
	}
	if !got.Document.DueAt.Equal(due) {
		t.Errorf("due %v, want %v", got.Document.DueAt, due)
	}
	// Setting a deadline does not take the work off whoever has it.
	if got.Document.AssignedTo != h.writer.User.ID {
		t.Errorf("assigned to %d after setting a due date, want the writer %d",
			got.Document.AssignedTo, h.writer.User.ID)
	}

	cleared, err := h.SetDue(t.Context(), h.editor, doc.Document.UID, nil)
	if err != nil {
		t.Fatalf("SetDue(nil): %v", err)
	}
	if !cleared.Document.DueAt.IsZero() {
		t.Errorf("due %v after clearing, want no deadline", cleared.Document.DueAt)
	}
	if cleared.Document.AssignedTo != h.writer.User.ID {
		t.Errorf("clearing the deadline unassigned the document")
	}

	written := h.eventsOfType(t, events.DocumentDueChanged)
	if len(written) != 2 {
		t.Fatalf("%d due-date events, want 2", len(written))
	}
	// EventsOfType is newest first, so the clearing is written[0].
	if written[0].Payload["cleared"] != true {
		t.Errorf("the clearing event is %v, want it to say the deadline was cleared", written[0].Payload)
	}
	if written[1].Payload["due_at"] == nil {
		t.Error("the setting event does not record the deadline it set")
	}
}

// TestAssignmentNeedsEditAndNotTheLease.
//
// Two things at once, and both matter. A reader may not hand work around; and
// assigning does not require a checkout, because the lease protects the
// working draft and this touches the document row. Requiring one would mean
// taking the draft away from the person being handed the work.
func TestAssignmentNeedsEditAndNotTheLease(t *testing.T) {
	h := newQueueHarness(t)
	reader := h.userWithGrant(t, "reader@example.com", "correct horse battery",
		domain.Grant{Privilege: domain.Read})
	doc := h.newDoc(t, h.editor, "Somebody else's to give", "to-give")

	if _, err := h.Assign(t.Context(), reader, doc.Document.UID, SelfAssignee, nil); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("a reader assigned a document: %v, want ErrForbidden", err)
	}

	// Checked out by the writer, and the editor may still assign it: the two
	// operations do not contend.
	if _, err := h.Checkout(t.Context(), h.writer, doc.Document.UID); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if _, err := h.Assign(t.Context(), h.editor, doc.Document.UID, h.writer.User.UID, nil); err != nil {
		t.Errorf("assigning a checked-out document: %v, want it to succeed", err)
	}
}

// TestAssignRefusesAUserNobodyIs: a uid nobody holds is a 404 naming the user,
// not a silent assignment to nobody.
func TestAssignRefusesAUserNobodyIs(t *testing.T) {
	h := newQueueHarness(t)
	doc := h.newDoc(t, h.editor, "Assign to a ghost", "ghost")

	if _, err := h.Assign(t.Context(), h.editor, doc.Document.UID, "01JQZZZZZZZZZZZZZZZZZZZZZZ", nil); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Assign to an unknown uid returned %v, want ErrNotFound", err)
	}
	// Nothing was written: no event, and the document is still unassigned.
	if got := h.eventsOfType(t, events.DocumentAssigned); len(got) != 0 {
		t.Errorf("%d assignment events after a refused assignment, want none", len(got))
	}
	after, err := h.Document(t.Context(), h.editor, doc.Document.UID)
	if err != nil {
		t.Fatalf("Document: %v", err)
	}
	if after.Document.AssignedTo != 0 {
		t.Errorf("assigned to %d after a refused assignment, want nobody", after.Document.AssignedTo)
	}
}

// TestListDocumentsFilters is PLAN.md M5 acceptance 1 at the use-case level:
// the named query, and the authorization filter still applied over it.
func TestListDocumentsFilters(t *testing.T) {
	h := newQueueHarness(t)

	loose := h.newDoc(t, h.editor, "Nobody's", "nobodys").Document
	taken := h.newDoc(t, h.editor, "Taken", "taken").Document
	draft := h.newDoc(t, h.editor, "Still drafting", "drafting").Document

	// Two documents into review, one of them assigned. Submit is the declared
	// move and the engine is the only thing that may make it (invariant 4).
	submit(t, h, h.editor, loose)
	submit(t, h, h.editor, taken)
	if _, err := h.Assign(t.Context(), h.editor, taken.UID, h.writer.User.UID, nil); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	// The named query: PLAN.md M5 acceptance 1.
	got, err := h.ListDocuments(t.Context(), h.editor, domain.DocumentFilter{
		State: "review", Unassigned: true,
	})
	if err != nil {
		t.Fatalf("ListDocuments: %v", err)
	}
	if len(got) != 1 || got[0].UID != loose.UID {
		t.Fatalf("in review and unassigned = %v, want exactly %q", docUIDs(got), loose.UID)
	}

	// And the complements, so that the filter is doing the work rather than
	// the fixture happening to have one row.
	if got, _ := h.ListDocuments(t.Context(), h.editor, domain.DocumentFilter{State: "review"}); len(got) != 2 {
		t.Errorf("in review = %v, want two", docUIDs(got))
	}
	if got, _ := h.ListDocuments(t.Context(), h.editor, domain.DocumentFilter{State: "draft"}); len(got) != 1 {
		t.Errorf("in draft = %v, want just %q", docUIDs(got), draft.UID)
	}
	if got, _ := h.ListDocuments(t.Context(), h.editor, domain.DocumentFilter{
		AssignedTo: h.writer.User.ID,
	}); len(got) != 1 || got[0].UID != taken.UID {
		t.Errorf("assigned to the writer = %v, want %q", docUIDs(got), taken.UID)
	}

	// A stranger sees none of it. The queue filter never widens what a reader
	// may open: authz.Resolve is applied over whatever the query returned.
	stranger := h.userWithGrant(t, "stranger@example.com", "correct horse battery", domain.Grant{})
	if got, _ := h.ListDocuments(t.Context(), stranger, domain.DocumentFilter{State: "review"}); len(got) != 0 {
		t.Errorf("a stranger sees %v in review, want nothing", docUIDs(got))
	}
}

// TestListDocumentsRefusesAContradiction: assigned to somebody and to nobody
// at once is not a query with an empty answer, it is a request nobody meant.
func TestListDocumentsRefusesAContradiction(t *testing.T) {
	h := newQueueHarness(t)
	_, err := h.ListDocuments(t.Context(), h.editor, domain.DocumentFilter{
		AssignedTo: h.writer.User.ID, Unassigned: true,
	})
	if !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("ListDocuments with both returned %v, want ErrInvalid", err)
	}

	_, err = h.ResolveFilter(t.Context(), h.editor, FilterRequest{
		Assignee: SelfAssignee, Unassigned: true,
	})
	if !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("ResolveFilter with both returned %v, want ErrInvalid", err)
	}
}

// TestOverdueUsesTheInjectedClock is PLAN.md M5 acceptance 5.
//
// Nothing here waits: the fake clock is moved past the deadline and the same
// query returns a different answer, which is the whole reason every component
// that needs the time is given one (invariant 3).
func TestOverdueUsesTheInjectedClock(t *testing.T) {
	h := newQueueHarness(t)
	doc := h.newDoc(t, h.editor, "Due tomorrow", "due-tomorrow")
	due := h.Now().Add(24 * time.Hour)
	if _, err := h.Assign(t.Context(), h.editor, doc.Document.UID, h.writer.User.UID, &due); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	overdue := func() []domain.Document {
		t.Helper()
		got, err := h.ListDocuments(t.Context(), h.editor, domain.DocumentFilter{Overdue: true})
		if err != nil {
			t.Fatalf("ListDocuments: %v", err)
		}
		return got
	}

	if got := overdue(); len(got) != 0 {
		t.Fatalf("overdue = %v before the deadline, want nothing", docUIDs(got))
	}
	h.clock.Advance(25 * time.Hour)
	if got := overdue(); len(got) != 1 || got[0].UID != doc.Document.UID {
		t.Errorf("overdue = %v after the clock passed the deadline, want %q", docUIDs(got), doc.Document.UID)
	}
}

// TestQueues runs every saved definition, which is what checks that each one
// is a question this system can actually ask.
func TestQueues(t *testing.T) {
	h := newQueueHarness(t)

	loose := h.newDoc(t, h.editor, "Nobody's", "nobodys").Document
	mine := h.newDoc(t, h.editor, "Mine", "mine").Document
	submit(t, h, h.editor, loose)
	if _, err := h.Assign(t.Context(), h.editor, mine.UID, SelfAssignee, nil); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	for _, q := range h.Queues() {
		if _, _, err := h.Queue(t.Context(), h.editor, q.Slug); err != nil {
			t.Errorf("queue %q: %v", q.Slug, err)
		}
	}

	// "needs-editor" is the named form of PLAN.md M5 acceptance 1's query, and
	// it must return exactly what the equivalent filter does.
	_, got, err := h.Queue(t.Context(), h.editor, "needs-editor")
	if err != nil {
		t.Fatalf("Queue(needs-editor): %v", err)
	}
	if len(got) != 1 || got[0].UID != loose.UID {
		t.Errorf("needs-editor = %v, want %q", docUIDs(got), loose.UID)
	}
	direct, err := h.ListDocuments(t.Context(), h.editor, domain.DocumentFilter{State: "review", Unassigned: true})
	if err != nil {
		t.Fatalf("ListDocuments: %v", err)
	}
	if !sameDocs(got, direct) {
		t.Errorf("the saved queue returned %v and the same filter returned %v", docUIDs(got), docUIDs(direct))
	}

	// "mine" resolves per request, so two people asking get different lists.
	_, editors, err := h.Queue(t.Context(), h.editor, "mine")
	if err != nil {
		t.Fatalf("Queue(mine): %v", err)
	}
	if len(editors) != 1 || editors[0].UID != mine.UID {
		t.Errorf("the editor's queue = %v, want %q", docUIDs(editors), mine.UID)
	}
	_, writers, err := h.Queue(t.Context(), h.writer, "mine")
	if err != nil {
		t.Fatalf("Queue(mine) as the writer: %v", err)
	}
	if len(writers) != 0 {
		t.Errorf("the writer's queue = %v, want nothing", docUIDs(writers))
	}
}

// TestQueueRefusesASlugNobodyDefined names what is available, because a
// refusal that does not is a refusal that sends somebody to read the source.
func TestQueueRefusesASlugNobodyDefined(t *testing.T) {
	h := newQueueHarness(t)
	_, _, err := h.Queue(t.Context(), h.editor, "everything-important")
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Queue on an unknown slug returned %v, want ErrNotFound", err)
	}
	if got := err.Error(); !strings.Contains(got, "mine") {
		t.Errorf("the refusal is %q, want it to say which queues exist", got)
	}
}

// TestAssignmentEffectsFireOnTransitions is PLAN.md M5 acceptance 3.
//
// M4 asserted the two effects against an assigned_to column set by raw SQL.
// Now that there is an assignment operation, the same two are asserted against
// a document assigned the way a person assigns one -- which is the version
// that could have broken without anybody noticing.
func TestAssignmentEffectsFireOnTransitions(t *testing.T) {
	h := newQueueHarness(t)
	doc := h.newDoc(t, h.editor, "Hand it around", "hand-around").Document

	// clear_assignee, on submit.
	if _, err := h.Assign(t.Context(), h.editor, doc.UID, h.writer.User.UID, nil); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	moved, err := h.Transition(t.Context(), h.writer, doc.UID, "review", "")
	if err != nil {
		t.Fatalf("Transition to review: %v", err)
	}
	if moved.Document.AssignedTo != 0 {
		t.Errorf("assigned to %d after clear_assignee, want nobody", moved.Document.AssignedTo)
	}

	// set_due_in, on the same transition: 48h from the injected clock.
	if want := h.Now().Add(48 * time.Hour); !moved.Document.DueAt.Equal(want) {
		t.Errorf("due %v after set_due_in, want %v", moved.Document.DueAt, want)
	}

	// assign_to_actor, on revise. The document is walked to published first
	// and assigned to somebody else, so that the effect is visibly a change of
	// assignee rather than the absence of one.
	//
	// The check-in comes before the approval and both come before the move.
	// Publish declares has_checked_in_version, because the publish job it
	// schedules pins a checked-in version (PLAN.md M9); Approve is guarded by
	// approvals_met, which migration 0011 gave something to count, and an
	// approval attaches to a checked-in version rather than to a draft that is
	// still being written (PLAN.md M11).
	if _, err := h.Checkout(t.Context(), h.editor, doc.UID); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if _, err := h.Checkin(t.Context(), h.editor, doc.UID, "ready"); err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if _, err := h.Approve(t.Context(), h.editor, doc.UID); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if _, err := h.Transition(t.Context(), h.editor, doc.UID, "approved", ""); err != nil {
		t.Fatalf("Transition to approved: %v", err)
	}
	if _, err := h.Transition(t.Context(), h.editor, doc.UID, "published", ""); err != nil {
		t.Fatalf("Transition to published: %v", err)
	}
	if _, err := h.Assign(t.Context(), h.editor, doc.UID, h.writer.User.UID, nil); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	revised, err := h.Transition(t.Context(), h.editor, doc.UID, "draft", "")
	if err != nil {
		t.Fatalf("Transition to draft: %v", err)
	}
	if revised.Document.AssignedTo != h.editor.User.ID {
		t.Errorf("assigned to %d after assign_to_actor, want the actor %d",
			revised.Document.AssignedTo, h.editor.User.ID)
	}

	// The queue sees what the effect did: a transition that assigns is an
	// assignment as far as "what is on my list" is concerned.
	got, err := h.ListDocuments(t.Context(), h.editor, domain.DocumentFilter{AssignedTo: h.editor.User.ID})
	if err != nil {
		t.Fatalf("ListDocuments: %v", err)
	}
	if len(got) != 1 || got[0].UID != doc.UID {
		t.Errorf("the actor's list = %v, want %q", docUIDs(got), doc.UID)
	}
}

// submit moves a document from draft to review through the engine, which is
// the only thing that may move one (invariant 4).
func submit(t *testing.T, h *queueHarness, actor domain.Identity, doc domain.Document) {
	t.Helper()
	if _, err := h.Transition(t.Context(), actor, doc.UID, "review", ""); err != nil {
		t.Fatalf("submitting %q: %v", doc.UID, err)
	}
}

func docUIDs(docs []domain.Document) []string {
	out := make([]string, 0, len(docs))
	for _, d := range docs {
		out = append(out, d.UID)
	}
	return out
}

func sameDocs(a, b []domain.Document) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].UID != b[i].UID {
			return false
		}
	}
	return true
}
