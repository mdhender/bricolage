// Copyright (c) 2026 Michael D Henderson.

package store

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/events"
	"github.com/mdhender/bricolage/internal/ids"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// The store half of M3, against a real in-memory database with every migration
// applied through the same code path cmsdb uses and with foreign keys on
// (DESIGN.md 15, invariant 22).

// docFixture is a database with one site, one element type, one user, and a
// clock, which is the least a document needs to exist at all.
type docFixture struct {
	db     *DB
	now    time.Time
	siteID int64
	etID   int64
	author domain.User
	other  domain.User
}

func newDocFixture(t *testing.T) *docFixture {
	t.Helper()
	db, err := OpenMemory(t.Context())
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	now := fixedClock().Now()
	f := &docFixture{db: db, now: now}

	if f.siteID, err = db.CreateSite(t.Context(), NewSite{
		UID: ids.MustNew(now), Name: "Default", Domain: "example.com",
	}); err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	et, err := db.CreateElementType(t.Context(), NewElementType{
		UID: ids.MustNew(now), KeyName: "story", Name: "Story",
		Kind: domain.KindStory, TopLevel: true, Schema: `{"fields":[]}`, CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateElementType: %v", err)
	}
	f.etID = et.ID
	f.author = makeUser(t, db, "author@example.com")
	f.other = makeUser(t, db, "other@example.com")
	return f
}

// create makes a document with an open working draft, which is what
// CreateDocument produces.
func (f *docFixture) create(t *testing.T, title string) (domain.Document, domain.Version) {
	t.Helper()
	d, v, err := f.db.CreateDocument(t.Context(), NewDocument{
		UID:           ids.MustNew(f.now),
		SiteID:        f.siteID,
		Kind:          domain.KindStory,
		ElementTypeID: f.etID,
		Title:         title,
		Content:       `{"body":"first"}`,
		CreatedBy:     f.author.ID,
		CreatedAt:     f.now,
		Event: domain.Event{
			Type: events.DocumentCreated, ActorID: f.author.ID, OccurredAt: f.now,
		},
	})
	if err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}
	return d, v
}

func (f *docFixture) event(actor int64, kind string) domain.Event {
	return domain.Event{Type: kind, ActorID: actor, OccurredAt: f.now}
}

// TestDocumentLifecycle is PLAN.md M3 acceptance 1 at the store level: create,
// check out, edit, check in produces version 1 with checked_in_at set and no
// open working draft left behind.
func TestDocumentLifecycle(t *testing.T) {
	f := newDocFixture(t)
	doc, draft := f.create(t, "First Draft")

	if draft.Number != 1 {
		t.Errorf("the first version is numbered %d, want 1 (there is no version 0)", draft.Number)
	}
	if !draft.IsDraft() {
		t.Error("the version a new document starts with is not the open working draft")
	}
	if doc.CurrentVersionID != draft.ID {
		t.Errorf("current_version_id = %d, want the draft %d", doc.CurrentVersionID, draft.ID)
	}
	if doc.Lock.Held(f.now) {
		t.Error("a new document is checked out; creating is not checking out")
	}

	expires := f.now.Add(time.Hour)
	doc, draft, err := f.db.Checkout(t.Context(), doc.ID, f.author.ID, f.now, expires,
		f.event(f.author.ID, events.DocumentCheckedOut))
	if err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if !doc.Lock.IsHeldBy(f.author.ID, f.now) {
		t.Errorf("the lease is %+v, want it held by the author", doc.Lock)
	}
	if draft.Number != 1 {
		t.Errorf("checking out the document opened version %d; the draft was already open", draft.Number)
	}

	edited := draft
	edited.Title = "First Draft, Edited"
	edited.Content = `{"body":"second"}`
	_, draft, err = f.db.UpdateDraft(t.Context(), doc.ID, f.author.ID, f.now, expires, edited,
		f.event(f.author.ID, events.DocumentDraftUpdated))
	if err != nil {
		t.Fatalf("UpdateDraft: %v", err)
	}
	if draft.Title != "First Draft, Edited" {
		t.Errorf("title = %q", draft.Title)
	}

	checkedIn := f.now.Add(time.Minute)
	doc, ver, err := f.db.Checkin(t.Context(), doc.ID, f.author.ID, checkedIn, "first pass",
		f.event(f.author.ID, events.DocumentCheckedIn))
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if ver.Number != 1 {
		t.Errorf("checked in version %d, want 1", ver.Number)
	}
	if ver.CheckedInAt.IsZero() {
		t.Error("checked_in_at is not set on the version that was checked in")
	}
	if ver.Note != "first pass" {
		t.Errorf("note = %q", ver.Note)
	}
	if doc.Lock.Held(checkedIn) {
		t.Error("checking in left the lease held")
	}
	if doc.CurrentVersionID != ver.ID {
		t.Errorf("current_version_id = %d, want %d", doc.CurrentVersionID, ver.ID)
	}

	// The working draft row is gone: it became version 1.
	if _, err := f.db.DraftForDocument(t.Context(), doc.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("DraftForDocument after check-in = %v, want ErrNotFound", err)
	}

	// A second checkout opens version 2, copying version 1.
	_, second, err := f.db.Checkout(t.Context(), doc.ID, f.author.ID, checkedIn, checkedIn.Add(time.Hour),
		f.event(f.author.ID, events.DocumentCheckedOut))
	if err != nil {
		t.Fatalf("the second Checkout: %v", err)
	}
	if second.Number != 2 {
		t.Errorf("the second checkout opened version %d, want 2", second.Number)
	}
	if second.Content != `{"body":"second"}` {
		t.Errorf("version 2 content = %q, want a copy of version 1", second.Content)
	}
	if second.Note != "" {
		t.Errorf("version 2 carried version 1's check-in note %q", second.Note)
	}
}

