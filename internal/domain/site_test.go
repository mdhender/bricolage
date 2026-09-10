// Copyright (c) 2026 Michael D Henderson.

package domain

import (
	"errors"
	"testing"
)

// TestNormalizeSiteDomain is the shape of the one value a deployment types and
// gets wrong (issue #3). It is a directory name and a host at once, so the
// table covers both kinds of mistake: the ones DNS would refuse, and the ones a
// filesystem would accept and a template lookup would then not find.
func TestNormalizeSiteDomain(t *testing.T) {
	for _, tc := range []struct {
		name  string
		in    string
		want  string
		valid bool
	}{
		{"the seeded default", "assemblage.localhost", "assemblage.localhost", true},
		{"trimmed and folded", "  WWW.Example.COM  ", "www.example.com", true},
		{"a port, because development serves on one", "assemblage.localhost:8443", "assemblage.localhost:8443", true},
		{"one label", "localhost", "localhost", true},
		{"a hyphen inside a label", "htmx-app.localhost", "htmx-app.localhost", true},
		{"an A-label", "xn--80ak6aa92e.com", "xn--80ak6aa92e.com", true},
		{"digits", "127.0.0.1", "127.0.0.1", true},

		{"empty", "", "", false},
		{"only spaces", "   ", "", false},
		{"a path separator", "example.com/features", "", false},
		{"a relative path", "../etc", "", false},
		{"a space inside", "www example com", "", false},
		{"an empty label", "www..example.com", "", false},
		{"a leading hyphen", "-example.com", "", false},
		{"a trailing hyphen", "example-.com", "", false},
		{"an underscore", "my_site.example.com", "", false},
		{"not ASCII", "exámple.com", "", false},
		{"a colon with no port", "example.com:", "", false},
		{"a port that is not a number", "example.com:https", "", false},
		{"port zero", "example.com:0", "", false},
		{"a port out of range", "example.com:99999", "", false},
		{"a port with no host", ":8443", "", false},
		{"a label too long", longLabel(MaxHostLabelLen+1) + ".com", "", false},
		{"a name too long", longName(), "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeSiteDomain(tc.in)
			switch {
			case tc.valid && err != nil:
				t.Fatalf("NormalizeSiteDomain(%q) = %v, want %q", tc.in, err, tc.want)
			case !tc.valid && err == nil:
				t.Fatalf("NormalizeSiteDomain(%q) = %q, want it refused", tc.in, got)
			case !tc.valid && !errors.Is(err, ErrInvalid):
				t.Fatalf("NormalizeSiteDomain(%q) = %v, want it to answer to ErrInvalid", tc.in, err)
			case got != tc.want:
				t.Errorf("NormalizeSiteDomain(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// longLabel is n letters, which is one label past whatever limit is being
// tested.
func longLabel(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}

// longName is a name past MaxSiteDomainLen, built from labels that are each
// legal, so that what it tests is the total and not a label.
func longName() string {
	s := ""
	for len(s) <= MaxSiteDomainLen {
		s += longLabel(60) + "."
	}
	return s + "com"
}

// TestValidateSiteName keeps the display name and the domain apart: prose is
// fine in one and refused in the other.
func TestValidateSiteName(t *testing.T) {
	if err := ValidateSiteName("The Daily Paper"); err != nil {
		t.Errorf("ValidateSiteName(prose) = %v, want it accepted", err)
	}
	for _, bad := range []string{"", "   ", "a\tname", "a\x00name", longLabel(MaxSiteNameLen + 1)} {
		if err := ValidateSiteName(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("ValidateSiteName(%q) = %v, want invalid", bad, err)
		}
	}
}
