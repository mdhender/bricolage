// Copyright (c) 2026 Michael D Henderson.

package service

import (
	"errors"
	"strings"
	"testing"

	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/events"
)

// Comments and approvals at the use-case level (PLAN.md M11).
//
// These assert on emitted events as well as returned values: an operation that
// does not write its event is not finished (invariant 7, AGENTS.md). The
// acceptance criterion each covers is named on the test.

// collabHarness is a service with an element type, an editor holding Publish
// over everything, a second editor, and a writer who may edit but not approve.
type collabHarness struct {
	*harness
	editor domain.Identity
	second domain.Identity
	writer domain.Identity
}

func newCollabHarness(t *testing.T) *collabHarness {
	t.Helper()
	h := newHarness(t)
	h.elementType(t, "story", domain.KindStory)
	return &collabHarness{
		harness: h,
		editor:  h.admin(t, "editor@example.com", "correct horse battery", domain.Publish),
		second:  h.admin(t, "second@example.com", "correct horse battery", domain.Publish),
		writer: h.userWithGrant(t, "writer@example.com", "correct horse battery",
			domain.Grant{Privilege: domain.Edit}),
	}
}

// inReview creates a document, checks it in, and submits it, which is the
// state an approval is about. The check-in matters: an approval attaches to a
// version, and a version still being written is not one to sign off on.
func (h *collabHarness) inReview(t *testing.T, title, slug string) domain.Document {
	t.Helper()
	view, err := h.CreateDocument(t.Context(), h.editor, domain.NewDocument{
		SiteID: h.siteID, Kind: domain.KindStory, ElementTypeKey: "story",
		Title: title, Slug: slug, CoverDate: "2026-03-01",
	})
	if err != nil {
		t.Fatalf("CreateDocument(%q): %v", title, err)
	}
	uid := view.Document.UID
	if _, err := h.Checkout(t.Context(), h.editor, uid); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if _, err := h.Checkin(t.Context(), h.editor, uid, "first cut"); err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	moved, err := h.Transition(t.Context(), h.editor, uid, "review", "")
	if err != nil {
		t.Fatalf("Transition to review: %v", err)
	}
	return moved.Document
}

// TestApprovingTwiceIsIdempotent is PLAN.md M11 acceptance 1.
func TestApprovingTwiceIsIdempotent(t *testing.T) {
	h := newCollabHarness(t)
	doc := h.inReview(t, "Signed Off Twice", "signed-off-twice")

	first, err := h.Approve(t.Context(), h.editor, doc.UID)
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if !first.Changed || first.Count() != 1 {
		t.Fatalf("the first approval reported changed = %t, count = %d", first.Changed, first.Count())
	}
	// Migration 0011 raised the review state to one approval, so one
	// sign-off is what the process wants.
	if first.Required != 1 || !first.Met() {
		t.Errorf("required = %d, met = %t; migration 0011 asks the review state for one approval",
			first.Required, first.Met())
	}

	second, err := h.Approve(t.Context(), h.editor, doc.UID)
	if err != nil {
		t.Fatalf("approving twice is an error: %v", err)
	}
	if second.Changed {
		t.Error("the second approval reported that it recorded one")
	}
	if second.Count() != 1 {
		t.Errorf("count = %d after approving twice, want 1: not two rows", second.Count())
	}

	if written := h.eventsOfType(t, events.DocumentApproved); len(written) != 1 {
		t.Errorf("approving twice wrote %d events, want 1: nothing changed the second time", len(written))
	}
}