// TestCheckedInVersionsAreImmutable is PLAN.md M3 acceptance 2: the trigger
// refuses an UPDATE, at the store level, with its own message.
//
// It is asserted against raw SQL rather than through a store method because
// the point is that the database refuses it. A guard that only this package's
// methods respect is a guard a future method forgets.
func TestCheckedInVersionsAreImmutable(t *testing.T) {
	f := newDocFixture(t)
	doc, _ := f.create(t, "Immutable")
	if _, _, err := f.db.Checkout(t.Context(), doc.ID, f.author.ID, f.now, f.now.Add(time.Hour),
		f.event(f.author.ID, events.DocumentCheckedOut)); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	_, ver, err := f.db.Checkin(t.Context(), doc.ID, f.author.ID, f.now, "",
		f.event(f.author.ID, events.DocumentCheckedIn))
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}

	err = f.db.Write(t.Context(), func(conn *sqlite.Conn) error {
		return sqlitex.ExecuteTransient(conn,
			`UPDATE document_versions SET title = 'rewritten' WHERE id = ?`,
			&sqlitex.ExecOptions{Args: []any{ver.ID}})
	})
	if err == nil {
		t.Fatal("a checked-in version was updated; the trigger did not fire")
	}
	if !strings.Contains(err.Error(), "checked-in versions are immutable") {
		t.Errorf("the trigger's message is missing from %v", err)
	}

	// And the row is unchanged.
	got, err := f.db.VersionByNumber(t.Context(), doc.ID, ver.Number)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Immutable" {
		t.Errorf("title = %q, want it unchanged", got.Title)
	}
}

// TestConcurrentCheckout is PLAN.md M3 acceptance 3: exactly one of two
// concurrent checkouts succeeds and the other is a conflict.
//
// It runs under -race in CI. The guarantee is not a mutex in this process --
// it is that the check is a condition on the UPDATE, so one statement finds
// the row unlocked and the other does not.
func TestConcurrentCheckout(t *testing.T) {
	f := newDocFixture(t)
	doc, _ := f.create(t, "Contended")

	users := []int64{f.author.ID, f.other.ID}
	results := make([]error, len(users))
	var wg sync.WaitGroup
	var ready sync.WaitGroup
	ready.Add(len(users))
	start := make(chan struct{})

	for i, user := range users {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ready.Done()
			<-start
			_, _, err := f.db.Checkout(t.Context(), doc.ID, user, f.now, f.now.Add(time.Hour),
				f.event(user, events.DocumentCheckedOut))
			results[i] = err
		}()
	}
	ready.Wait()
	close(start)
	wg.Wait()

	succeeded, conflicts := 0, 0
	for i, err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, domain.ErrConflict):
			conflicts++
		default:
			t.Errorf("checkout %d returned %v, want nil or a conflict", i, err)
		}
	}
	if succeeded != 1 || conflicts != 1 {
		t.Errorf("%d succeeded and %d conflicted, want exactly one of each: %v", succeeded, conflicts, results)
	}
}

