// Copyright (c) 2026 Michael D Henderson.

package service

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/events"
	"github.com/mdhender/bricolage/internal/ids"
	"github.com/mdhender/bricolage/internal/store"
)

// The service half of M3. These tests assert on emitted events as well as on
// returned values: an operation that does not write its event is not finished
// (invariant 7, PLAN.md M3 acceptance 6).

// elementType gives the harness the element type every document needs.
func (h *harness) elementType(t *testing.T, keyName, kind string) domain.ElementType {
	t.Helper()
	et, err := h.db.CreateElementType(t.Context(), store.NewElementType{
		UID: ids.MustNew(start), KeyName: keyName, Name: keyName, Kind: kind,
		TopLevel: true, Schema: "{}", CreatedAt: start,
	})
	if err != nil {
		t.Fatalf("CreateElementType: %v", err)
	}
	return et
}

// newDocHarness is a service with one element type and one editor holding
// Create over everything, which is the shape "cmsdb seed" produces.
func newDocHarness(t *testing.T) (*harness, domain.Identity) {
	t.Helper()
	h := newHarness(t)
	h.elementType(t, "story", domain.KindStory)
	editor := h.admin(t, "editor@example.com", "correct horse battery", domain.Create)
	return h, editor
}

func (h *harness) newDoc(t *testing.T, actor domain.Identity, title string) DocumentView {
	t.Helper()
	view, err := h.CreateDocument(t.Context(), actor, domain.NewDocument{
		SiteID:         h.siteID,
		Kind:           domain.KindStory,
		ElementTypeKey: "story",
		Title:          title,
		Content:        `{"body":"The quick brown fox."}`,
	})
	if err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}
	return view
}

// docEvents returns a document's history, newest first.
func (h *harness) docEvents(t *testing.T, documentID int64) []domain.Event {
	t.Helper()
	got, err := h.db.EventsForSubject(t.Context(), domain.SubjectDocument, documentID, 100)
	if err != nil {
		t.Fatalf("EventsForSubject: %v", err)
	}
	return got
}

// TestDocumentCycleWritesOneEventEach is PLAN.md M3 acceptance 1 and 6: the
// full cycle, and exactly one event of the expected type for each step, with
// the actor and a payload.
func TestDocumentCycleWritesOneEventEach(t *testing.T) {
	h, editor := newDocHarness(t)

	view := h.newDoc(t, editor, "The Quick Brown Fox")
	id := view.Document.ID

	if _, err := h.Checkout(t.Context(), editor, view.Document.UID); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	title := "The Slow Brown Fox"
	if _, err := h.UpdateDraft(t.Context(), editor, view.Document.UID,
		domain.DraftUpdate{Title: &title}); err != nil {
		t.Fatalf("UpdateDraft: %v", err)
	}
	after, err := h.Checkin(t.Context(), editor, view.Document.UID, "first pass")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}

	// Acceptance 1: version 1, with checked_in_at set and no draft left.
	if after.Version.Number != 1 {
		t.Errorf("checked in version %d, want 1", after.Version.Number)
	}
	if after.Version.CheckedInAt.IsZero() {
		t.Error("checked_in_at is not set")
	}
	if after.Version.Title != title {
		t.Errorf("title = %q, want the edit to have survived the check-in", after.Version.Title)
	}
	if _, err := h.db.DraftForDocument(t.Context(), id); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("a working draft survived the check-in: %v", err)
	}

	// Acceptance 6: one event per step, in order, each naming the actor.
	want := []string{
		events.DocumentCheckedIn,
		events.DocumentDraftUpdated,
		events.DocumentCheckedOut,
		events.DocumentCreated,
	}
	got := h.docEvents(t, id)
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d: %+v", len(got), len(want), got)
	}
	for i, kind := range want {
		if got[i].Type != kind {
			t.Errorf("event %d is %q, want %q", i, got[i].Type, kind)
		}
		if got[i].ActorID != editor.User.ID {
			t.Errorf("event %q has actor %d, want %d", got[i].Type, got[i].ActorID, editor.User.ID)
		}
		if len(got[i].Payload) == 0 {
			t.Errorf("event %q has no payload; it must carry enough to reconstruct what happened", got[i].Type)
		}
	}

	// The payload of an edit names the fields and never their contents: a
	// document body is not something this system logs.
	for _, e := range got {
		if e.Type != events.DocumentDraftUpdated {
			continue
		}
		fields, ok := e.Payload["fields"].([]any)
		if !ok || len(fields) != 1 || fields[0] != "title" {
			t.Errorf("the edit event names %v, want [title]", e.Payload["fields"])
		}
		if _, present := e.Payload["content"]; present {
			t.Error("the edit event carries the content")
		}
	}
}

