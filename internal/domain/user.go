// Copyright (c) 2026 Michael D Henderson.

package domain

import (
	"fmt"
	"strings"
	"time"
)

// User is a person the system knows. The uid is what the API speaks; the
// integer id never leaves this process (invariant 10).
type User struct {
	ID        int64
	UID       string
	Email     string
	Name      string
	Active    bool
	CreatedAt time.Time

	// PasswordHash is the stored bcrypt hash, or the empty string when the
	// user has never been given a password. The empty string is a definite
	// failure to verify and not a value to compare against: a user with no
	// password must not be able to log in with one.
	//
	// It is never serialised, never logged, and never leaves the store except
	// on its way into the verifier (DESIGN.md 14, "Logging").
	PasswordHash string
}

// HasPassword reports whether the user has a password set at all.
func (u User) HasPassword() bool { return u.PasswordHash != "" }

// NormalizeEmail is the one place an address is folded before it is stored or
// looked up.
//
// Case folding matters more than it looks: users.email is UNIQUE, and without
// folding "Admin@example.com" and "admin@example.com" are two accounts. The
// local part of an address is technically case-sensitive; no mail system that
// anybody uses treats it that way, and two accounts differing only in case is
// a worse failure than the one this trades away.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// ValidateEmail is the shape check applied before an address reaches the
// store. It is deliberately weak -- one "@", something either side, no spaces
// -- because the strong check is delivery, which this system does not do.
func ValidateEmail(email string) error {
	if email == "" {
		return fmt.Errorf("email: required: %w", ErrInvalid)
	}
	at := strings.IndexByte(email, '@')
	if at <= 0 || at == len(email)-1 || strings.ContainsAny(email, " \t\r\n") ||
		strings.Count(email, "@") != 1 {
		return fmt.Errorf("email %q: not an address: %w", email, ErrInvalid)
	}
	return nil
}

// Session is one login: a row keyed by the SHA-256 of a token that this
// database never holds in the clear.
type Session struct {
	ID         int64
	UserID     int64
	TokenHash  string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	LastSeenAt time.Time
}

// Expired reports whether the session has expired as of now.
//
// The instant is passed in rather than read, because time.Now belongs to main
// and internal/clock (invariant 3) and because an expiry that cannot be tested
// at an arbitrary instant is an expiry nobody tests.
func (s Session) Expired(now time.Time) bool { return !now.Before(s.ExpiresAt) }

// Identity is who the caller is and what they may do: the user, the roles they
// hold, and the grants those roles carry.
//
// It is assembled once per request and read from the request context
// (DESIGN.md 7.2). The grants travel with it so that authorization is a pure
// function over a value the transport already has, rather than a query in the
// middle of a handler.
type Identity struct {
	User    User
	Roles   []Role
	Grants  []Grant
	Session Session
}

// HasRole reports whether the identity holds the role with this slug.
func (i Identity) HasRole(slug string) bool {
	for _, r := range i.Roles {
		if r.Slug == slug {
			return true
		}
	}
	return false
}

// RoleSlugs returns the slugs of the roles held, in the order they were
// loaded.
func (i Identity) RoleSlugs() []string {
	out := make([]string, 0, len(i.Roles))
	for _, r := range i.Roles {
		out = append(out, r.Slug)
	}
	return out
}