// TestApprovalIsAuthorisedByTheTransitionItSatisfies is the rule an approval
// is gated by: the person whose sign-off the process counts is the person the
// process would let make the move.
func TestApprovalIsAuthorisedByTheTransitionItSatisfies(t *testing.T) {
	h := newCollabHarness(t)
	doc := h.inReview(t, "Not Yours To Approve", "not-yours")

	// The default workflow's Approve needs "create"; the writer holds "edit".
	// The refusal is a 403 rather than a 404: they can see the document.
	_, err := h.Approve(t.Context(), h.writer, doc.UID)
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a writer approved a document out of review: %v", err)
	}
	if !strings.Contains(err.Error(), "create") {
		t.Errorf("the refusal is %q, want it to name the privilege moving it out of review asks for", err)
	}

	// A state no transition counts approvals in refuses everybody, including
	// an administrator: it is a statement about the process rather than about
	// the person, so a bigger grant would not help.
	back, err := h.Transition(t.Context(), h.editor, doc.UID, "draft", "not yet")
	if err != nil {
		t.Fatalf("Transition to draft: %v", err)
	}
	_, err = h.Approve(t.Context(), h.editor, back.Document.UID)
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a document in draft was approved, and no transition out of draft counts approvals: %v", err)
	}
}

// TestApprovingAWorkingDraftIsRefused is the other half of "approvals attach
// to a version": a draft is edited in place, so an approval of one would
// survive the very change it exists to be invalidated by.
func TestApprovingAWorkingDraftIsRefused(t *testing.T) {
	h := newCollabHarness(t)
	view, err := h.CreateDocument(t.Context(), h.editor, domain.NewDocument{
		SiteID: h.siteID, Kind: domain.KindStory, ElementTypeKey: "story",
		Title: "Still Being Written", Slug: "still-being-written", CoverDate: "2026-03-01",
	})
	if err != nil {
		t.Fatal(err)
	}
	uid := view.Document.UID
	if _, err := h.Transition(t.Context(), h.editor, uid, "review", ""); err != nil {
		t.Fatalf("Transition to review: %v", err)
	}

	_, err = h.Approve(t.Context(), h.editor, uid)
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("an open working draft was approved: %v", err)
	}
	if !strings.Contains(err.Error(), "working draft") {
		t.Errorf("the refusal is %q, want it to say the current version is still a draft", err)
	}
}

// TestCheckinResetsTheApprovalCount is PLAN.md M11 acceptance 3.
func TestCheckinResetsTheApprovalCount(t *testing.T) {
	h := newCollabHarness(t)
	doc := h.inReview(t, "Then And Now", "then-and-now")

	if _, err := h.Approve(t.Context(), h.editor, doc.UID); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	// A checkout opens version 2 and a check-in closes it. Either way the
	// current version is no longer the one that was signed off on.
	if _, err := h.Checkout(t.Context(), h.editor, doc.UID); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if _, err := h.Checkin(t.Context(), h.editor, doc.UID, "second cut"); err != nil {
		t.Fatalf("Checkin: %v", err)
	}

	now, err := h.Approvals(t.Context(), h.editor, doc.UID, 0)
	if err != nil {
		t.Fatalf("Approvals: %v", err)
	}
	if now.Version.Number != 2 {
		t.Fatalf("the current version is %d, want 2; this test proves nothing otherwise", now.Version.Number)
	}
	if now.Count() != 0 {
		t.Errorf("version 2 carries %d approvals, want 0: a change invalidates sign-off", now.Count())
	}
	if now.Met() {
		t.Error("the guard would be satisfied against a version nobody has approved")
	}

	// The old version's approvals remain queryable.
	before, err := h.Approvals(t.Context(), h.editor, doc.UID, 1)
	if err != nil {
		t.Fatalf("Approvals(version 1): %v", err)
	}
	if before.Count() != 1 {
		t.Errorf("version 1 carries %d approvals, want the one that was recorded", before.Count())
	}
	if len(before.Approvals) != 1 || before.Approvals[0].UserID != h.editor.User.ID {
		t.Errorf("version 1's approvals are %+v, want the editor's", before.Approvals)
	}

	// And the transition that needs one is refused again, which is the
	// behaviour the count exists for.
	_, err = h.Transition(t.Context(), h.editor, doc.UID, "approved", "")
	if g, ok := domain.GuardOf(err); !ok || g != domain.GuardApprovalsMet {
		t.Errorf("the move out of review was allowed against an unapproved version: %v", err)
	}
}

