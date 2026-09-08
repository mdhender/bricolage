// Copyright (c) 2026 Michael D Henderson.

package config

import (
	"testing"
)

// TestValidateReturnTo is PLAN.md M2 acceptance 12.
//
// An open redirect in a route that only exists in development is still an open
// redirect, and it is the pattern that gets copied into the route that is not
// development-only. The refusals are the interesting half of this table: each
// one is a string that looks like a path to a careless parse and like another
// origin to a browser.
func TestValidateReturnTo(t *testing.T) {
	origin, err := ParsePublicOrigin("https://htmx-app.localhost:8443")
	if err != nil {
		t.Fatalf("ParsePublicOrigin: %v", err)
	}

	for _, tc := range []struct {
		name     string
		returnTo string
		ok       bool
	}{
		{"a relative path", "/documents", true},
		{"a relative path with a query", "/documents?state=review", true},
		{"a relative path with a fragment", "/documents#top", true},
		{"the root", "/", true},
		{"the same origin, absolute", "https://htmx-app.localhost:8443/documents", true},
		{"the same origin, no path", "https://htmx-app.localhost:8443", true},

		{"empty", "", false},
		{"a bare word, which a browser resolves against the current path", "documents", false},
		{"another origin", "https://evil.example.com/", false},
		{"scheme-relative, which looks like a path and is not", "//evil.example.com", false},
		{"scheme-relative with a path", "//evil.example.com/documents", false},
		{"userinfo naming one host to us and another to the browser", "https://evil.example.com@localhost", false},
		{"userinfo against the real origin", "https://evil.example.com@htmx-app.localhost:8443/", false},
		{"a backslash, which some browsers normalise to a slash", "/\\evil.example.com", false},
		{"two backslashes", "\\\\evil.example.com", false},
		{"the same host on another scheme", "http://htmx-app.localhost:8443/documents", false},
		{"the same host on another port", "https://htmx-app.localhost:9443/documents", false},
		{"a host that merely starts the same", "https://htmx-app.localhost.evil.example.com/", false},
		{"a newline, which splits a header", "/documents\nSet-Cookie: x=y", false},
		{"another scheme entirely", "javascript:alert(1)", false},
		{"a data URL", "data:text/html,<script>alert(1)</script>", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := origin.ValidateReturnTo(tc.returnTo)
			if tc.ok {
				if err != nil {
					t.Fatalf("ValidateReturnTo(%q) refused: %v", tc.returnTo, err)
				}
				if got == "" {
					t.Fatalf("ValidateReturnTo(%q) accepted but returned nothing", tc.returnTo)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateReturnTo(%q) accepted, returning %q", tc.returnTo, got)
			}
		})
	}
}

func TestParsePublicOrigin(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"the development default", DefaultPublicOrigin, DefaultPublicOrigin, true},
		{"a trailing slash is a path we tolerate", "https://example.com/", "https://example.com", true},
		{"plain http, for a deployment that terminates elsewhere", "http://example.com", "http://example.com", true},

		{"empty", "", "", false},
		{"no scheme", "example.com", "", false},
		{"no host", "https://", "", false},
		{"a path is not part of an origin", "https://example.com/app", "", false},
		{"a query is not part of an origin", "https://example.com?a=b", "", false},
		{"userinfo is not part of an origin", "https://user@example.com", "", false},
		{"another scheme", "ftp://example.com", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParsePublicOrigin(tc.in)
			if tc.ok {
				if err != nil {
					t.Fatalf("ParsePublicOrigin(%q): %v", tc.in, err)
				}
				if got.String() != tc.want {
					t.Errorf("ParsePublicOrigin(%q) = %q, want %q", tc.in, got, tc.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("ParsePublicOrigin(%q) accepted, returning %q", tc.in, got)
			}
		})
	}
}

func TestParseTrustedProxies(t *testing.T) {
	nets, err := ParseTrustedProxies(DefaultTrustedProxies)
	if err != nil {
		t.Fatalf("ParseTrustedProxies: %v", err)
	}
	if len(nets) != 2 {
		t.Fatalf("parsed %d networks, want 2", len(nets))
	}

	// A bare address is read as a single-host range, because "127.0.0.1" is
	// what somebody writes when they mean "127.0.0.1/32".
	bare, err := ParseTrustedProxies([]string{"10.0.0.1", " ", ""})
	if err != nil {
		t.Fatalf("ParseTrustedProxies: %v", err)
	}
	if len(bare) != 1 {
		t.Fatalf("parsed %d networks from one address and two blanks, want 1", len(bare))
	}
	if ones, bits := bare[0].Mask.Size(); ones != 32 || bits != 32 {
		t.Errorf("a bare IPv4 address became a /%d, want /32", ones)
	}

	if _, err := ParseTrustedProxies([]string{"not-an-address"}); err == nil {
		t.Error("ParseTrustedProxies accepted a value that is neither an address nor a CIDR block")
	}
}

// TestSessionCookieNameIsHostPrefixed is invariant 13 enforced by the browser
// rather than by us. The "__Host-" prefix is only accepted on a cookie that is
// Secure, has no Domain, and has Path=/, so a mistake in the cookie attributes
// fails visibly instead of silently shipping an insecure cookie.
func TestSessionCookieNameIsHostPrefixed(t *testing.T) {
	const prefix = "__Host-"
	if len(SessionCookieName) <= len(prefix) || SessionCookieName[:len(prefix)] != prefix {
		t.Errorf("SessionCookieName = %q, want the %q prefix", SessionCookieName, prefix)
	}
}
