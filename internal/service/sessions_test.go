// Copyright (c) 2026 Michael D Henderson.

package service

import (
	"errors"
	"testing"
	"time"

	"github.com/mdhender/bricolage/internal/authz"
	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/events"
	"github.com/mdhender/bricolage/internal/ids"
	"github.com/mdhender/bricolage/internal/store"
)

const password = "correct horse battery"

func TestLogin(t *testing.T) {
	h := newHarness(t)
	h.admin(t, "admin@example.com", password, domain.Publish)

	result, err := h.Login(t.Context(), "admin@example.com", password, "203.0.113.5")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if result.Token == "" {
		t.Fatal("Login returned no token")
	}
	if result.Session.TokenHash == result.Token {
		t.Fatal("the stored hash is the token")
	}
	if got, want := result.Session.ExpiresAt, start.Add(h.SessionTTL()); !got.Equal(want) {
		t.Errorf("expires_at = %s, want %s", got, want)
	}

	// Invariant 7: the operation wrote its event, carrying enough to
	// reconstruct what happened.
	recorded := h.eventsOfType(t, events.SessionCreated)
	if len(recorded) != 1 {
		t.Fatalf("recorded %d %s events, want 1", len(recorded), events.SessionCreated)
	}
	if recorded[0].SubjectID != result.Session.ID {
		t.Errorf("the event names session %d, want %d", recorded[0].SubjectID, result.Session.ID)
	}
	if recorded[0].Payload["email"] != "admin@example.com" {
		t.Errorf("payload = %v, want the email", recorded[0].Payload)
	}
	// The resolved client address, not whatever a header claimed
	// (invariant 14). It is the answer to "where was this session created
	// from" about a session somebody thinks was stolen.
	if recorded[0].Payload["client"] != "203.0.113.5" {
		t.Errorf("payload client = %v, want the resolved client address", recorded[0].Payload["client"])
	}
}

// TestLoginNormalisesTheEmail is the reason users.email is folded before it is
// stored or looked up: without it, "Admin@example.com" and
// "admin@example.com" are two accounts and one login.
func TestLoginNormalisesTheEmail(t *testing.T) {
	h := newHarness(t)
	h.admin(t, "Admin@Example.com", password, domain.Read)

	for _, spelling := range []string{"admin@example.com", "Admin@Example.com", "  ADMIN@EXAMPLE.COM  "} {
		if _, err := h.Login(t.Context(), spelling, password, "203.0.113.5"); err != nil {
			t.Errorf("Login(%q): %v", spelling, err)
		}
	}
}

// TestLoginFailuresAreIndistinguishable is the one answer to four causes. Any
// difference between them tells an attacker which addresses have accounts.
func TestLoginFailuresAreIndistinguishable(t *testing.T) {
	h := newHarness(t)
	h.admin(t, "admin@example.com", password, domain.Read)

	// A user with no password at all: the empty hash is a state, not a value
	// to compare against. It is written through the store, because the
	// service will not create one -- which is itself the point.
	if _, err := h.db.CreateUser(t.Context(), store.NewUser{
		UID:       ids.MustNew(h.Now()),
		Email:     "nopassword@example.com",
		Name:      "No Password",
		CreatedAt: h.Now(),
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	for _, tc := range []struct{ name, email, pass string }{
		{"unknown email", "nobody@example.com", password},
		{"wrong password", "admin@example.com", "not the password"},
		{"empty password", "admin@example.com", ""},
		{"no password set", "nopassword@example.com", ""},
		{"no password set, guessing", "nopassword@example.com", password},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.Login(t.Context(), tc.email, tc.pass, "203.0.113.5")
			if !errors.Is(err, authz.ErrBadCredentials) {
				t.Fatalf("Login = %v, want ErrBadCredentials", err)
			}
			if !errors.Is(err, domain.ErrUnauthenticated) {
				t.Errorf("Login = %v, which will not map to 401", err)
			}
		})
	}

	if got := h.eventsOfType(t, events.SessionCreated); len(got) != 0 {
		t.Errorf("a failed login wrote %d session.created events", len(got))
	}
}