// TestCheckoutConflictWritesNothing is acceptance 3 and 6 together: a refusal
// changes nothing, events included, because the check is inside the
// transaction the event would be written in.
func TestCheckoutConflictWritesNothing(t *testing.T) {
	h, editor := newDocHarness(t)
	other := h.admin(t, "other@example.com", "correct horse battery", domain.Create)
	view := h.newDoc(t, editor, "Contended")

	if _, err := h.Checkout(t.Context(), editor, view.Document.UID); err != nil {
		t.Fatalf("the first Checkout: %v", err)
	}
	before := len(h.docEvents(t, view.Document.ID))

	_, err := h.Checkout(t.Context(), other, view.Document.UID)
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("the second Checkout = %v, want a conflict", err)
	}
	if after := len(h.docEvents(t, view.Document.ID)); after != before {
		t.Errorf("%d events became %d; a refusal writes none", before, after)
	}
}

// TestExpiredLeaseDoesNotBlock is acceptance 4, driven through the fake clock:
// past the lease, the document is available again with no administrator.
func TestExpiredLeaseDoesNotBlock(t *testing.T) {
	h, editor := newDocHarness(t)
	other := h.admin(t, "other@example.com", "correct horse battery", domain.Create)
	view := h.newDoc(t, editor, "Leased")

	if _, err := h.Checkout(t.Context(), editor, view.Document.UID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Checkout(t.Context(), other, view.Document.UID); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("checking out a leased document = %v, want a conflict", err)
	}

	h.clock.Advance(h.LockLease() + time.Second)
	got, err := h.Checkout(t.Context(), other, view.Document.UID)
	if err != nil {
		t.Fatalf("checking out after the lease ran out: %v", err)
	}
	if !got.Document.Lock.IsHeldBy(other.User.ID, h.Now()) {
		t.Errorf("the lease is %+v, want it held by the second caller", got.Document.Lock)
	}
}

// TestRevert is acceptance 5, both halves, with the event each writes.
func TestRevert(t *testing.T) {
	t.Run("a document with no checked-in version is deleted", func(t *testing.T) {
		h, editor := newDocHarness(t)
		view := h.newDoc(t, editor, "Never Saved")

		out, err := h.Revert(t.Context(), editor, view.Document.UID)
		if err != nil {
			t.Fatalf("Revert: %v", err)
		}
		if !out.Deleted {
			t.Fatal("Deleted = false")
		}
		if _, err := h.Document(t.Context(), editor, view.Document.UID); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("the document is still readable: %v", err)
		}

		got := h.docEvents(t, view.Document.ID)
		if len(got) != 2 || got[0].Type != events.DocumentReverted {
			t.Fatalf("events = %+v, want a creation and a revert", got)
		}
		if deleted, _ := got[0].Payload["deleted"].(bool); !deleted {
			t.Errorf("payload = %v, want it to record the deletion", got[0].Payload)
		}
	})

	t.Run("the latest checked-in version is left intact", func(t *testing.T) {
		h, editor := newDocHarness(t)
		view := h.newDoc(t, editor, "Saved Once")
		uid := view.Document.UID

		if _, err := h.Checkout(t.Context(), editor, uid); err != nil {
			t.Fatal(err)
		}
		first, err := h.Checkin(t.Context(), editor, uid, "kept")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.Checkout(t.Context(), editor, uid); err != nil {
			t.Fatal(err)
		}
		discarded := "Discarded"
		if _, err := h.UpdateDraft(t.Context(), editor, uid, domain.DraftUpdate{Title: &discarded}); err != nil {
			t.Fatal(err)
		}

		out, err := h.Revert(t.Context(), editor, uid)
		if err != nil {
			t.Fatalf("Revert: %v", err)
		}
		if out.Deleted {
			t.Fatal("Deleted = true; the document has a checked-in version")
		}
		if out.View.Version.ID != first.Version.ID {
			t.Errorf("current version = %d, want the checked-in %d", out.View.Version.ID, first.Version.ID)
		}
		if out.View.Version.Title != "Saved Once" {
			t.Errorf("title = %q, want the checked-in version untouched", out.View.Version.Title)
		}

		_, versions, err := h.Versions(t.Context(), editor, uid)
		if err != nil {
			t.Fatal(err)
		}
		if len(versions) != 1 {
			t.Errorf("got %d versions, want the checked-in one alone", len(versions))
		}
	})
}

// TestCancelCheckoutKeepsTheWork is the distinction the two commands exist to
// make: cancelling releases the lease, reverting throws the work away.
func TestCancelCheckoutKeepsTheWork(t *testing.T) {
	h, editor := newDocHarness(t)
	view := h.newDoc(t, editor, "In Progress")
	uid := view.Document.UID

	if _, err := h.Checkout(t.Context(), editor, uid); err != nil {
		t.Fatal(err)
	}
	title := "Half Written"
	if _, err := h.UpdateDraft(t.Context(), editor, uid, domain.DraftUpdate{Title: &title}); err != nil {
		t.Fatal(err)
	}
	out, err := h.CancelCheckout(t.Context(), editor, uid)
	if err != nil {
		t.Fatalf("CancelCheckout: %v", err)
	}
	if out.Document.Lock.Held(h.Now()) {
		t.Error("the lease is still held")
	}
	if out.Version.Title != title {
		t.Errorf("title = %q, want the work kept", out.Version.Title)
	}

	got := h.docEvents(t, view.Document.ID)
	if got[0].Type != events.DocumentCheckoutCanceled {
		t.Errorf("the newest event is %q", got[0].Type)
	}
}

