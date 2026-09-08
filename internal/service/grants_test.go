// Copyright (c) 2026 Michael D Henderson.

package service

import (
	"errors"
	"testing"

	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/events"
)

// TestCreateGrantRefusesAnEscalation is PLAN.md M2 acceptance 7 and
// invariant 12: a user holding EDIT on a scope cannot create a PUBLISH grant
// on it, the attempt is a forbidden, and nothing is written.
//
// "Nothing is written" is asserted rather than assumed. The check happens
// before the INSERT rather than inside a transaction somebody may forget to
// roll back, and the way to know that stayed true is to count the rows.
func TestCreateGrantRefusesAnEscalation(t *testing.T) {
	h := newHarness(t)
	editor := h.admin(t, "editor@example.com", password, domain.Edit)

	target, err := h.db.CreateRole(t.Context(), "target", "Target")
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}

	_, err = h.CreateGrant(t.Context(), editor, GrantRequest{
		RoleSlug:  "target",
		Privilege: domain.Publish,
	})
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("CreateGrant = %v, want a domain.ErrForbidden, which the edge maps to 403", err)
	}

	written, err := h.db.GrantsForRole(t.Context(), target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != 0 {
		t.Errorf("the refused grant wrote %d rows", len(written))
	}
	if got := h.eventsOfType(t, events.GrantCreated); len(got) != 0 {
		t.Errorf("the refused grant wrote %d events", len(got))
	}
}

// TestCreateGrantAllowsWhatTheActorHolds is the other half: the check refuses
// an escalation and not everything.
func TestCreateGrantAllowsWhatTheActorHolds(t *testing.T) {
	h := newHarness(t)
	admin := h.admin(t, "admin@example.com", password, domain.Publish)

	if _, err := h.db.CreateRole(t.Context(), "target", "Target"); err != nil {
		t.Fatalf("CreateRole: %v", err)
	}

	g, err := h.CreateGrant(t.Context(), admin, GrantRequest{
		RoleSlug:  "target",
		Privilege: domain.Edit,
		Scope:     domain.Scope{SiteID: domain.Ref(h.siteID), DocKind: domain.Ref("story")},
	})
	if err != nil {
		t.Fatalf("CreateGrant: %v", err)
	}
	if g.ID == 0 {
		t.Fatal("CreateGrant returned no id")
	}
	if g.CreatedBy != admin.User.ID {
		t.Errorf("created_by = %d, want %d", g.CreatedBy, admin.User.ID)
	}

	// Invariant 7.
	recorded := h.eventsOfType(t, events.GrantCreated)
	if len(recorded) != 1 {
		t.Fatalf("recorded %d %s events, want 1", len(recorded), events.GrantCreated)
	}
	if recorded[0].Payload["role"] != "target" || recorded[0].Payload["privilege"] != "edit" {
		t.Errorf("payload = %v, want the role and the privilege", recorded[0].Payload)
	}
	if recorded[0].Payload["scope"] != g.Scope.String() {
		t.Errorf("payload scope = %v, want %q", recorded[0].Payload["scope"], g.Scope.String())
	}
}

// TestCreateGrantRefusesAWiderScope is the escalation that is easy to miss:
// the actor holds everything over one site, and writes a grant over all of
// them.
func TestCreateGrantRefusesAWiderScope(t *testing.T) {
	h := newHarness(t)
	siteAdmin := h.userWithGrant(t, "site@example.com", password, domain.Grant{
		Privilege: domain.Publish,
		Scope:     domain.Scope{SiteID: domain.Ref(h.siteID)},
	})

	if _, err := h.db.CreateRole(t.Context(), "target", "Target"); err != nil {
		t.Fatalf("CreateRole: %v", err)
	}

	if _, err := h.CreateGrant(t.Context(), siteAdmin, GrantRequest{
		RoleSlug:  "target",
		Privilege: domain.Read,
	}); !errors.Is(err, domain.ErrForbidden) {
		t.Errorf("a site administrator wrote a global grant: %v", err)
	}

	// The same privilege on their own site is fine.
	if _, err := h.CreateGrant(t.Context(), siteAdmin, GrantRequest{
		RoleSlug:  "target",
		Privilege: domain.Read,
		Scope:     domain.Scope{SiteID: domain.Ref(h.siteID)},
	}); err != nil {
		t.Errorf("a site administrator could not grant on their own site: %v", err)
	}
}

// TestCreateGrantRefusesAMalformedScope keeps a category constraint from being
// stored without the path a subtree match needs. A constraint that silently
// matches nothing is the worst of the three possible answers.
func TestCreateGrantRefusesAMalformedScope(t *testing.T) {
	h := newHarness(t)
	admin := h.admin(t, "admin@example.com", password, domain.Publish)
	if _, err := h.db.CreateRole(t.Context(), "target", "Target"); err != nil {
		t.Fatalf("CreateRole: %v", err)
	}

	_, err := h.CreateGrant(t.Context(), admin, GrantRequest{
		RoleSlug:  "target",
		Privilege: domain.Read,
		Scope:     domain.Scope{CategoryID: domain.Ref(int64(3))},
	})
	if !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("CreateGrant = %v, want a domain.ErrInvalid", err)
	}
}

// TestAssignRoleChecksEveryGrantItCarries: handing somebody a role hands them
// every grant it carries, so the actor must be able to confer each of them.
func TestAssignRoleChecksEveryGrantItCarries(t *testing.T) {
	h := newHarness(t)
	editor := h.admin(t, "editor@example.com", password, domain.Edit)
	victim := h.admin(t, "victim@example.com", password, domain.Read)

	publisher, err := h.db.CreateRole(t.Context(), "publisher", "Publisher")
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	if _, err := h.db.CreateGrant(t.Context(), domain.Grant{
		RoleID:    publisher.ID,
		Privilege: domain.Publish,
		CreatedAt: h.Now(),
	}); err != nil {
		t.Fatalf("CreateGrant: %v", err)
	}

	if err := h.AssignRole(t.Context(), editor, victim.User.UID, "publisher"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("an editor handed out a publisher role: %v", err)
	}

	after, err := h.db.Identity(t.Context(), victim.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.HasRole("publisher") {
		t.Error("the refused assignment took effect anyway")
	}

	// An administrator may do it, and it writes its event.
	admin := h.admin(t, "admin@example.com", password, domain.Publish)
	if err := h.AssignRole(t.Context(), admin, victim.User.UID, "publisher"); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}
	if got := h.eventsOfType(t, events.RoleAssigned); len(got) != 1 {
		t.Errorf("recorded %d %s events, want 1", len(got), events.RoleAssigned)
	}
}
