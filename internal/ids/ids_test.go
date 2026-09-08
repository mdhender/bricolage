// Copyright (c) 2026 Michael D Henderson.

package ids

import (
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/mdhender/bricolage/internal/clock"
)

func TestNewShape(t *testing.T) {
	c := clock.NewFake(time.Date(2026, 3, 4, 5, 6, 7, 8e6, time.UTC))
	uid, err := New(c.Now())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if len(uid) != Length {
		t.Errorf("len(%q) = %d, want %d", uid, len(uid), Length)
	}
	if uid != strings.ToLower(uid) {
		t.Errorf("%q is not lowercase; uids appear in URLs and in JSON", uid)
	}
	for _, forbidden := range []string{"i", "l", "o", "u"} {
		if strings.Contains(uid, forbidden) {
			t.Errorf("%q contains %q; the Crockford alphabet excludes it", uid, forbidden)
		}
	}
	if !Valid(uid) {
		t.Errorf("Valid(%q) = false", uid)
	}
}

// TestTimeRoundTrips is the property that earns a ULID over a random string:
// the creation time reads back out of the identifier.
func TestTimeRoundTrips(t *testing.T) {
	for _, want := range []time.Time{
		time.UnixMilli(0).UTC(),
		time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC),
		time.Date(2999, 12, 31, 23, 59, 59, 999e6, time.UTC),
	} {
		uid, err := New(want)
		if err != nil {
			t.Fatalf("New(%s): %v", want, err)
		}
		got, err := Time(uid)
		if err != nil {
			t.Fatalf("Time(%q): %v", uid, err)
		}
		if !got.Equal(want.Truncate(time.Millisecond)) {
			t.Errorf("Time(%q) = %s, want %s", uid, got, want)
		}
	}
}

// TestSortsByTime is why uid is the indexed external key: identifiers minted
// later sort later, so an index on uid is not a random walk.
func TestSortsByTime(t *testing.T) {
	c := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	var uids []string
	for range 50 {
		uids = append(uids, MustNew(c.Now()))
		c.Advance(time.Millisecond)
	}
	if !sort.StringsAreSorted(uids) {
		t.Errorf("uids minted in time order do not sort in time order: %v", uids)
	}
}

func TestUnique(t *testing.T) {
	c := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	seen := make(map[string]bool, 10000)
	for range 10000 {
		uid := MustNew(c.Now())
		if seen[uid] {
			t.Fatalf("%q was minted twice within one millisecond", uid)
		}
		seen[uid] = true
	}
}

func TestTimeRange(t *testing.T) {
	for _, tc := range []time.Time{
		time.UnixMilli(-1).UTC(),
		time.UnixMilli(maxTime + 1).UTC(),
	} {
		if _, err := New(tc); !errors.Is(err, ErrTimeRange) {
			t.Errorf("New(%s) = %v, want ErrTimeRange", tc, err)
		}
	}
}

func TestValid(t *testing.T) {
	valid := MustNew(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	for _, tc := range []struct {
		name string
		uid  string
		want bool
	}{
		{"a real one", valid, true},
		{"empty", "", false},
		{"too short", valid[:Length-1], false},
		{"too long", valid + "0", false},
		{"uppercase", strings.ToUpper(valid), false},
		{"the letter i", "0" + strings.Repeat("i", Length-1), false},
		{"the letter l", "0" + strings.Repeat("l", Length-1), false},
		{"the letter o", "0" + strings.Repeat("o", Length-1), false},
		{"the letter u", "0" + strings.Repeat("u", Length-1), false},
		{"a hyphen", "0" + strings.Repeat("-", Length-1), false},
		{"the largest encodable value", "7" + strings.Repeat("z", Length-1), true},
		{"overflowing 128 bits", "8" + strings.Repeat("z", Length-1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Valid(tc.uid); got != tc.want {
				t.Errorf("Valid(%q) = %v, want %v", tc.uid, got, tc.want)
			}
		})
	}
}

// TestEncodingMatchesTheSpecification checks the encoding against a vector
// computed by hand: the epoch with all-zero entropy is 26 zeroes, and the
// all-ones value is the largest encodable ULID.
func TestEncodingMatchesTheSpecification(t *testing.T) {
	if got, want := encode([16]byte{}), strings.Repeat("0", Length); got != want {
		t.Errorf("encode(zero) = %q, want %q", got, want)
	}

	var ones [16]byte
	for i := range ones {
		ones[i] = 0xff
	}
	if got, want := encode(ones), "7"+strings.Repeat("z", Length-1); got != want {
		t.Errorf("encode(ones) = %q, want %q", got, want)
	}

	// The specification's own example, 01ARYZ6S41TSV4RRFFQ69G5FAV, whose
	// timestamp is 1469918176385 ms. Lowercased, its first ten characters are
	// what this encoder must produce for those six bytes.
	uid := encode([16]byte{0x01, 0x56, 0x3d, 0xf3, 0x64, 0x81, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	if want := "01aryz6s41"; uid[:10] != want {
		t.Errorf("timestamp prefix = %q, want %q", uid[:10], want)
	}
	when, err := Time(uid)
	if err != nil {
		t.Fatalf("Time: %v", err)
	}
	if got, want := when.UnixMilli(), int64(1469918176385); got != want {
		t.Errorf("timestamp = %d, want %d", got, want)
	}
}
