// Copyright (c) 2026 Michael D Henderson.

package authz

import (
	"errors"
	"strings"
	"testing"

	"github.com/mdhender/bricolage/internal/domain"
)

func TestPasswordRoundTrip(t *testing.T) {
	const password = "correct horse battery"

	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if strings.Contains(hash, password) {
		t.Fatal("the hash contains the password")
	}

	u := domain.User{Active: true, PasswordHash: hash}
	if err := VerifyPassword(u, password); err != nil {
		t.Errorf("VerifyPassword: %v", err)
	}
	if err := VerifyPassword(u, password+"x"); !errors.Is(err, ErrBadCredentials) {
		t.Errorf("the wrong password returned %v, want ErrBadCredentials", err)
	}
}

// TestVerifyRefusesWithoutComparing is the empty-hash case.
//
// The empty string is a state -- "this user has never been given a password"
// -- and not a missing value. A verifier that compared against it would let
// anybody in with the empty password, or, worse, with whatever bcrypt does
// with an empty hash.
func TestVerifyRefusesWithoutComparing(t *testing.T) {
	for _, tc := range []struct {
		name string
		user domain.User
	}{
		{"no password set", domain.User{Active: true}},
		{"inactive user", domain.User{Active: false, PasswordHash: mustHash(t, "correct horse battery")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, attempt := range []string{"", "correct horse battery", "anything at all"} {
				if err := VerifyPassword(tc.user, attempt); !errors.Is(err, ErrBadCredentials) {
					t.Errorf("VerifyPassword(%q) = %v, want ErrBadCredentials", attempt, err)
				}
			}
		})
	}
}

// TestBadCredentialsIsUnauthenticated ties the sentinel to the status the edge
// will produce. One error for four causes, and it must be a 401.
func TestBadCredentialsIsUnauthenticated(t *testing.T) {
	if !errors.Is(ErrBadCredentials, domain.ErrUnauthenticated) {
		t.Error("ErrBadCredentials does not wrap domain.ErrUnauthenticated")
	}
}

func TestValidatePassword(t *testing.T) {
	for _, tc := range []struct {
		name     string
		password string
		ok       bool
	}{
		{"too short", strings.Repeat("a", MinPasswordLength-1), false},
		{"the shortest allowed", strings.Repeat("a", MinPasswordLength), true},
		{"the longest allowed", strings.Repeat("a", MaxPasswordLength), true},
		{"past where bcrypt stops reading", strings.Repeat("a", MaxPasswordLength+1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePassword(tc.password)
			if tc.ok && err != nil {
				t.Fatalf("ValidatePassword: %v", err)
			}
			if !tc.ok && !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("ValidatePassword = %v, want a domain.ErrInvalid", err)
			}
		})
	}
}

// TestGeneratedPasswordsDiffer is a smoke test on the entropy source. A
// generator that returns the same value twice is a generator that returns a
// constant, and "cmsdb bootstrap admin" prints it once and cannot take it
// back.
func TestGeneratedPasswordsDiffer(t *testing.T) {
	seen := make(map[string]bool, 64)
	for range 64 {
		p, err := GeneratePassword()
		if err != nil {
			t.Fatalf("GeneratePassword: %v", err)
		}
		if err := ValidatePassword(p); err != nil {
			t.Fatalf("a generated password is not one this system would store: %v", err)
		}
		if seen[p] {
			t.Fatal("GeneratePassword repeated itself")
		}
		seen[p] = true
	}
}

// TestTokens covers the two properties the sessions table depends on: a token
// is unguessable, and the hash is a function of it alone.
func TestTokens(t *testing.T) {
	seen := make(map[string]bool, 64)
	for range 64 {
		token, hash, err := NewToken()
		if err != nil {
			t.Fatalf("NewToken: %v", err)
		}
		if seen[token] {
			t.Fatal("NewToken repeated itself")
		}
		seen[token] = true

		if hash == token {
			t.Fatal("the stored hash is the token; the table would be a set of usable credentials")
		}
		if got := HashToken(token); got != hash {
			t.Errorf("HashToken(%q) = %q, want %q", token, got, hash)
		}
		if len(hash) != 64 {
			t.Errorf("hash is %d characters, want 64 hex characters of SHA-256", len(hash))
		}
	}
}

func TestTokensEqual(t *testing.T) {
	if !TokensEqual("abc", "abc") {
		t.Error("equal tokens compared unequal")
	}
	if TokensEqual("abc", "abd") || TokensEqual("abc", "abcd") {
		t.Error("unequal tokens compared equal")
	}
}

func mustHash(t *testing.T, password string) string {
	t.Helper()
	h, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	return h
}