// TestAuthenticate covers the middleware's question, including the expiry that
// PLAN.md M2 acceptance 8 is about.
func TestAuthenticate(t *testing.T) {
	h := newHarness(t)
	h.admin(t, "admin@example.com", password, domain.Publish)

	result, err := h.Login(t.Context(), "admin@example.com", password, "203.0.113.5")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	t.Run("a live session resolves to the identity", func(t *testing.T) {
		identity, err := h.Authenticate(t.Context(), result.Token)
		if err != nil {
			t.Fatalf("Authenticate: %v", err)
		}
		if identity.User.Email != "admin@example.com" {
			t.Errorf("authenticated as %q", identity.User.Email)
		}
		if len(identity.Grants) != 1 || identity.Grants[0].Privilege != domain.Publish {
			t.Errorf("grants = %v, want one publish", identity.Grants)
		}
	})

	t.Run("no credential is unauthenticated", func(t *testing.T) {
		if _, err := h.Authenticate(t.Context(), ""); !errors.Is(err, domain.ErrUnauthenticated) {
			t.Errorf("Authenticate(\"\") = %v, want unauthenticated", err)
		}
	})

	t.Run("an unknown token is unauthenticated", func(t *testing.T) {
		if _, err := h.Authenticate(t.Context(), "not-a-token"); !errors.Is(err, domain.ErrUnauthenticated) {
			t.Errorf("Authenticate = %v, want unauthenticated", err)
		}
	})

	// PLAN.md M2 acceptance 8. The clock is injected, so this costs no wall
	// time and tests the boundary exactly (invariant 3).
	t.Run("an expired session is unauthenticated", func(t *testing.T) {
		h.clock.Set(result.Session.ExpiresAt.Add(-time.Second))
		if _, err := h.Authenticate(t.Context(), result.Token); err != nil {
			t.Fatalf("a session one second before expiry was refused: %v", err)
		}

		h.clock.Set(result.Session.ExpiresAt)
		if _, err := h.Authenticate(t.Context(), result.Token); !errors.Is(err, domain.ErrUnauthenticated) {
			t.Fatalf("a session at its expiry was accepted: %v", err)
		}

		// The expired row is swept on the way past, so a token that stopped
		// working also stops occupying the table.
		if _, err := h.db.SessionByTokenHash(t.Context(), authz.HashToken(result.Token)); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("the expired session is still in the table: %v", err)
		}
	})
}

