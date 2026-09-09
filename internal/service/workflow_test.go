// Copyright (c) 2026 Michael D Henderson.

package service

import (
	"errors"
	"testing"

	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/events"
)

// The service half of M4. The rules are tested in internal/workflow, where the
// engine lives; what is tested here is what this layer owns -- that a caller
// who may not see a document is told it is not there, that the document a new
// one becomes is placed in a workflow, and that the event lands.

// TestNewDocumentsEnterTheDefaultWorkflow is the placement M4 adds to create.
func TestNewDocumentsEnterTheDefaultWorkflow(t *testing.T) {
	h, editor := newDocHarness(t)
	view := h.newDoc(t, editor, "Placed")

	if view.Document.WorkflowID == 0 {
		t.Fatal("the new document is in no workflow; documents.workflow_id is NOT NULL and a document with no process is not one this schema holds")
	}
	if view.Document.State != "draft" {
		t.Errorf("state = %q, want the workflow's initial state draft", view.Document.State)
	}

	// The creation event records where it went, so that the history explains a
	// document's starting point rather than assuming today's default.
	got := h.docEvents(t, view.Document.ID)
	if len(got) == 0 || got[len(got)-1].Type != events.DocumentCreated {
		t.Fatalf("the oldest event is not the creation: %+v", got)
	}
	created := got[len(got)-1]
	if created.Payload["state"] != "draft" {
		t.Errorf("the creation event does not record the state: %+v", created.Payload)
	}
	if created.Payload["workflow"] != "Story" {
		t.Errorf("the creation event does not record the workflow: %+v", created.Payload)
	}
}

// TestCreateRefusesAKindWithNoWorkflow is the refusal that replaces a silent
// placement in somebody else's process.
func TestCreateRefusesAKindWithNoWorkflow(t *testing.T) {
	h := newHarness(t)
	h.elementType(t, "photo", domain.KindMedia)
	editor := h.admin(t, "editor@example.com", "correct horse battery", domain.Create)

	_, err := h.CreateDocument(t.Context(), editor, domain.NewDocument{
		SiteID:         h.siteID,
		Kind:           domain.KindMedia,
		ElementTypeKey: "photo",
		Title:          "A Photograph",
	})
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("CreateDocument for a kind with no workflow = %v, want ErrConflict", err)
	}
}

// TestTransitionsNeedsOnlyRead is the rule this layer owns: asking what you
// could do is not doing it.
//
// A viewer sees the menu with every entry refused and a reason on each, which
// is how somebody learns the process without being refused by it.
func TestTransitionsNeedsOnlyRead(t *testing.T) {
	h, editor := newDocHarness(t)
	view := h.newDoc(t, editor, "Readable")
	viewer := h.admin(t, "viewer@example.com", "correct horse battery", domain.Read)

	doc, menu, err := h.Transitions(t.Context(), viewer, view.Document.UID)
	if err != nil {
		t.Fatalf("Transitions for a reader: %v", err)
	}
	if doc.State != "draft" {
		t.Errorf("state = %q, want draft", doc.State)
	}
	if len(menu) == 0 {
		t.Fatal("the menu is empty; draft has two ways out")
	}
	for _, a := range menu {
		if a.Permitted {
			t.Errorf("a reader is permitted %s", a.Transition.Name)
		}
		if a.Reason == "" {
			t.Errorf("%s is refused with no reason", a.Transition.Name)
		}
	}
}

// TestTransitionHidesADocumentTheCallerMayNotSee keeps the API from confirming
// that a document exists to somebody who may not read it.
func TestTransitionHidesADocumentTheCallerMayNotSee(t *testing.T) {
	h, editor := newDocHarness(t)
	view := h.newDoc(t, editor, "Private")
	nobody := h.admin(t, "nobody@example.com", "correct horse battery", domain.NoPrivilege)

	if _, _, err := h.Transitions(t.Context(), nobody, view.Document.UID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Transitions for somebody with no grant = %v, want ErrNotFound", err)
	}
	if _, err := h.Transition(t.Context(), nobody, view.Document.UID, "review", ""); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Transition for somebody with no grant = %v, want ErrNotFound", err)
	}
}

// TestTransitionWritesItsEvent is invariant 7 through this layer, and the
// returned view is the document as it now stands.
func TestTransitionWritesItsEvent(t *testing.T) {
	h, editor := newDocHarness(t)
	view := h.newDoc(t, editor, "Submitted")

	// The default workflow's submit declares has_slug, so the draft gets one.
	slug := "submitted"
	if _, err := h.Checkout(t.Context(), editor, view.Document.UID); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if _, err := h.UpdateDraft(t.Context(), editor, view.Document.UID,
		domain.DraftUpdate{Slug: &slug}); err != nil {
		t.Fatalf("UpdateDraft: %v", err)
	}
	if _, err := h.Checkin(t.Context(), editor, view.Document.UID, "ready"); err != nil {
		t.Fatalf("Checkin: %v", err)
	}

	moved, err := h.Transition(t.Context(), editor, view.Document.UID, "review", "")
	if err != nil {
		t.Fatalf("Transition: %v", err)
	}
	if moved.Document.State != "review" {
		t.Errorf("state = %q, want review", moved.Document.State)
	}
	if moved.Version.ID == 0 {
		t.Error("the returned view carries no version; a document has no title of its own")
	}

	got := h.docEvents(t, view.Document.ID)
	if got[0].Type != events.DocumentTransitioned {
		t.Fatalf("the newest event is %q, want %q", got[0].Type, events.DocumentTransitioned)
	}
	if got[0].ActorID != editor.User.ID {
		t.Errorf("the event names actor %d, want %d", got[0].ActorID, editor.User.ID)
	}
}

// TestSubmitNeedsASlug is the guard reaching this layer intact: the refusal
// answers to ErrGuardFailed and names the guard, which is what the transport
// edge turns into a 409 carrying that name.
func TestSubmitNeedsASlug(t *testing.T) {
	h, editor := newDocHarness(t)
	view := h.newDoc(t, editor, "Unslugged")

	_, err := h.Transition(t.Context(), editor, view.Document.UID, "review", "")
	if !errors.Is(err, domain.ErrGuardFailed) {
		t.Fatalf("Transition without a slug = %v, want a guard refusal", err)
	}
	if g, ok := domain.GuardOf(err); !ok || g != domain.GuardHasSlug {
		t.Errorf("the refusal names %q, want has_slug", g)
	}
}