// TestExpiredLockDoesNotBlockCheckout is PLAN.md M3 acceptance 4. The lock is
// a lease: an editor who closed their laptop does not hold a document until an
// administrator intervenes, which is what it took in the system we learned
// from.
func TestExpiredLockDoesNotBlockCheckout(t *testing.T) {
	f := newDocFixture(t)
	doc, _ := f.create(t, "Leased")

	expires := f.now.Add(time.Hour)
	if _, _, err := f.db.Checkout(t.Context(), doc.ID, f.other.ID, f.now, expires,
		f.event(f.other.ID, events.DocumentCheckedOut)); err != nil {
		t.Fatalf("the first Checkout: %v", err)
	}

	// While the lease is live, nobody else gets it.
	if _, _, err := f.db.Checkout(t.Context(), doc.ID, f.author.ID, f.now, expires,
		f.event(f.author.ID, events.DocumentCheckedOut)); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("checking out a leased document = %v, want a conflict", err)
	}

	// Once it has run out, they do.
	later := expires.Add(time.Second)
	doc, _, err := f.db.Checkout(t.Context(), doc.ID, f.author.ID, later, later.Add(time.Hour),
		f.event(f.author.ID, events.DocumentCheckedOut))
	if err != nil {
		t.Fatalf("checking out after the lease expired: %v", err)
	}
	if !doc.Lock.IsHeldBy(f.author.ID, later) {
		t.Errorf("the lease is %+v, want it held by the second caller", doc.Lock)
	}
}

// TestRevert is PLAN.md M3 acceptance 5, both halves.
func TestRevert(t *testing.T) {
	t.Run("with no checked-in version the document is deleted", func(t *testing.T) {
		f := newDocFixture(t)
		doc, _ := f.create(t, "Never Saved")

		out, err := f.db.Revert(t.Context(), doc.ID, f.author.ID, f.now,
			f.event(f.author.ID, events.DocumentReverted))
		if err != nil {
			t.Fatalf("Revert: %v", err)
		}
		if !out.Deleted {
			t.Fatal("Deleted = false; a document whose draft was its only version is deleted")
		}
		if _, err := f.db.DocumentByUID(t.Context(), doc.UID); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("the document is still there: %v", err)
		}

		// The account of what happened survives the row it was about.
		got, err := f.db.EventsForSubject(t.Context(), domain.SubjectDocument, doc.ID, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Fatalf("got %d events, want the creation and the revert", len(got))
		}
		if got[0].Type != events.DocumentReverted {
			t.Errorf("the newest event is %q", got[0].Type)
		}
		if deleted, _ := got[0].Payload["deleted"].(bool); !deleted {
			t.Errorf("the revert event payload is %v, want it to say the document was deleted", got[0].Payload)
		}
	})

	t.Run("with a checked-in version the draft is discarded", func(t *testing.T) {
		f := newDocFixture(t)
		doc, _ := f.create(t, "Saved Once")
		if _, _, err := f.db.Checkout(t.Context(), doc.ID, f.author.ID, f.now, f.now.Add(time.Hour),
			f.event(f.author.ID, events.DocumentCheckedOut)); err != nil {
			t.Fatal(err)
		}
		_, first, err := f.db.Checkin(t.Context(), doc.ID, f.author.ID, f.now, "kept",
			f.event(f.author.ID, events.DocumentCheckedIn))
		if err != nil {
			t.Fatal(err)
		}

		// A second checkout opens version 2, which is then thrown away.
		if _, _, err := f.db.Checkout(t.Context(), doc.ID, f.author.ID, f.now, f.now.Add(time.Hour),
			f.event(f.author.ID, events.DocumentCheckedOut)); err != nil {
			t.Fatal(err)
		}
		out, err := f.db.Revert(t.Context(), doc.ID, f.author.ID, f.now,
			f.event(f.author.ID, events.DocumentReverted))
		if err != nil {
			t.Fatalf("Revert: %v", err)
		}
		if out.Deleted {
			t.Fatal("Deleted = true; the document has a checked-in version")
		}
		if out.Version.ID != first.ID {
			t.Errorf("the current version is %d, want the checked-in %d", out.Version.ID, first.ID)
		}
		if out.Document.CurrentVersionID != first.ID {
			t.Errorf("current_version_id = %d, want %d", out.Document.CurrentVersionID, first.ID)
		}
		if out.Document.Lock.Held(f.now) {
			t.Error("reverting left the lease held")
		}

		versions, err := f.db.VersionsForDocument(t.Context(), doc.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(versions) != 1 || versions[0].Number != 1 {
			t.Errorf("versions = %+v, want version 1 alone", versions)
		}
	})
}