// TestAuthenticateRecordsUseWithoutWritingEveryTime is the throttle on
// last_seen_at: the write connection is serialized, and one write per read
// would make the server as slow as its slowest writer.
func TestAuthenticateRecordsUseWithoutWritingEveryTime(t *testing.T) {
	h := newHarness(t)
	h.admin(t, "admin@example.com", password, domain.Read)
	result, err := h.Login(t.Context(), "admin@example.com", password, "203.0.113.5")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	h.clock.Advance(time.Second)
	identity, err := h.Authenticate(t.Context(), result.Token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if !identity.Session.LastSeenAt.Equal(start) {
		t.Errorf("last_seen_at moved after one second; the threshold is %s", h.touchAfter)
	}

	h.clock.Advance(2 * DefaultTouchAfter)
	identity, err = h.Authenticate(t.Context(), result.Token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if !identity.Session.LastSeenAt.Equal(h.Now()) {
		t.Errorf("last_seen_at = %s, want %s", identity.Session.LastSeenAt, h.Now())
	}
}

func TestLogout(t *testing.T) {
	h := newHarness(t)
	h.admin(t, "admin@example.com", password, domain.Read)
	result, err := h.Login(t.Context(), "admin@example.com", password, "203.0.113.5")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	if err := h.Logout(t.Context(), result.Identity); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if _, err := h.Authenticate(t.Context(), result.Token); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Errorf("the token still works after logout: %v", err)
	}
	if got := h.eventsOfType(t, events.SessionEnded); len(got) != 1 {
		t.Errorf("recorded %d %s events, want 1", len(got), events.SessionEnded)
	}
}

// TestDevLogin is PLAN.md M2 acceptance 10, 11, and 13.
func TestDevLogin(t *testing.T) {
	h := newHarness(t)
	h.admin(t, "admin@example.com", password, domain.Publish)

	t.Run("an unknown email creates no account", func(t *testing.T) {
		before, err := h.db.CountUsers(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.DevLogin(t.Context(), "nobody@example.com", "127.0.0.1"); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("DevLogin = %v, want a domain.ErrNotFound", err)
		}
		after, err := h.db.CountUsers(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if after != before {
			t.Errorf("the user count went from %d to %d; this route does not create accounts", before, after)
		}
	})

	// Acceptance 13: every dev login writes a session.dev_login event carrying
	// the email and the peer address.
	t.Run("it writes its event", func(t *testing.T) {
		result, err := h.DevLogin(t.Context(), "admin@example.com", "192.0.2.7")
		if err != nil {
			t.Fatalf("DevLogin: %v", err)
		}
		recorded := h.eventsOfType(t, events.SessionDevLogin)
		if len(recorded) != 1 {
			t.Fatalf("recorded %d %s events, want 1", len(recorded), events.SessionDevLogin)
		}
		e := recorded[0]
		if e.SubjectID != result.Session.ID {
			t.Errorf("the event names session %d, want %d", e.SubjectID, result.Session.ID)
		}
		if e.Payload["email"] != "admin@example.com" {
			t.Errorf("payload email = %v", e.Payload["email"])
		}
		if e.Payload["peer"] != "192.0.2.7" {
			t.Errorf("payload peer = %v, want the peer address", e.Payload["peer"])
		}
		if e.ActorID != result.Identity.User.ID {
			t.Errorf("actor = %d, want %d", e.ActorID, result.Identity.User.ID)
		}
	})
}

// TestDevLoginGrantsNoElevation is PLAN.md M2 acceptance 10.
//
// The comparison is the point: the session the development route issues is
// compared against a password-authenticated session for the same user, through
// Resolve, over a subject that every grant they hold could match. If the two
// ever differ, one of the two login paths has grown something the other has
// not, which is exactly what "no elevation" forbids.
func TestDevLoginGrantsNoElevation(t *testing.T) {
	h := newHarness(t)
	h.userWithGrant(t, "editor@example.com", password, domain.Grant{
		Privilege: domain.Edit,
		Scope:     domain.Scope{SiteID: domain.Ref(h.siteID)},
	})

	byPassword, err := h.Login(t.Context(), "editor@example.com", password, "203.0.113.5")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	byDev, err := h.DevLogin(t.Context(), "editor@example.com", "127.0.0.1")
	if err != nil {
		t.Fatalf("DevLogin: %v", err)
	}

	if got, want := byDev.Identity.RoleSlugs(), byPassword.Identity.RoleSlugs(); !equal(got, want) {
		t.Errorf("roles = %v, want %v", got, want)
	}
	if len(byDev.Identity.Grants) != len(byPassword.Identity.Grants) {
		t.Fatalf("grants = %d, want %d", len(byDev.Identity.Grants), len(byPassword.Identity.Grants))
	}

	subjects := []domain.Subject{
		{SiteID: h.siteID, DocKind: "story", State: "draft"},
		{SiteID: h.siteID + 1, DocKind: "story", State: "draft"},
		{},
	}
	for _, subj := range subjects {
		want := authz.Resolve(byPassword.Identity.Grants, subj)
		got := authz.Resolve(byDev.Identity.Grants, subj)
		if got != want {
			t.Errorf("over %+v the development session resolves to %s and the password session to %s", subj, got, want)
		}
	}

	// And the expiry is the ordinary one, not a longer or shorter special
	// case.
	if !byDev.Session.ExpiresAt.Equal(byPassword.Session.ExpiresAt) {
		t.Errorf("the development session expires at %s and the password session at %s",
			byDev.Session.ExpiresAt, byPassword.Session.ExpiresAt)
	}
}

func TestCreateUserRefusesWhatItWillNotStore(t *testing.T) {
	h := newHarness(t)

	for _, tc := range []struct {
		name string
		in   NewUser
	}{
		{"no email", NewUser{Name: "A", Password: password}},
		{"not an address", NewUser{Email: "not-an-address", Name: "A", Password: password}},
		{"no name", NewUser{Email: "a@b.c", Password: password}},
		{"a password too short to store", NewUser{Email: "a@b.c", Name: "A", Password: "short"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := h.CreateUser(t.Context(), tc.in); !errors.Is(err, domain.ErrInvalid) {
				t.Errorf("CreateUser = %v, want a domain.ErrInvalid", err)
			}
		})
	}
}

// TestCreateUserWritesItsEvent is invariant 7 for the one operation M2 has
// that creates something.
func TestCreateUserWritesItsEvent(t *testing.T) {
	h := newHarness(t)
	u, err := h.CreateUser(t.Context(), NewUser{Email: "admin@example.com", Name: "Admin", Password: password})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	recorded := h.eventsOfType(t, events.UserCreated)
	if len(recorded) != 1 {
		t.Fatalf("recorded %d %s events, want 1", len(recorded), events.UserCreated)
	}
	if recorded[0].SubjectID != u.ID || recorded[0].Payload["uid"] != u.UID {
		t.Errorf("the event does not name the user it created: %+v", recorded[0])
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
