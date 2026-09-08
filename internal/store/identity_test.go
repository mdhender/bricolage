// Copyright (c) 2026 Michael D Henderson.

package store

import (
	"errors"
	"testing"
	"time"

	"github.com/mdhender/bricolage/internal/clock"
	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/ids"
)

// The store half of M2: users, roles, grants, sessions, and events, against a
// real in-memory database with every migration applied through the same code
// path cmsdb uses and with foreign keys on (DESIGN.md 15, invariant 22).

// fixedClock is the instant these tests happen at. A fixed clock is what makes
// "expires twelve hours from now" a value to assert on rather than a window.
func fixedClock() *clock.Fake {
	return clock.NewFake(time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC))
}

// makeUser inserts a user and returns it.
func makeUser(t *testing.T, db *DB, email string) domain.User {
	t.Helper()
	now := fixedClock().Now()
	u, err := db.CreateUser(t.Context(), NewUser{
		UID:          ids.MustNew(now),
		Email:        email,
		Name:         "Test " + email,
		PasswordHash: "$2a$10$notarealhashbutthecolumnholdsit",
		CreatedAt:    now,
	})
	if err != nil {
		t.Fatalf("CreateUser(%q): %v", email, err)
	}
	return u
}

// TestUsers covers the round trip and the constraint that makes "cmsdb
// bootstrap admin" idempotent.
func TestUsers(t *testing.T) {
	db := memoryStore(t)
	u := makeUser(t, db, "admin@example.com")

	if u.ID == 0 || u.UID == "" {
		t.Fatalf("CreateUser returned %+v", u)
	}

	for _, tc := range []struct {
		name string
		read func() (domain.User, error)
	}{
		{"by email", func() (domain.User, error) { return db.UserByEmail(t.Context(), u.Email) }},
		{"by uid", func() (domain.User, error) { return db.UserByUID(t.Context(), u.UID) }},
		{"by id", func() (domain.User, error) { return db.UserByID(t.Context(), u.ID) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.read()
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if got.ID != u.ID || got.Email != u.Email || got.PasswordHash != u.PasswordHash {
				t.Errorf("read %+v, want %+v", got, u)
			}
			if !got.CreatedAt.Equal(u.CreatedAt) {
				t.Errorf("created_at = %s, want %s", got.CreatedAt, u.CreatedAt)
			}
		})
	}

	t.Run("a missing user is a domain.ErrNotFound", func(t *testing.T) {
		if _, err := db.UserByEmail(t.Context(), "nobody@example.com"); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("UserByEmail returned %v, want a domain.ErrNotFound", err)
		}
	})

	// The duplicate is detected by result code and never by matching the
	// message (invariant 11), and it answers to domain.ErrConflict so that the
	// caller does not have to know SQLite was involved.
	t.Run("a duplicate email is a conflict, by result code", func(t *testing.T) {
		now := fixedClock().Now()
		_, err := db.CreateUser(t.Context(), NewUser{
			UID:       ids.MustNew(now),
			Email:     u.Email,
			Name:      "Somebody Else",
			CreatedAt: now,
		})
		if !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("the duplicate returned %v, want a domain.ErrConflict", err)
		}
		ce, ok := AsConstraint(err)
		if !ok {
			t.Fatalf("the duplicate returned %v, want a *ConstraintError", err)
		}
		if !ce.IsUnique() {
			t.Errorf("the violated constraint was %v, want a UNIQUE index", ce.Code)
		}
	})
}