// TestOneOpenDraftPerDocument is the partial unique index in
// 0004_documents.sql. Everything above it assumes there is exactly one draft to
// find; a second would make each of those queries return an arbitrary row.
func TestOneOpenDraftPerDocument(t *testing.T) {
	f := newDocFixture(t)
	doc, _ := f.create(t, "One Draft")

	err := f.db.Write(t.Context(), func(conn *sqlite.Conn) error {
		_, err := insertVersion(conn, domain.Version{
			DocumentID: doc.ID, Number: 99, Title: "Second draft",
			CreatedBy: f.author.ID, CreatedAt: f.now,
		})
		return err
	})
	ce, ok := AsConstraint(err)
	if !ok {
		t.Fatalf("a second open draft was accepted: %v", err)
	}
	if !ce.IsUnique() {
		t.Errorf("the violated constraint is %v, want the unique index", ce.Code)
	}
	if !errors.Is(err, domain.ErrConflict) {
		t.Errorf("the error does not answer to ErrConflict: %v", err)
	}
}

// TestEditingRequiresTheLease is what makes the lock worth having: a caller
// who does not hold it may not write, and neither may one whose lease ran out.
func TestEditingRequiresTheLease(t *testing.T) {
	f := newDocFixture(t)
	doc, draft := f.create(t, "Leased Edits")
	expires := f.now.Add(time.Hour)

	// Nobody holds it.
	if _, _, err := f.db.UpdateDraft(t.Context(), doc.ID, f.author.ID, f.now, expires, draft,
		f.event(f.author.ID, events.DocumentDraftUpdated)); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("editing an unlocked document = %v, want a conflict", err)
	}

	if _, _, err := f.db.Checkout(t.Context(), doc.ID, f.author.ID, f.now, expires,
		f.event(f.author.ID, events.DocumentCheckedOut)); err != nil {
		t.Fatal(err)
	}

	// Somebody else holds it.
	if _, _, err := f.db.UpdateDraft(t.Context(), doc.ID, f.other.ID, f.now, expires, draft,
		f.event(f.other.ID, events.DocumentDraftUpdated)); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("editing somebody else's checkout = %v, want a conflict", err)
	}
	if _, _, err := f.db.Checkin(t.Context(), doc.ID, f.other.ID, f.now, "",
		f.event(f.other.ID, events.DocumentCheckedIn)); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("checking in somebody else's checkout = %v, want a conflict", err)
	}

	// The holder's own lease has run out.
	after := expires.Add(time.Second)
	if _, _, err := f.db.UpdateDraft(t.Context(), doc.ID, f.author.ID, after, after.Add(time.Hour), draft,
		f.event(f.author.ID, events.DocumentDraftUpdated)); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("editing on an expired lease = %v, want a conflict", err)
	}
}

// TestCancelCheckoutKeepsTheDraft is the difference between cancelling and
// reverting: one releases the lease, the other throws the work away.
func TestCancelCheckoutKeepsTheDraft(t *testing.T) {
	f := newDocFixture(t)
	doc, draft := f.create(t, "Cancelled")
	expires := f.now.Add(time.Hour)

	if _, _, err := f.db.Checkout(t.Context(), doc.ID, f.author.ID, f.now, expires,
		f.event(f.author.ID, events.DocumentCheckedOut)); err != nil {
		t.Fatal(err)
	}
	draft.Content = `{"body":"work in progress"}`
	if _, _, err := f.db.UpdateDraft(t.Context(), doc.ID, f.author.ID, f.now, expires, draft,
		f.event(f.author.ID, events.DocumentDraftUpdated)); err != nil {
		t.Fatal(err)
	}

	doc, err := f.db.CancelCheckout(t.Context(), doc.ID, f.author.ID, f.now,
		f.event(f.author.ID, events.DocumentCheckoutCanceled))
	if err != nil {
		t.Fatalf("CancelCheckout: %v", err)
	}
	if doc.Lock.Held(f.now) {
		t.Error("the lease is still held")
	}

	kept, err := f.db.DraftForDocument(t.Context(), doc.ID)
	if err != nil {
		t.Fatalf("the draft did not survive the cancellation: %v", err)
	}
	if kept.Content != `{"body":"work in progress"}` {
		t.Errorf("content = %q, want the work in progress", kept.Content)
	}
}

