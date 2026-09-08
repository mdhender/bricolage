// Copyright (c) 2026 Michael D Henderson.

package ids

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

// Length is the number of characters in a ULID: 26, encoding 128 bits in
// Crockford base32.
const Length = 26

// alphabet is Crockford's base32, lowercased.
//
// Lowercase is the repository's choice, not the ULID specification's: uids
// appear in URLs and in JSON, where a case difference is the kind of thing
// that costs an afternoon. It excludes i, l, o, and u, so the two characters
// most often confused with digits cannot appear at all.
const alphabet = "0123456789abcdefghjkmnpqrstvwxyz"

// ErrTimeRange is returned for an instant a ULID cannot represent. The
// timestamp is 48 bits of Unix milliseconds, which runs out in the year 10889
// and starts at the epoch.
var ErrTimeRange = errors.New("timestamp out of ULID range")

// maxTime is the largest Unix millisecond value that fits in 48 bits.
const maxTime = int64(1)<<48 - 1

// New returns a lowercase ULID for the instant t: 48 bits of Unix
// milliseconds, big-endian, followed by 80 bits from crypto/rand.
//
// The caller passes the time rather than this package reading it, because
// time.Now belongs to main and internal/clock (invariant 3). Everything that
// mints a uid already holds a Clock.
//
// Two uids minted in the same millisecond sort in an arbitrary order relative
// to one another. That is a deliberate simplification: the specification's
// monotonic-within-a-millisecond variant requires shared mutable state, and
// nothing here depends on the ordering of uids at millisecond resolution.
// Sort by a timestamp column when the order matters.
func New(t time.Time) (string, error) {
	var b [16]byte
	ms := t.UTC().UnixMilli()
	if ms < 0 || ms > maxTime {
		return "", fmt.Errorf("%s: %w", t.UTC().Format(time.RFC3339), ErrTimeRange)
	}
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	if _, err := rand.Read(b[6:]); err != nil {
		return "", fmt.Errorf("reading entropy for a uid: %w", err)
	}
	return encode(b), nil
}

// MustNew is New for a caller that has nowhere to put an error, such as a
// test. It panics rather than returning a uid that is not one.
func MustNew(t time.Time) string {
	s, err := New(t)
	if err != nil {
		panic("ids: " + err.Error())
	}
	return s
}

// encode writes the 128 bits of b as 26 base32 characters, most significant
// first. 26 characters carry 130 bits, so the first character encodes only the
// top three bits of b[0] and is never above '7'.
func encode(b [16]byte) string {
	hi := binary.BigEndian.Uint64(b[0:8])
	lo := binary.BigEndian.Uint64(b[8:16])

	var out [Length]byte
	for i := Length - 1; i >= 0; i-- {
		out[i] = alphabet[lo&0x1f]
		lo = lo>>5 | hi<<59
		hi >>= 5
	}
	return string(out[:])
}

// Valid reports whether s is a well-formed uid: 26 characters, all from the
// lowercase Crockford alphabet, and not overflowing 128 bits.
//
// It is a check on the shape of a string, not a claim that anything is
// identified by it. The store decides that.
func Valid(s string) bool {
	if len(s) != Length {
		return false
	}
	// 26 characters carry 130 bits and a ULID is 128, so the leading
	// character may not set either of its top two bits. Rejecting the
	// overflow here keeps Valid a total function on strings.
	if s[0] > '7' {
		return false
	}
	for i := range len(s) {
		if value(s[i]) < 0 {
			return false
		}
	}
	return true
}

// value returns the base32 value of c, or -1 if c is not in the alphabet.
//
// The alphabet skips i, l, o, and u, so there is no arithmetic that maps a
// letter to its value. A scan of 32 bytes is cheaper to read than the
// arithmetic that would avoid it, and this is not a hot path.
func value(c byte) int {
	for i := range len(alphabet) {
		if alphabet[i] == c {
			return i
		}
	}
	return -1
}

// Time returns the instant encoded in the first 48 bits of s, in UTC. It is
// what makes a uid worth having over a random string: a row's approximate
// creation time is readable from its identifier without a query.
func Time(s string) (time.Time, error) {
	if !Valid(s) {
		return time.Time{}, fmt.Errorf("uid %q: not a ULID", s)
	}
	// The first ten characters carry 50 bits, of which the top two are zero,
	// so the timestamp is exactly what they hold.
	var ms int64
	for i := range 10 {
		ms = ms<<5 | int64(value(s[i]))
	}
	return time.UnixMilli(ms).UTC(), nil
}
