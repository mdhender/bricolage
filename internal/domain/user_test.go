// Copyright (c) 2026 Michael D Henderson.

package domain

import (
	"errors"
	"testing"
	"time"
)

// TestNormalizeEmail is the fold that keeps users.email a useful UNIQUE index.
// Without it "Admin@example.com" and "admin@example.com" are two accounts.
func TestNormalizeEmail(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"admin@example.com", "admin@example.com"},
		{"Admin@Example.com", "admin@example.com"},
		{"  ADMIN@EXAMPLE.COM  ", "admin@example.com"},
		{"", ""},
	} {
		if got := NormalizeEmail(tc.in); got != tc.want {
			t.Errorf("NormalizeEmail(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestValidateEmail(t *testing.T) {
	for _, tc := range []struct {
		in string
		ok bool
	}{
		{"admin@example.com", true},
		{"a@b", true},
		{"a+tag@example.co.uk", true},

		{"", false},
		{"admin", false},
		{"@example.com", false},
		{"admin@", false},
		{"admin@@example.com", false},
		{"admin @example.com", false},
		{"admin@example.com\nBcc: someone", false},
	} {
		err := ValidateEmail(tc.in)
		if tc.ok && err != nil {
			t.Errorf("ValidateEmail(%q): %v", tc.in, err)
		}
		if !tc.ok && !errors.Is(err, ErrInvalid) {
			t.Errorf("ValidateEmail(%q) = %v, want a domain.ErrInvalid", tc.in, err)
		}
	}
}

// TestHasPassword: the empty hash is a state -- "never given a password" --
// and not a missing value.
func TestHasPassword(t *testing.T) {
	if (User{}).HasPassword() {
		t.Error("a user with no hash reported having a password")
	}
	if !(User{PasswordHash: "$2a$10$..."}).HasPassword() {
		t.Error("a user with a hash reported having none")
	}
}

// TestSessionExpiry is the boundary, at the instant. The clock is passed in
// rather than read, because an expiry that cannot be tested at an arbitrary
// instant is an expiry nobody tests (invariant 3).
func TestSessionExpiry(t *testing.T) {
	at := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	s := Session{ExpiresAt: at}

	if s.Expired(at.Add(-time.Nanosecond)) {
		t.Error("a session expired one nanosecond early")
	}
	if !s.Expired(at) {
		t.Error("a session did not expire at its expiry")
	}
	if !s.Expired(at.Add(time.Hour)) {
		t.Error("a session did not expire an hour after its expiry")
	}
}

func TestIdentityRoles(t *testing.T) {
	id := Identity{Roles: []Role{{Slug: "admin"}, {Slug: "editor"}}}

	if !id.HasRole("admin") || !id.HasRole("editor") {
		t.Error("HasRole missed a role the identity holds")
	}
	if id.HasRole("writer") {
		t.Error("HasRole found a role the identity does not hold")
	}
	if got := id.RoleSlugs(); len(got) != 2 || got[0] != "admin" || got[1] != "editor" {
		t.Errorf("RoleSlugs = %v", got)
	}
	if got := (Identity{}).RoleSlugs(); len(got) != 0 {
		t.Errorf("RoleSlugs of an empty identity = %v, want empty", got)
	}
}

// TestEventPayloadIsAlwaysAnObject: the column defaults to "{}" and other code
// hands it to a JSON parser, so it must never be "" or "null".
func TestEventPayloadIsAlwaysAnObject(t *testing.T) {
	for _, e := range []Event{
		{},
		{Payload: map[string]any{}},
		{Payload: nil},
	} {
		got, err := e.PayloadJSON()
		if err != nil {
			t.Fatalf("PayloadJSON: %v", err)
		}
		if got != "{}" {
			t.Errorf("PayloadJSON = %q, want \"{}\"", got)
		}
	}

	e := Event{Payload: map[string]any{"email": "a@b.c"}}
	got, err := e.PayloadJSON()
	if err != nil {
		t.Fatalf("PayloadJSON: %v", err)
	}
	if got != `{"email":"a@b.c"}` {
		t.Errorf("PayloadJSON = %q", got)
	}
}

func TestEventValidate(t *testing.T) {
	at := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	valid := Event{Type: "user.created", SubjectKind: SubjectUser, SubjectID: 1, OccurredAt: at}
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	for _, tc := range []struct {
		name  string
		event Event
	}{
		{"no type", Event{SubjectKind: SubjectUser, SubjectID: 1, OccurredAt: at}},
		{"no subject kind", Event{Type: "user.created", SubjectID: 1, OccurredAt: at}},
		{"no timestamp", Event{Type: "user.created", SubjectKind: SubjectUser, SubjectID: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.event.Validate(); !errors.Is(err, ErrInvalid) {
				t.Errorf("Validate = %v, want a domain.ErrInvalid", err)
			}
		})
	}
}
