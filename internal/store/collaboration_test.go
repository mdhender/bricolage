// Copyright (c) 2026 Michael D Henderson.

package store

import (
	"time"

	"testing"

	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/events"
	"github.com/mdhender/bricolage/internal/ids"
)

// The store half of M11, against a real in-memory database with foreign keys
// on (invariant 22). What is under test here is the two properties the SQL
// carries rather than the service: an approval is idempotent because of a
// UNIQUE index, and an open thread is counted once however many replies it
// has.

// TestApprovingTwiceIsOneRow is PLAN.md M11 acceptance 1 at the store level.
func TestApprovingTwiceIsOneRow(t *testing.T) {
	f := newDocFixture(t)
	doc, v := f.create(t, "Signed Off Twice")

	a1, created, err := f.db.CreateApproval(t.Context(), NewApproval{
		DocumentID: doc.ID, VersionID: v.ID, State: "review",
		UserID: f.author.ID, CreatedAt: f.now,
	}, f.event(f.author.ID, events.DocumentApproved))
	if err != nil || !created {
		t.Fatalf("the first approval reported created = %t, %v", created, err)
	}

	a2, created, err := f.db.CreateApproval(t.Context(), NewApproval{
		DocumentID: doc.ID, VersionID: v.ID, State: "review",
		UserID: f.author.ID, CreatedAt: f.now,
	}, f.event(f.author.ID, events.DocumentApproved))
	if err != nil {
		t.Fatalf("the second approval failed: %v", err)
	}
	if created {
		t.Error("the second approval reported that it created a row")
	}
	if a2.ID != a1.ID {
		t.Errorf("the second approval returned row %d, want the first one, %d", a2.ID, a1.ID)
	}

	got, err := f.db.ApprovalsForVersion(t.Context(), v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("the version carries %d approvals, want 1", len(got))
	}

	// One event, not two: nothing changed the second time, and invariant 7
	// asks for an event per state change rather than per request.
	if history, err := f.db.EventsOfType(t.Context(), events.DocumentApproved, 10); err != nil {
		t.Fatal(err)
	} else if len(history) != 1 {
		t.Errorf("approving twice wrote %d events, want 1", len(history))
	}
}

// TestTwoDistinctPeopleApprove is PLAN.md M11 acceptance 2's arithmetic: the
// UNIQUE constraint is what makes it a plain COUNT.
func TestTwoDistinctPeopleApprove(t *testing.T) {
	f := newDocFixture(t)
	doc, v := f.create(t, "Two Signatures")

	for _, user := range []int64{f.author.ID, f.other.ID} {
		if _, created, err := f.db.CreateApproval(t.Context(), NewApproval{
			DocumentID: doc.ID, VersionID: v.ID, State: "review",
			UserID: user, CreatedAt: f.now,
		}, f.event(user, events.DocumentApproved)); err != nil || !created {
			t.Fatalf("CreateApproval(%d): created = %t, %v", user, created, err)
		}
	}
	if n, err := f.db.CountApprovals(t.Context(), v.ID, "review"); err != nil || n != 2 {
		t.Errorf("CountApprovals = %d, %v, want 2", n, err)
	}
}