// TestDocumentEventsAreOneTransaction is invariant 7: the change and the event
// commit together. A failing operation leaves neither behind.
func TestDocumentEventsAreOneTransaction(t *testing.T) {
	f := newDocFixture(t)
	doc, _ := f.create(t, "Atomic")

	before, err := f.db.EventsForSubject(t.Context(), domain.SubjectDocument, doc.ID, 10)
	if err != nil {
		t.Fatal(err)
	}

	// A checkout that is refused writes no event, because the refusal happens
	// inside the transaction the event would have been written in.
	if _, _, err := f.db.Checkout(t.Context(), doc.ID, f.author.ID, f.now, f.now.Add(time.Hour),
		f.event(f.author.ID, events.DocumentCheckedOut)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.db.Checkout(t.Context(), doc.ID, f.other.ID, f.now, f.now.Add(time.Hour),
		f.event(f.other.ID, events.DocumentCheckedOut)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("the second checkout = %v, want a conflict", err)
	}

	after, err := f.db.EventsForSubject(t.Context(), domain.SubjectDocument, doc.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before)+1 {
		t.Errorf("%d events became %d; the refused checkout wrote one", len(before), len(after))
	}
}

// TestElementTypes covers the read paths the API and "cmsdb seed" use.
func TestElementTypes(t *testing.T) {
	f := newDocFixture(t)

	et, err := f.db.ElementTypeByKeyName(t.Context(), "story")
	if err != nil {
		t.Fatalf("ElementTypeByKeyName: %v", err)
	}
	if et.Name != "Story" || et.Kind != domain.KindStory || !et.TopLevel {
		t.Errorf("element type = %+v", et)
	}
	if et.Schema != `{"fields":[]}` {
		t.Errorf("schema = %q, want it stored verbatim", et.Schema)
	}

	if _, err := f.db.ElementTypeByKeyName(t.Context(), "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("an unknown key name = %v, want ErrNotFound", err)
	}

	// A duplicate key name is a conflict, which is what makes "cmsdb seed"
	// idempotent without a read-then-write race.
	_, err = f.db.CreateElementType(t.Context(), NewElementType{
		UID: ids.MustNew(f.now), KeyName: "story", Name: "Story Again",
		Kind: domain.KindStory, Schema: "{}", CreatedAt: f.now,
	})
	if !errors.Is(err, domain.ErrConflict) {
		t.Errorf("a duplicate key name = %v, want a conflict", err)
	}

	all, err := f.db.ListElementTypes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Errorf("got %d element types, want 1", len(all))
	}
}

// TestGrantScopeCarriesDocument is the scope column 0004 added, and the
// promise 0003 made about it: the resolver has carried this dimension since
// M2, and now the schema does too.
func TestGrantScopeCarriesDocument(t *testing.T) {
	f := newDocFixture(t)
	doc, _ := f.create(t, "Scoped")

	role, err := f.db.CreateRole(t.Context(), "one-document", "One document")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.CreateGrant(t.Context(), domain.Grant{
		RoleID:    role.ID,
		Privilege: domain.Edit,
		Scope:     domain.Scope{DocumentID: &doc.ID},
		CreatedAt: f.now,
	}); err != nil {
		t.Fatalf("CreateGrant: %v", err)
	}

	got, err := f.db.GrantsForRole(t.Context(), role.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d grants, want 1", len(got))
	}
	if got[0].Scope.DocumentID == nil || *got[0].Scope.DocumentID != doc.ID {
		t.Errorf("scope = %s, want document=%d", got[0].Scope, doc.ID)
	}

	// The column is a real foreign key, which is the whole reason it waited
	// for the migration that created its table.
	_, err = f.db.CreateGrant(t.Context(), domain.Grant{
		RoleID:    role.ID,
		Privilege: domain.Edit,
		Scope:     domain.Scope{DocumentID: domain.Ref(int64(999999))},
		CreatedAt: f.now,
	})
	ce, ok := AsConstraint(err)
	if !ok || !ce.IsForeignKey() {
		t.Errorf("a grant naming a document that does not exist = %v, want a foreign key violation", err)
	}
}