// TestClearApprovalsRemovesThem is PLAN.md M11 acceptance 4. The default
// workflow's Reject declares clear_approvals, so sending a story back discards
// the sign-off it had collected.
func TestClearApprovalsRemovesThem(t *testing.T) {
	h := newCollabHarness(t)
	doc := h.inReview(t, "Sent Back", "sent-back")

	if _, err := h.Approve(t.Context(), h.editor, doc.UID); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if _, err := h.Transition(t.Context(), h.editor, doc.UID, "draft", "the lede is buried"); err != nil {
		t.Fatalf("Transition to draft: %v", err)
	}

	// The version has not changed -- nobody checked anything in -- so a count
	// of zero here is the effect and nothing else.
	got, err := h.Approvals(t.Context(), h.editor, doc.UID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version.Number != 1 {
		t.Fatalf("the current version is %d, want 1; clear_approvals is not what emptied it", got.Version.Number)
	}
	if got.Count() != 0 {
		t.Errorf("count = %d after clear_approvals, want 0", got.Count())
	}
}

// TestWithdrawApproval covers the second writer DESIGN.md 12 names, and its
// idempotence.
func TestWithdrawApproval(t *testing.T) {
	h := newCollabHarness(t)
	doc := h.inReview(t, "Taken Back", "taken-back")

	if _, err := h.Approve(t.Context(), h.editor, doc.UID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Approve(t.Context(), h.second, doc.UID); err != nil {
		t.Fatal(err)
	}

	got, err := h.WithdrawApproval(t.Context(), h.editor, doc.UID)
	if err != nil {
		t.Fatalf("WithdrawApproval: %v", err)
	}
	if !got.Changed || got.Count() != 1 {
		t.Errorf("changed = %t, count = %d after withdrawing one of two", got.Changed, got.Count())
	}
	if domain.ApprovedBy(got.Approvals, got.State, h.editor.User.ID) {
		t.Error("the withdrawn approval is still there")
	}
	if !domain.ApprovedBy(got.Approvals, got.State, h.second.User.ID) {
		t.Error("withdrawing one approval took somebody else's with it")
	}

	again, err := h.WithdrawApproval(t.Context(), h.editor, doc.UID)
	if err != nil {
		t.Fatalf("withdrawing an approval that is not there is an error: %v", err)
	}
	if again.Changed {
		t.Error("the second withdrawal reported that it removed something")
	}
	if written := h.eventsOfType(t, events.DocumentApprovalWithdrawn); len(written) != 1 {
		t.Errorf("withdrawing twice wrote %d events, want 1", len(written))
	}
}

// TestAnUnresolvedThreadBlocksATransition is PLAN.md M11 acceptance 5.
func TestAnUnresolvedThreadBlocksATransition(t *testing.T) {
	h := newCollabHarness(t)
	doc := h.inReview(t, "Under Discussion", "under-discussion")
	if _, err := h.Approve(t.Context(), h.editor, doc.UID); err != nil {
		t.Fatal(err)
	}

	// Commenting needs only read access: raising a concern is what somebody
	// who may not touch the story does.
	_, root, err := h.Comment(t.Context(), h.writer, doc.UID, "the second source is not named", "")
	if err != nil {
		t.Fatalf("Comment: %v", err)
	}
	// Two replies, which are part of one discussion rather than two more open
	// questions.
	for _, body := range []string{"agreed", "chasing it now"} {
		if _, _, err := h.Comment(t.Context(), h.editor, doc.UID, body, root.UID); err != nil {
			t.Fatalf("Comment(reply): %v", err)
		}
	}

	_, err = h.Transition(t.Context(), h.editor, doc.UID, "approved", "")
	if g, ok := domain.GuardOf(err); !ok || g != domain.GuardCommentsResolved {
		t.Fatalf("an open thread did not block the move: %v", err)
	}
	if !strings.Contains(err.Error(), "1 thread unresolved") {
		t.Errorf("the refusal is %q, want it to name the open thread count", err)
	}

	// A reply cannot be resolved on its own, and the refusal names the thread
	// to resolve instead.
	_, threads, err := h.Comments(t.Context(), h.editor, doc.UID)
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 1 || len(threads[0].Replies) != 2 {
		t.Fatalf("the discussion is %d threads with %d replies, want 1 and 2",
			len(threads), len(threads[0].Replies))
	}
	_, _, err = h.ResolveComment(t.Context(), h.editor, threads[0].Replies[0].UID)
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a reply was resolved on its own: %v", err)
	}
	if !strings.Contains(err.Error(), root.UID) {
		t.Errorf("the refusal is %q, want it to name the thread %s", err, root.UID)
	}

	// Resolve the thread, and the move goes through.
	if _, _, err := h.ResolveComment(t.Context(), h.editor, root.UID); err != nil {
		t.Fatalf("ResolveComment: %v", err)
	}
	moved, err := h.Transition(t.Context(), h.editor, doc.UID, "approved", "")
	if err != nil {
		t.Fatalf("the move is still refused after the thread was resolved: %v", err)
	}
	if moved.Document.State != "approved" {
		t.Errorf("state = %q, want approved", moved.Document.State)
	}
}

// TestEveryCommentAndApprovalWritesAnEvent is PLAN.md M11 acceptance 6.
func TestEveryCommentAndApprovalWritesAnEvent(t *testing.T) {
	h := newCollabHarness(t)
	doc := h.inReview(t, "On The Record", "on-the-record")

	_, root, err := h.Comment(t.Context(), h.writer, doc.UID, "the lede is buried", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.Comment(t.Context(), h.editor, doc.UID, "fixed", root.UID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.ResolveComment(t.Context(), h.editor, root.UID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Approve(t.Context(), h.editor, doc.UID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.WithdrawApproval(t.Context(), h.editor, doc.UID); err != nil {
		t.Fatal(err)
	}

	commented := h.eventsOfType(t, events.DocumentCommented)
	if len(commented) != 2 {
		t.Fatalf("%d comment events, want one per comment and reply", len(commented))
	}
	// Newest first. The reply says so, and the thread it belongs to does not.
	if commented[0].Payload["reply"] != true {
		t.Errorf("the reply's event payload is %v, want it marked as a reply", commented[0].Payload)
	}
	if _, ok := commented[1].Payload["reply"]; ok {
		t.Errorf("the thread's event payload is %v, want no reply marker", commented[1].Payload)
	}
	if commented[1].Payload["body"] != "the lede is buried" {
		t.Errorf("the event payload is %v, want the comment somebody wrote", commented[1].Payload)
	}
	if commented[1].ActorID != h.writer.User.ID {
		t.Errorf("the comment event names actor %d, want the writer %d", commented[1].ActorID, h.writer.User.ID)
	}

	for _, kind := range []string{
		events.DocumentCommentResolved,
		events.DocumentApproved,
		events.DocumentApprovalWithdrawn,
	} {
		written := h.eventsOfType(t, kind)
		if len(written) != 1 {
			t.Errorf("%d %s events, want 1", len(written), kind)
			continue
		}
		if written[0].SubjectKind != domain.SubjectDocument || written[0].SubjectID != doc.ID {
			t.Errorf("%s is about %s %d, want document %d",
				kind, written[0].SubjectKind, written[0].SubjectID, doc.ID)
		}
		if written[0].Payload["uid"] != doc.UID {
			t.Errorf("%s payload is %v, want it to name the document", kind, written[0].Payload)
		}
	}

	// The whole lot is in the document's history, which is the question a
	// person actually asks (DESIGN.md 10).
	history, err := h.DocumentEvents(t.Context(), h.editor, doc.UID, 100)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range history {
		seen[e.Type] = true
	}
	for _, kind := range []string{
		events.DocumentCommented,
		events.DocumentCommentResolved,
		events.DocumentApproved,
		events.DocumentApprovalWithdrawn,
	} {
		if !seen[kind] {
			t.Errorf("the document's history does not carry %s", kind)
		}
	}
}

// TestCommentRefusals covers the ways a comment is turned down.
func TestCommentRefusals(t *testing.T) {
	h := newCollabHarness(t)
	doc := h.inReview(t, "Refusals", "refusals")
	other := h.inReview(t, "Elsewhere", "elsewhere")

	if _, _, err := h.Comment(t.Context(), h.editor, doc.UID, "   ", ""); !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("an empty comment was accepted: %v", err)
	}
	if _, _, err := h.Comment(t.Context(), h.editor, doc.UID, strings.Repeat("x", domain.MaxCommentBody+1), ""); !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("an oversized comment was accepted: %v", err)
	}

	// Replying to a thread on another document files the reply where nobody
	// asked for it, so it is refused rather than redirected.
	_, elsewhere, err := h.Comment(t.Context(), h.editor, other.UID, "over here", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.Comment(t.Context(), h.editor, doc.UID, "over there", elsewhere.UID); !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("a reply crossed documents: %v", err)
	}

	// A person who may not read the document is told it is not there.
	nobody := h.userWithGrant(t, "nobody@example.com", "correct horse battery", domain.Grant{})
	if _, _, err := h.Comment(t.Context(), nobody, doc.UID, "hello", ""); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("somebody with no access commented, or was told the document exists: %v", err)
	}
}

// TestRepliesAreOneLevelDeep is DESIGN.md 5.5's rule: replying to a reply
// joins the thread rather than starting a branch, so that "is this settled"
// has one answer per discussion.
func TestRepliesAreOneLevelDeep(t *testing.T) {
	h := newCollabHarness(t)
	doc := h.inReview(t, "One Level", "one-level")

	_, root, err := h.Comment(t.Context(), h.editor, doc.UID, "the lede is buried", "")
	if err != nil {
		t.Fatal(err)
	}
	_, reply, err := h.Comment(t.Context(), h.editor, doc.UID, "agreed", root.UID)
	if err != nil {
		t.Fatal(err)
	}
	_, deeper, err := h.Comment(t.Context(), h.editor, doc.UID, "fixed", reply.UID)
	if err != nil {
		t.Fatalf("replying to a reply: %v", err)
	}
	if deeper.InReplyTo != root.ID {
		t.Errorf("the reply to a reply hangs off %d, want the thread root %d", deeper.InReplyTo, root.ID)
	}

	_, threads, err := h.Comments(t.Context(), h.editor, doc.UID)
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 1 || len(threads[0].Replies) != 2 {
		t.Errorf("the discussion is %d threads with %d replies, want 1 and 2",
			len(threads), len(threads[0].Replies))
	}
}

// TestResolvingNeedsEditOrAuthorship is the split M11 draws: raising a concern
// needs only the ability to see the document, and deciding it is settled is an
// act on the document's process.
func TestResolvingNeedsEditOrAuthorship(t *testing.T) {
	h := newCollabHarness(t)
	doc := h.inReview(t, "Who May Close It", "who-may-close-it")

	reader := h.userWithGrant(t, "reader@example.com", "correct horse battery",
		domain.Grant{Privilege: domain.Read})

	// The reader may comment.
	_, theirs, err := h.Comment(t.Context(), reader, doc.UID, "is this on the record?", "")
	if err != nil {
		t.Fatalf("a reader could not comment: %v", err)
	}
	// And may resolve their own thread: they raised the question.
	if _, _, err := h.ResolveComment(t.Context(), reader, theirs.UID); err != nil {
		t.Errorf("the author of a thread could not resolve it: %v", err)
	}

	// But not somebody else's.
	_, editors, err := h.Comment(t.Context(), h.editor, doc.UID, "check the byline", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.ResolveComment(t.Context(), reader, editors.UID); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("a reader resolved somebody else's thread: %v", err)
	}
}
