// Copyright (c) 2026 Michael D Henderson.

package service

import (
	"testing"
	"time"

	"github.com/mdhender/bricolage/internal/clock"
	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/ids"
	"github.com/mdhender/bricolage/internal/store"
)

// The service tests run against a real in-memory store -- the same migrations
// through the same runner, with foreign keys on -- and a fake clock
// (DESIGN.md 15). They assert on emitted events as well as returned values: an
// operation that does not write its event is not finished (invariant 7).

// start is the instant every test begins at.
var start = time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)

// harness is a service over a fresh in-memory database, and the clock driving
// it.
type harness struct {
	*Service
	clock *clock.Fake
	db    *store.DB

	// siteID is the seeded site every scoped grant in these tests points at.
	siteID int64
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	db, err := store.OpenMemory(t.Context())
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	c := clock.NewFake(start)
	svc, err := New(db, Options{Clock: c})
	if err != nil {
		t.Fatalf("service.New: %v", err)
	}
	h := &harness{Service: svc, clock: c, db: db}

	// One site, which is what "cmsdb seed" creates. It exists here because
	// grants.site_id is a real foreign key: a scope naming site 1 against a
	// database with no sites is refused, and a test that passed anyway would
	// be a test running with foreign keys off.
	id, err := db.CreateSite(t.Context(), store.NewSite{
		UID: ids.MustNew(start), Name: "Default", Domain: "example.com",
	})
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	h.siteID = id
	return h
}

// admin creates a user with a password and gives them a role carrying one
// global grant, which is the shape "cmsdb seed" plus "cmsdb bootstrap admin"
// produces.
func (h *harness) admin(t *testing.T, email, password string, p domain.Privilege) domain.Identity {
	t.Helper()
	return h.userWithGrant(t, email, password, domain.Grant{Privilege: p})
}

func (h *harness) userWithGrant(t *testing.T, email, password string, g domain.Grant) domain.Identity {
	t.Helper()

	u, err := h.CreateUser(t.Context(), NewUser{Email: email, Name: "Test User", Password: password})
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
	if g.Privilege != domain.NoPrivilege {
		g.RoleID = role.ID
		g.CreatedAt = h.Now()
		if _, err := h.db.CreateGrant(t.Context(), g); err != nil {
			t.Fatalf("CreateGrant: %v", err)
		}
	}

	identity, err := h.db.Identity(t.Context(), u.ID)
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	return identity
}

// eventTypes returns the types of every event recorded about a subject,
// newest first.
func (h *harness) eventsOfType(t *testing.T, eventType string) []domain.Event {
	t.Helper()
	got, err := h.db.EventsOfType(t.Context(), eventType, 100)
	if err != nil {
		t.Fatalf("EventsOfType(%q): %v", eventType, err)
	}
	return got
}