// TestWithdrawApprovalIsIdempotent covers the other half of "approve and
// withdraw": DELETE means "make sure mine is not there", and it is not an
// error when it already is not.
func TestWithdrawApprovalIsIdempotent(t *testing.T) {
	f := newDocFixture(t)
	doc, v := f.create(t, "Taken Back")

	if _, _, err := f.db.CreateApproval(t.Context(), NewApproval{
		DocumentID: doc.ID, VersionID: v.ID, State: "review",
		UserID: f.author.ID, CreatedAt: f.now,
	}, f.event(f.author.ID, events.DocumentApproved)); err != nil {
		t.Fatal(err)
	}

	removed, err := f.db.WithdrawApproval(t.Context(), doc.ID, v.ID, "review", f.author.ID,
		f.event(f.author.ID, events.DocumentApprovalWithdrawn))
	if err != nil || !removed {
		t.Fatalf("the first withdrawal reported removed = %t, %v", removed, err)
	}
	removed, err = f.db.WithdrawApproval(t.Context(), doc.ID, v.ID, "review", f.author.ID,
		f.event(f.author.ID, events.DocumentApprovalWithdrawn))
	if err != nil {
		t.Fatalf("withdrawing an approval that is not there failed: %v", err)
	}
	if removed {
		t.Error("the second withdrawal reported that it removed something")
	}
	if history, err := f.db.EventsOfType(t.Context(), events.DocumentApprovalWithdrawn, 10); err != nil {
		t.Fatal(err)
	} else if len(history) != 1 {
		t.Errorf("withdrawing twice wrote %d events, want 1", len(history))
	}

	// Somebody else's approval of the same version is untouched by a
	// withdrawal that names this user.
	if _, _, err := f.db.CreateApproval(t.Context(), NewApproval{
		DocumentID: doc.ID, VersionID: v.ID, State: "review",
		UserID: f.other.ID, CreatedAt: f.now,
	}, f.event(f.other.ID, events.DocumentApproved)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.WithdrawApproval(t.Context(), doc.ID, v.ID, "review", f.author.ID,
		f.event(f.author.ID, events.DocumentApprovalWithdrawn)); err != nil {
		t.Fatal(err)
	}
	if n, err := f.db.CountApprovals(t.Context(), v.ID, "review"); err != nil || n != 1 {
		t.Errorf("CountApprovals = %d, %v: a withdrawal took somebody else's approval with it", n, err)
	}
}

// TestApprovalsOfAnOldVersionSurvive is PLAN.md M11 acceptance 3 at the store
// level. The guard's count drops to zero with a new version because the guard
// counts one version's; the rows are history and history does not move.
func TestApprovalsOfAnOldVersionSurvive(t *testing.T) {
	f := newDocFixture(t)
	doc, v1 := f.create(t, "Then And Now")

	if _, _, err := f.db.CreateApproval(t.Context(), NewApproval{
		DocumentID: doc.ID, VersionID: v1.ID, State: "review",
		UserID: f.author.ID, CreatedAt: f.now,
	}, f.event(f.author.ID, events.DocumentApproved)); err != nil {
		t.Fatal(err)
	}
	// Check in and out again: the checkout is what opens version 2.
	if _, _, err := f.db.Checkout(t.Context(), doc.ID, f.author.ID, f.now, f.now.Add(time.Hour),
		f.event(f.author.ID, events.DocumentCheckedOut)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.db.Checkin(t.Context(), doc.ID, f.author.ID, f.now, "one",
		f.event(f.author.ID, events.DocumentCheckedIn)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.db.Checkout(t.Context(), doc.ID, f.author.ID, f.now, f.now.Add(time.Hour),
		f.event(f.author.ID, events.DocumentCheckedOut)); err != nil {
		t.Fatal(err)
	}

	after, err := f.db.DocumentByUID(t.Context(), doc.UID)
	if err != nil {
		t.Fatal(err)
	}
	if after.CurrentVersionID == v1.ID {
		t.Fatal("checking out did not open a new version, so this test proves nothing")
	}
	if n, err := f.db.CountApprovals(t.Context(), after.CurrentVersionID, "review"); err != nil || n != 0 {
		t.Errorf("the new version carries %d approvals, %v, want 0", n, err)
	}

	// The whole document's history, which is what makes the old version's
	// approvals queryable rather than merely still present.
	all, err := f.db.ApprovalsForDocument(t.Context(), doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].VersionID != v1.ID {
		t.Errorf("ApprovalsForDocument = %+v, want the one against version %d", all, v1.ID)
	}
}

// TestCommentThreads covers the shape the API renders and the number the guard
// refuses on: replies belong to a thread and are not open questions of their
// own.
func TestCommentThreads(t *testing.T) {
	f := newDocFixture(t)
	doc, ver := f.create(t, "Discussed At Length")

	root, err := f.db.CreateComment(t.Context(), NewComment{
		UID: ids.MustNew(f.now), DocumentID: doc.ID, VersionID: ver.ID,
		AuthorID: f.author.ID, Body: "the lede is buried", CreatedAt: f.now,
	}, f.event(f.author.ID, events.DocumentCommented))
	if err != nil {
		t.Fatalf("CreateComment: %v", err)
	}
	if root.VersionNumber != ver.Number {
		t.Errorf("the comment records version %d, want %d; the API speaks numbers, not keys",
			root.VersionNumber, ver.Number)
	}

	for _, body := range []string{"agreed", "moved it up"} {
		if _, err := f.db.CreateComment(t.Context(), NewComment{
			UID: ids.MustNew(f.now), DocumentID: doc.ID, VersionID: ver.ID,
			InReplyTo: root.ID, AuthorID: f.other.ID, Body: body, CreatedAt: f.now,
		}, f.event(f.other.ID, events.DocumentCommented)); err != nil {
			t.Fatalf("CreateComment(reply): %v", err)
		}
	}

	// Three rows, one open thread. Counting every unresolved row would report
	// four open questions and leave the guard refusing on rows nothing can
	// close.
	if n, err := f.db.CountUnresolvedComments(t.Context(), doc.ID); err != nil || n != 1 {
		t.Errorf("CountUnresolvedComments = %d, %v, want 1: a reply is not a thread", n, err)
	}

	got, err := f.db.CommentsForDocument(t.Context(), doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("CommentsForDocument returned %d comments, want 3", len(got))
	}
	threads := domain.Threads(got)
	if len(threads) != 1 || len(threads[0].Replies) != 2 {
		t.Fatalf("Threads produced %d threads with %d replies, want 1 and 2",
			len(threads), len(threads[0].Replies))
	}

	// Resolving is idempotent, and the second call does not overwrite the
	// first person's name.
	resolved, changed, err := f.db.ResolveComment(t.Context(), root.ID, f.other.ID, f.now,
		f.event(f.other.ID, events.DocumentCommentResolved))
	if err != nil || !changed {
		t.Fatalf("ResolveComment: changed = %t, %v", changed, err)
	}
	if resolved.ResolvedBy != f.other.ID {
		t.Errorf("resolved_by = %d, want %d", resolved.ResolvedBy, f.other.ID)
	}
	again, changed, err := f.db.ResolveComment(t.Context(), root.ID, f.author.ID, f.now,
		f.event(f.author.ID, events.DocumentCommentResolved))
	if err != nil {
		t.Fatalf("resolving a resolved thread failed: %v", err)
	}
	if changed {
		t.Error("resolving a resolved thread reported that it changed something")
	}
	if again.ResolvedBy != f.other.ID {
		t.Errorf("resolved_by = %d after a second resolution, want the first resolver %d",
			again.ResolvedBy, f.other.ID)
	}
	if n, err := f.db.CountUnresolvedComments(t.Context(), doc.ID); err != nil || n != 0 {
		t.Errorf("CountUnresolvedComments = %d, %v after resolving the thread, want 0", n, err)
	}
	if history, err := f.db.EventsOfType(t.Context(), events.DocumentCommentResolved, 10); err != nil {
		t.Fatal(err)
	} else if len(history) != 1 {
		t.Errorf("resolving twice wrote %d events, want 1", len(history))
	}
}

// TestCommentByUID is the lookup the resolution route makes: the API speaks
// uid and never an internal key (invariant 10).
func TestCommentByUID(t *testing.T) {
	f := newDocFixture(t)
	doc, _ := f.create(t, "Findable")

	uid := ids.MustNew(f.now)
	if _, err := f.db.CreateComment(t.Context(), NewComment{
		UID: uid, DocumentID: doc.ID, AuthorID: f.author.ID,
		Body: "no version on this one", CreatedAt: f.now,
	}, f.event(f.author.ID, events.DocumentCommented)); err != nil {
		t.Fatal(err)
	}

	got, err := f.db.CommentByUID(t.Context(), uid)
	if err != nil {
		t.Fatalf("CommentByUID: %v", err)
	}
	if got.UID != uid || got.DocumentID != doc.ID {
		t.Errorf("CommentByUID = %+v, want the comment on document %d", got, doc.ID)
	}
	// A comment about the document as a whole has no version, and the LEFT
	// JOIN that fetches the number must not lose the row.
	if got.VersionID != 0 || got.VersionNumber != 0 {
		t.Errorf("a comment with no version reports version %d (%d)", got.VersionNumber, got.VersionID)
	}

	if _, err := f.db.CommentByUID(t.Context(), "nosuchcomment"); !isNotFound(err) {
		t.Errorf("CommentByUID of a uid that is not there = %v, want not found", err)
	}
}
