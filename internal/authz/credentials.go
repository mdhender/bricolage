// Copyright (c) 2026 Michael D Henderson.

package authz

import (
	"testing"

	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/mdhender/bricolage/internal/domain"
	"golang.org/x/crypto/bcrypt"
)

// Cost is the bcrypt work factor a real run hashes with. bcrypt records it
// inside the hash, so raising it later needs no migration and no second column:
// existing hashes keep verifying at the cost they were written with, and a
// rehash-on-login can be added when somebody wants one.
const Cost = bcrypt.DefaultCost

// TestCost is what a test binary hashes with, and it is the cheapest bcrypt
// offers.
//
// Password hashing was roughly half of "make check": at Cost the suite ran 3:39
// and burned 436s of CPU, at TestCost 1:55 and 167s. internal/service fell 84%,
// internal/api 88%, internal/web 77%, while internal/store and internal/workflow
// -- which hash nothing -- did not move. No test in this repository asserts
// anything about how long a hash takes, so all of that was waste.
const TestCost = bcrypt.MinCost

// cost is the work factor this build actually uses: TestCost under "go test",
// Cost everywhere else.
//
// testing.Testing() is what makes this *only* tests, which is the whole
// requirement. The alternatives were worse in ways worth recording, because
// both look tidier at first glance:
//
//   - The production build tag would have made every untagged binary hash
//     cheaply -- "go run ./cmd/cmsd" included -- and bcrypt records the cost
//     inside the hash, so a password written by a development binary is weak
//     for ever. It would also have made the tag gate behaviour, which
//     DESIGN.md 14 promises it never does.
//   - An exported variable lowered by each test harness would be global mutable
//     state in the package that hashes passwords, and a harness that forgot to
//     set it would be silently slow.
//
// This costs one import of "testing" from non-test code, which is unusual
// enough to justify: testing.Testing() exists for exactly this, it is false in
// anything "go build" produces, and the alternative to importing it is a knob
// somebody has to remember. The linker leaves the rest of the package out --
// the two binaries differ by well under a percent.
//
// One consequence worth knowing rather than discovering: a test that runs cmsd
// as a *subprocess* gets Cost, not TestCost, because that subprocess is not a
// test binary. That is correct -- it is the real binary -- and it is why
// cmd/cmsd's tests barely moved in the measurement above.
func cost() int { return costFor(testing.Testing()) }

// costFor is the decision, separated from the fact so that both answers can be
// asserted without building a second binary. It is the same move invariant 3
// makes with the clock: the caller supplies the circumstance, and what is left
// is a pure function over it.
func costFor(inTest bool) int {
	if inTest {
		return TestCost
	}
	return Cost
}

// TokenBytes is the size of a session token: 32 random bytes, base64url
// encoded (DESIGN.md 14). The database stores only the SHA-256 of it.
const TokenBytes = 32

// MinPasswordLength is the shortest password bootstrap or a password change
// will accept. bcrypt truncates at 72 bytes, so MaxPasswordLength refuses
// anything longer rather than silently ignoring the tail -- a passphrase whose
// last words do not count is worse than a rejected one.
const (
	MinPasswordLength = 12
	MaxPasswordLength = 72
)

// ErrBadCredentials is the single answer to every failed authentication: an
// unknown email, an inactive user, no password set, the wrong password.
//
// One error for four causes is deliberate. Distinguishing them tells an
// attacker which addresses have accounts, and the person who genuinely mistyped
// their address is not helped by knowing which half was wrong.
var ErrBadCredentials = fmt.Errorf("%w: email or password is incorrect", domain.ErrUnauthenticated)

// HashPassword returns the bcrypt hash of a password, after checking that it
// is one the system will store.
func HashPassword(password string) (string, error) {
	if err := ValidatePassword(password); err != nil {
		return "", err
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), cost())
	if err != nil {
		return "", fmt.Errorf("hashing a password: %w", err)
	}
	return string(h), nil
}

// ValidatePassword reports whether a password may be stored.
func ValidatePassword(password string) error {
	switch {
	case len(password) < MinPasswordLength:
		return fmt.Errorf("password: at least %d characters: %w", MinPasswordLength, domain.ErrInvalid)
	case len(password) > MaxPasswordLength:
		return fmt.Errorf("password: at most %d bytes, which is where bcrypt stops reading: %w",
			MaxPasswordLength, domain.ErrInvalid)
	}
	return nil
}

// VerifyPassword checks a password against a user's stored hash.
//
// A user with no password set fails here without a comparison. The empty hash
// is a state, not a missing value, and comparing against it would let anybody
// with an empty password in.
func VerifyPassword(u domain.User, password string) error {
	if !u.Active || !u.HasPassword() {
		return ErrBadCredentials
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)); err != nil {
		if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
			return ErrBadCredentials
		}
		return fmt.Errorf("verifying a password: %w", err)
	}
	return nil
}

// GeneratePassword returns a password for "cmsdb bootstrap admin" to print
// once. It is 32 bytes of crypto/rand in base64url: long enough that its
// strength does not depend on anybody's judgement, and copy-pasteable.
func GeneratePassword() (string, error) {
	b := make([]byte, TokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("reading entropy for a password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// NewToken mints a session token and returns it with its SHA-256, lowercase
// hex. The caller stores the hash and shows the token to its owner once.
func NewToken() (token, hash string, err error) {
	b := make([]byte, TokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("reading entropy for a session token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, HashToken(token), nil
}

// HashToken returns the SHA-256 of a session token, lowercase hex, which is
// what the sessions table is keyed by.
//
// SHA-256 and not bcrypt, deliberately. A password is short and chosen by a
// person, so verifying it must be slow; a token is 256 bits from crypto/rand,
// so there is nothing to slow an attacker down about and a per-request bcrypt
// would be a denial of service with our own name on it.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// TokensEqual compares two tokens in constant time. Nothing in this milestone
// needs it -- lookups go through the hash -- but a comparison that leaks its
// answer through timing is the kind of thing that arrives later by copy, and
// this is the copy to make.
func TokensEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
