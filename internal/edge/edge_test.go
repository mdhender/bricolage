// Copyright (c) 2026 Michael D Henderson.

package edge

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mdhender/bricolage/internal/config"
	"github.com/mdhender/bricolage/internal/domain"
)

// TestStatusFor is DESIGN.md 12's table, asserted where the table lives.
//
// Both transports read this function, so a change here changes what every
// client sees. The wrapped cases are the ones that matter in practice: errors
// reach it through several layers of %w, and a mapping that only worked on a
// bare sentinel would answer 500 for every real refusal.
func TestStatusFor(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		kind   string
	}{
		{domain.ErrUnauthenticated, http.StatusUnauthorized, "unauthenticated"},
		{domain.ErrForbidden, http.StatusForbidden, "forbidden"},
		{domain.ErrNotFound, http.StatusNotFound, "not-found"},
		{domain.ErrGuardFailed, http.StatusConflict, "guard-failed"},
		{domain.ErrConflict, http.StatusConflict, "conflict"},
		{domain.ErrInvalid, http.StatusUnprocessableEntity, "invalid"},
		{domain.ErrUnavailable, http.StatusServiceUnavailable, "unavailable"},
		{errors.New("something else"), http.StatusInternalServerError, "internal"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			status, kind, title := StatusFor(tc.err)
			if status != tc.status || kind != tc.kind {
				t.Errorf("StatusFor(%v) = %d %q, want %d %q", tc.err, status, kind, tc.status, tc.kind)
			}
			if title == "" {
				t.Error("the title is empty; it is what the error page and the problem document are headed with")
			}
			wrapped := fmt.Errorf("doing a thing: %w", tc.err)
			if got, _, _ := StatusFor(wrapped); got != tc.status {
				t.Errorf("StatusFor(wrapped %v) = %d, want %d", tc.err, got, tc.status)
			}
		})
	}
}

// TestSessionCookieIsAlwaysSecure is invariant 13 at the one place that writes
// the cookie.
//
// There is no request in sight, deliberately: the Secure flag describes the
// browser's connection, which is TLS, not the loopback hop from the proxy, so
// there is nothing about the request this function could correctly consult.
func TestSessionCookieIsAlwaysSecure(t *testing.T) {
	w := httptest.NewRecorder()
	SetSessionCookie(w, "a-token", time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))

	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("SetSessionCookie wrote %d cookies, want 1", len(cookies))
	}
	c := cookies[0]
	if c.Name != config.SessionCookieName {
		t.Errorf("name = %q, want %q", c.Name, config.SessionCookieName)
	}
	if c.Value != "a-token" {
		t.Errorf("value = %q", c.Value)
	}
	if !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie = %+v; it is always Secure, HttpOnly, and SameSite=Lax", c)
	}
	// The "__Host-" prefix is only accepted on a cookie with no Domain and
	// Path=/, so these are what make the name legal.
	if c.Path != "/" || c.Domain != "" {
		t.Errorf("Path = %q, Domain = %q; the __Host- prefix requires Path=/ and no Domain", c.Path, c.Domain)
	}
}

// TestClearSessionCookieMatchesTheOneItReplaces asserts the attributes match.
// A browser keyed the old cookie by name, path, and domain; a clear that
// differed in any of them would leave the old one beside the new.
func TestClearSessionCookieMatchesTheOneItReplaces(t *testing.T) {
	set := httptest.NewRecorder()
	SetSessionCookie(set, "a-token", time.Now())
	clear := httptest.NewRecorder()
	ClearSessionCookie(clear)

	a, b := set.Result().Cookies()[0], clear.Result().Cookies()[0]
	if a.Name != b.Name || a.Path != b.Path || a.Domain != b.Domain ||
		a.Secure != b.Secure || a.HttpOnly != b.HttpOnly || a.SameSite != b.SameSite {
		t.Errorf("clearing writes %+v, which does not match the cookie set as %+v", b, a)
	}
	if b.Value != "" || b.MaxAge >= 0 {
		t.Errorf("the cleared cookie is %+v; it carries no value and expires at once", b)
	}
}