// TestDocumentAuthorization is DESIGN.md 7.2 applied to documents: reading
// needs Read, editing needs Edit, creating needs Create, and a caller who may
// not read a document is told it is not there rather than that it exists.
func TestDocumentAuthorization(t *testing.T) {
	h, editor := newDocHarness(t)
	view := h.newDoc(t, editor, "Restricted")
	uid := view.Document.UID

	reader := h.admin(t, "reader@example.com", "correct horse battery", domain.Read)
	stranger := h.admin(t, "stranger@example.com", "correct horse battery", domain.NoPrivilege)

	t.Run("a reader may read and may not edit", func(t *testing.T) {
		if _, err := h.Document(t.Context(), reader, uid); err != nil {
			t.Errorf("Document: %v", err)
		}
		if _, err := h.Checkout(t.Context(), reader, uid); !errors.Is(err, domain.ErrForbidden) {
			t.Errorf("Checkout as a reader = %v, want ErrForbidden", err)
		}
	})

	t.Run("a stranger is told it is not there", func(t *testing.T) {
		if _, err := h.Document(t.Context(), stranger, uid); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("Document as a stranger = %v, want ErrNotFound", err)
		}
		if _, err := h.Checkout(t.Context(), stranger, uid); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("Checkout as a stranger = %v, want ErrNotFound", err)
		}
	})

	t.Run("creating needs Create", func(t *testing.T) {
		_, err := h.CreateDocument(t.Context(), reader, domain.NewDocument{
			SiteID: h.siteID, Kind: domain.KindStory, ElementTypeKey: "story", Title: "Nope",
		})
		if !errors.Is(err, domain.ErrForbidden) {
			t.Errorf("CreateDocument as a reader = %v, want ErrForbidden", err)
		}
	})

	t.Run("a list shows only what the caller may read", func(t *testing.T) {
		visible, err := h.ListDocuments(t.Context(), reader, domain.DocumentFilter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(visible) != 1 {
			t.Errorf("a reader sees %d documents, want 1", len(visible))
		}
		hidden, err := h.ListDocuments(t.Context(), stranger, domain.DocumentFilter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(hidden) != 0 {
			t.Errorf("a stranger sees %d documents, want none", len(hidden))
		}
	})
}

// TestCreateDocumentRefusesAMismatchedElementType keeps a document's fields
// renderable: an element type declares which kind it applies to.
func TestCreateDocumentRefusesAMismatchedElementType(t *testing.T) {
	h, editor := newDocHarness(t)
	h.elementType(t, "page-template", domain.KindTemplate)

	_, err := h.CreateDocument(t.Context(), editor, domain.NewDocument{
		SiteID: h.siteID, Kind: domain.KindStory, ElementTypeKey: "page-template", Title: "Wrong Type",
	})
	if !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("CreateDocument with a mismatched element type = %v, want ErrInvalid", err)
	}
}

// TestDiff is acceptance 7 at the service level: two versions, compared.
func TestDiff(t *testing.T) {
	h, editor := newDocHarness(t)
	view := h.newDoc(t, editor, "The Quick Brown Fox")
	uid := view.Document.UID

	if _, err := h.Checkout(t.Context(), editor, uid); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Checkin(t.Context(), editor, uid, "v1"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Checkout(t.Context(), editor, uid); err != nil {
		t.Fatal(err)
	}
	title := "The Slow Brown Fox"
	content := `{"body":"The slow brown fox."}`
	if _, err := h.UpdateDraft(t.Context(), editor, uid,
		domain.DraftUpdate{Title: &title, Content: &content}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Checkin(t.Context(), editor, uid, "v2"); err != nil {
		t.Fatal(err)
	}

	d, err := h.Diff(t.Context(), editor, uid, 1, 2)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	text := d.Text()
	for _, want := range []string{"--- version 1", "+++ version 2", "@@ title @@", "[-Quick-]{+Slow+}"} {
		if !strings.Contains(text, want) {
			t.Errorf("the diff does not contain %q:\n%s", want, text)
		}
	}

	// An unknown version is a 404, not an empty diff.
	if _, err := h.Diff(t.Context(), editor, uid, 1, 9); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Diff against a version that does not exist = %v, want ErrNotFound", err)
	}
	if _, err := h.Diff(t.Context(), editor, uid, 0, 1); !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("Diff from version 0 = %v, want ErrInvalid; there is no version 0", err)
	}
}

// TestDocumentEventsAreReadableByAReader is DESIGN.md 10: the history of a
// document is part of the document, so anybody who may read one may read it.
func TestDocumentEventsAreReadableByAReader(t *testing.T) {
	h, editor := newDocHarness(t)
	reader := h.admin(t, "reader@example.com", "correct horse battery", domain.Read)
	view := h.newDoc(t, editor, "Audited")

	got, err := h.DocumentEvents(t.Context(), reader, view.Document.UID, 10)
	if err != nil {
		t.Fatalf("DocumentEvents: %v", err)
	}
	if len(got) != 1 || got[0].Type != events.DocumentCreated {
		t.Errorf("events = %+v, want the creation", got)
	}
}