// TestIdentityCarriesRolesAndGrants is the query the authentication middleware
// makes: one call, one connection, so the three reads cannot see three
// different states.
func TestIdentityCarriesRolesAndGrants(t *testing.T) {
	db := memoryStore(t)
	u := makeUser(t, db, "editor@example.com")
	now := fixedClock().Now()

	editor, err := db.CreateRole(t.Context(), "editor", "Editor")
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	writer, err := db.CreateRole(t.Context(), "writer", "Writer")
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}

	siteID, err := db.CreateSite(t.Context(), NewSite{
		UID: ids.MustNew(now), Name: "Default", Domain: "example.com",
	})
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}

	if _, err := db.CreateGrant(t.Context(), domain.Grant{
		RoleID:    editor.ID,
		Privilege: domain.Create,
		Scope:     domain.Scope{SiteID: &siteID, DocKind: domain.Ref("story")},
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("CreateGrant: %v", err)
	}
	if _, err := db.CreateGrant(t.Context(), domain.Grant{
		RoleID:    writer.ID,
		Privilege: domain.Edit,
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("CreateGrant: %v", err)
	}

	// Only the editor role is assigned, so only its grant may appear.
	if err := db.AssignRole(t.Context(), u.ID, editor.ID); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}

	identity, err := db.Identity(t.Context(), u.ID)
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	if identity.User.ID != u.ID {
		t.Errorf("Identity returned user %d, want %d", identity.User.ID, u.ID)
	}
	if got := identity.RoleSlugs(); len(got) != 1 || got[0] != "editor" {
		t.Errorf("roles = %v, want [editor]", got)
	}
	if len(identity.Grants) != 1 {
		t.Fatalf("grants = %d, want 1; a role the user does not hold leaked in", len(identity.Grants))
	}

	// NULL is the wildcard, and it must survive the round trip as nil rather
	// than as a zero that matches nothing.
	g := identity.Grants[0]
	if g.Privilege != domain.Create {
		t.Errorf("privilege = %s, want create", g.Privilege)
	}
	if g.Scope.SiteID == nil || *g.Scope.SiteID != siteID {
		t.Errorf("site = %v, want %d", g.Scope.SiteID, siteID)
	}
	if g.Scope.DocKind == nil || *g.Scope.DocKind != "story" {
		t.Errorf("doc_kind = %v, want \"story\"", g.Scope.DocKind)
	}
	if g.Scope.State != nil {
		t.Errorf("state = %v, want nil; NULL is the wildcard", *g.Scope.State)
	}
	if g.Scope.IsGlobal() {
		t.Error("a scope with two constraints reported itself as global")
	}

	// Assigning a role twice is not an error: the end state is what was asked
	// for, and "cmsdb seed" runs more than once.
	if err := db.AssignRole(t.Context(), u.ID, editor.ID); err != nil {
		t.Errorf("the second AssignRole failed: %v", err)
	}
}

// TestGrantsReferentialIntegrity is invariant 22 doing its job: a grant for a
// role that does not exist must be refused, and by result code.
func TestGrantsReferentialIntegrity(t *testing.T) {
	db := memoryStore(t)

	_, err := db.CreateGrant(t.Context(), domain.Grant{
		RoleID:    999,
		Privilege: domain.Read,
		CreatedAt: fixedClock().Now(),
	})
	ce, ok := AsConstraint(err)
	if !ok {
		t.Fatalf("CreateGrant returned %v, want a *ConstraintError", err)
	}
	if !ce.IsForeignKey() {
		t.Errorf("the violated constraint was %v, want a foreign key; a test that passes with foreign keys off proves nothing", ce.Code)
	}
}

