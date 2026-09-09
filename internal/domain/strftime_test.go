// Copyright (c) 2026 Michael D Henderson.

package domain

import (
	"errors"
	"testing"
	"time"
)

// The strftime formatter (DESIGN.md 5.3).
//
// Table-driven and exhaustive over the conversions this system implements,
// because a URI format is configuration a person typed and every one of these
// ends up in somebody's address bar.

func TestStrftime(t *testing.T) {
	// A Sunday afternoon in March, chosen so that %u and %w disagree (7
	// against 0), %e pads with a space, %I is not %H, and %j is not the day of
	// the month.
	at := time.Date(2026, 3, 1, 14, 5, 9, 0, time.UTC)

	for _, tc := range []struct{ format, want string }{
		{"%Y", "2026"},
		{"%y", "26"},
		{"%C", "20"},
		{"%m", "03"},
		{"%d", "01"},
		{"%e", " 1"},
		{"%j", "060"},
		{"%b", "Mar"},
		{"%h", "Mar"},
		{"%B", "March"},
		{"%a", "Sun"},
		{"%A", "Sunday"},
		{"%u", "7"},
		{"%w", "0"},
		{"%F", "2026-03-01"},
		{"%D", "03/01/26"},
		{"%H", "14"},
		{"%I", "02"},
		{"%M", "05"},
		{"%S", "09"},
		{"%p", "PM"},
		{"%P", "pm"},
		{"%R", "14:05"},
		{"%T", "14:05:09"},
		{"%Z", "UTC"},
		{"%z", "+0000"},
		{"%%", "%"},
		{"%Y/%m/%d", "2026/03/01"},
		{"/archive/%Y/%m/", "/archive/2026/03/"},
		{"nothing to expand", "nothing to expand"},
		{"", ""},
	} {
		got, err := Strftime(tc.format, at)
		if err != nil {
			t.Errorf("Strftime(%q) = %v", tc.format, err)
			continue
		}
		if got != tc.want {
			t.Errorf("Strftime(%q) = %q, want %q", tc.format, got, tc.want)
		}
	}
}

// TestStrftimeRefusesAnUnknownConversion is the decision worth a test: an
// unimplemented conversion is an error and not a passthrough.
//
// A URI format is configuration somebody typed, and emitting the two
// characters "%Q" into every URI on the site is the failure mode where nobody
// notices for a month.
func TestStrftimeRefusesAnUnknownConversion(t *testing.T) {
	at := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for _, format := range []string{"%Q", "%Y/%Q", "%", "abc%"} {
		_, err := Strftime(format, at)
		if err == nil {
			t.Errorf("Strftime(%q) was accepted; an unimplemented conversion must refuse", format)
			continue
		}
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("Strftime(%q) = %v, want it to wrap ErrInvalid", format, err)
		}
		var unknown *UnknownTokenError
		if !errors.As(err, &unknown) {
			t.Errorf("Strftime(%q) = %v, want an *UnknownTokenError naming the conversion", format, err)
		}
	}
}

// TestHasDateConversion covers the question BuildURI asks before it refuses a
// version with no cover date: does this format read the clock at all.
func TestHasDateConversion(t *testing.T) {
	for _, tc := range []struct {
		format string
		want   bool
	}{
		{TokenCategories + "/" + TokenSlug, false},
		{TokenCategories + "/%Y/" + TokenSlug, true},
		{"/archive/", false},
		{"100%% off", false},
		{"%n%t", false},
		{"%d", true},
	} {
		if got := HasDateConversion(tc.format); got != tc.want {
			t.Errorf("HasDateConversion(%q) = %t, want %t", tc.format, got, tc.want)
		}
	}
}