// TestSessions covers the round trip, the expiry the caller judges, and the
// deletion that logging out is.
func TestSessions(t *testing.T) {
	db := memoryStore(t)
	u := makeUser(t, db, "admin@example.com")
	c := fixedClock()
	now := c.Now()

	sess, err := db.CreateSession(t.Context(), NewSession{
		UserID:    u.ID,
		TokenHash: "0123456789abcdef",
		CreatedAt: now,
		ExpiresAt: now.Add(12 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	got, err := db.SessionByTokenHash(t.Context(), "0123456789abcdef")
	if err != nil {
		t.Fatalf("SessionByTokenHash: %v", err)
	}
	if got.ID != sess.ID || got.UserID != u.ID {
		t.Errorf("read %+v, want %+v", got, sess)
	}
	if !got.ExpiresAt.Equal(now.Add(12 * time.Hour)) {
		t.Errorf("expires_at = %s, want %s", got.ExpiresAt, now.Add(12*time.Hour))
	}

	// Expiry is the caller's question, asked against an injected clock, and
	// the store does not answer it.
	if got.Expired(now) {
		t.Error("a fresh session reported itself expired")
	}
	if !got.Expired(now.Add(12 * time.Hour)) {
		t.Error("a session did not expire at its expiry")
	}

	t.Run("touch records use", func(t *testing.T) {
		later := c.Advance(time.Hour)
		if err := db.TouchSession(t.Context(), sess.ID, later); err != nil {
			t.Fatalf("TouchSession: %v", err)
		}
		got, err := db.SessionByTokenHash(t.Context(), "0123456789abcdef")
		if err != nil {
			t.Fatalf("SessionByTokenHash: %v", err)
		}
		if !got.LastSeenAt.Equal(later) {
			t.Errorf("last_seen_at = %s, want %s", got.LastSeenAt, later)
		}
	})

	t.Run("an unknown token is a not found", func(t *testing.T) {
		if _, err := db.SessionByTokenHash(t.Context(), "deadbeef"); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("SessionByTokenHash returned %v, want a domain.ErrNotFound", err)
		}
	})

	t.Run("delete", func(t *testing.T) {
		if err := db.DeleteSession(t.Context(), sess.ID); err != nil {
			t.Fatalf("DeleteSession: %v", err)
		}
		if _, err := db.SessionByTokenHash(t.Context(), "0123456789abcdef"); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("the session survived deletion: %v", err)
		}
	})
}

// TestDeleteExpiredSessions removes what expired and leaves what did not.
func TestDeleteExpiredSessions(t *testing.T) {
	db := memoryStore(t)
	u := makeUser(t, db, "admin@example.com")
	now := fixedClock().Now()

	for i, ttl := range []time.Duration{-time.Hour, -time.Minute, time.Hour} {
		if _, err := db.CreateSession(t.Context(), NewSession{
			UserID:    u.ID,
			TokenHash: string(rune('a'+i)) + "000",
			CreatedAt: now.Add(-2 * time.Hour),
			ExpiresAt: now.Add(ttl),
		}); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
	}

	n, err := db.DeleteExpiredSessions(t.Context(), now)
	if err != nil {
		t.Fatalf("DeleteExpiredSessions: %v", err)
	}
	if n != 2 {
		t.Errorf("deleted %d sessions, want 2", n)
	}
	if _, err := db.SessionByTokenHash(t.Context(), "c000"); err != nil {
		t.Errorf("the live session was deleted too: %v", err)
	}
}

// TestSessionsCascadeWithTheirUser is the ON DELETE CASCADE, which only works
// with foreign keys on (invariant 22).
func TestEventsRoundTrip(t *testing.T) {
	db := memoryStore(t)
	u := makeUser(t, db, "admin@example.com")
	now := fixedClock().Now()

	id, err := db.RecordEvent(t.Context(), domain.Event{
		Type:        "session.dev_login",
		ActorID:     u.ID,
		SubjectKind: domain.SubjectSession,
		SubjectID:   7,
		Payload:     map[string]any{"email": u.Email, "peer": "127.0.0.1"},
		OccurredAt:  now,
	})
	if err != nil {
		t.Fatalf("RecordEvent: %v", err)
	}
	if id == 0 {
		t.Fatal("RecordEvent returned no id")
	}

	got, err := db.EventsForSubject(t.Context(), domain.SubjectSession, 7, 10)
	if err != nil {
		t.Fatalf("EventsForSubject: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("read %d events, want 1", len(got))
	}
	e := got[0]
	if e.Type != "session.dev_login" || e.ActorID != u.ID {
		t.Errorf("read %+v", e)
	}
	if e.Payload["email"] != u.Email || e.Payload["peer"] != "127.0.0.1" {
		t.Errorf("payload = %v, want the email and the peer", e.Payload)
	}
	if !e.OccurredAt.Equal(now) {
		t.Errorf("occurred_at = %s, want %s", e.OccurredAt, now)
	}

	t.Run("a system event has no actor", func(t *testing.T) {
		if _, err := db.RecordEvent(t.Context(), domain.Event{
			Type:        "user.created",
			SubjectKind: domain.SubjectUser,
			SubjectID:   u.ID,
			OccurredAt:  now,
		}); err != nil {
			t.Fatalf("RecordEvent: %v", err)
		}
		got, err := db.EventsOfType(t.Context(), "user.created", 10)
		if err != nil {
			t.Fatalf("EventsOfType: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("read %d events, want 1", len(got))
		}
		if got[0].ActorID != 0 {
			t.Errorf("actor = %d, want 0 for the system", got[0].ActorID)
		}
		if got[0].Payload != nil {
			t.Errorf("payload = %v, want nil for an empty one", got[0].Payload)
		}
	})

	t.Run("an event with a missing actor is refused", func(t *testing.T) {
		_, err := db.RecordEvent(t.Context(), domain.Event{
			Type:        "user.created",
			ActorID:     999,
			SubjectKind: domain.SubjectUser,
			SubjectID:   1,
			OccurredAt:  now,
		})
		ce, ok := AsConstraint(err)
		if !ok || !ce.IsForeignKey() {
			t.Errorf("RecordEvent returned %v, want a foreign key violation", err)
		}
	})

	t.Run("an event with no type is refused before it reaches SQL", func(t *testing.T) {
		if _, err := db.RecordEvent(t.Context(), domain.Event{
			SubjectKind: domain.SubjectUser, SubjectID: 1, OccurredAt: now,
		}); !errors.Is(err, domain.ErrInvalid) {
			t.Errorf("RecordEvent returned %v, want a domain.ErrInvalid", err)
		}
	})
}
