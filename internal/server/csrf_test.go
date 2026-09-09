// Copyright (c) 2026 Michael D Henderson.

package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mdhender/bricolage/internal/clock"
	"github.com/mdhender/bricolage/internal/config"
	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/service"
	"github.com/mdhender/bricolage/internal/store"
)

// PLAN.md M13 acceptance 3, the three tests it asks for: a cross-origin POST
// to a cookie-authenticated route is rejected, the same request with the
// correct Origin succeeds, and a bearer-token request with no Origin at all
// succeeds.
//
// The protection is net/http.CrossOriginProtection, wrapped around the whole
// mux in withCSRF and exempting a request that carries a bearer token
// (DESIGN.md 11). The exemption is safe for exactly one reason: a browser does
// not attach a bearer token on its own, so there is no ambient credential to
// abuse -- which is the entire mechanism CSRF depends on. A cookie is ambient,
// so a cookie request is never exempt.
//
// TestCSRF in middleware_test.go asserts the same rules against a stub
// handler, which is where the wrapper's own table of cases belongs. These
// three drive the whole server instead, so that what is asserted is a real
// cookie-authenticated write being refused and a real session surviving it: a
// middleware that were correct and not wired in would pass that test and fail
// these.
//
// The requests here go through Server.Handler and not through the mux, because
// the protection is middleware: a test that drove the mux directly would prove
// that the handler works and nothing about whether anything guards it.

const csrfPassword = "correct horse battery"

// csrfHarness is a whole server -- middleware, route table, real service --
// over an in-memory database.
type csrfHarness struct {
	server *Server
	svc    *service.Service
	origin string
	token  string
	cookie *http.Cookie
}

func newCSRFHarness(t *testing.T) *csrfHarness {
	t.Helper()

	db, err := store.OpenMemory(t.Context())
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	svc, err := service.New(db, service.Options{
		Clock: clock.NewFake(time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)),
	})
	if err != nil {
		t.Fatalf("service.New: %v", err)
	}

	user, err := svc.CreateUser(t.Context(), service.NewUser{
		Email: "editor@example.com", Name: "Editor", Password: csrfPassword,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	role, err := db.CreateRole(t.Context(), "editor", "Editor")
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	if err := db.AssignRole(t.Context(), user.ID, role.ID); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}
	if _, err := db.CreateGrant(t.Context(), domain.Grant{
		RoleID: role.ID, Privilege: domain.Publish, CreatedAt: svc.Now(),
	}); err != nil {
		t.Fatalf("CreateGrant: %v", err)
	}

	origin, err := config.ParsePublicOrigin(config.DefaultPublicOrigin)
	if err != nil {
		t.Fatalf("ParsePublicOrigin: %v", err)
	}
	s, err := New(Options{
		Environment: config.Production,
		Addr:        "127.0.0.1:0",
		Service:     svc,
		Origin:      origin,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	login, err := svc.Login(t.Context(), "editor@example.com", csrfPassword, "127.0.0.1")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	return &csrfHarness{
		server: s,
		svc:    svc,
		origin: origin.String(),
		token:  login.Token,
		cookie: &http.Cookie{Name: config.SessionCookieName, Value: login.Token},
	}
}

// post drives one request through the whole middleware stack.
func (h *csrfHarness) post(t *testing.T, path string, decorate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(url.Values{}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "127.0.0.1:54321"
	decorate(req)
	rec := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(rec, req)
	return rec
}

// TestCrossOriginCookiePostIsRejected is acceptance 3's first test.
func TestCrossOriginCookiePostIsRejected(t *testing.T) {
	h := newCSRFHarness(t)

	rec := h.post(t, "/logout", func(r *http.Request) {
		r.AddCookie(h.cookie)
		r.Header.Set("Origin", "https://evil.example.com")
		// Sec-Fetch-Site is what a browser sends and what the protection
		// reads first; a form posted from another site says cross-site.
		r.Header.Set("Sec-Fetch-Site", "cross-site")
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a cross-origin cookie POST = %d, want 403", rec.Code)
	}

	// And the session survives, which is the thing the refusal protects: a
	// request that was refused must not have done half of what it asked for.
	if _, err := h.svc.Authenticate(t.Context(), h.token); err != nil {
		t.Errorf("the refused request logged the session out anyway: %v", err)
	}
}

// TestSameOriginCookiePostSucceeds is acceptance 3's second test: the same
// request with the correct Origin.
func TestSameOriginCookiePostSucceeds(t *testing.T) {
	h := newCSRFHarness(t)

	rec := h.post(t, "/logout", func(r *http.Request) {
		r.AddCookie(h.cookie)
		r.Header.Set("Origin", h.origin)
		r.Header.Set("Sec-Fetch-Site", "same-origin")
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("a same-origin cookie POST = %d, want 303: %s", rec.Code, rec.Body)
	}
	if _, err := h.svc.Authenticate(t.Context(), h.token); err == nil {
		t.Error("the session survived a sign-out that answered 303")
	}
}

// TestBearerPostWithNoOriginSucceeds is acceptance 3's third test.
//
// A request with no Origin at all is what a command-line client sends, and
// earl is one. It is exempt because it carries a bearer token: a browser does
// not attach one on its own, so there is no ambient credential for another
// site to borrow.
func TestBearerPostWithNoOriginSucceeds(t *testing.T) {
	h := newCSRFHarness(t)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/current", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("Authorization", "Bearer "+h.token)
	rec := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("a bearer request with no Origin = %d, want 204: %s", rec.Code, rec.Body)
	}
}

// TestCrossOriginBearerPostIsAlsoExempt states the exemption's shape out loud.
//
// It is the token and not the absence of an Origin that exempts a request.
// Writing it down as a test is the point: somebody reading the middleware
// might reasonably assume the check is "no Origin", and a change in that
// direction would break every browser-hosted client of the API without
// breaking a test.
func TestCrossOriginBearerPostIsAlsoExempt(t *testing.T) {
	h := newCSRFHarness(t)

	rec := h.post(t, "/api/v1/sessions/current", func(r *http.Request) {
		r.Method = http.MethodDelete
		r.Header.Set("Authorization", "Bearer "+h.token)
		r.Header.Set("Origin", "https://somewhere-else.example.com")
		r.Header.Set("Sec-Fetch-Site", "cross-site")
	})
	if rec.Code != http.StatusNoContent {
		t.Errorf("a cross-origin bearer request = %d, want 204: %s", rec.Code, rec.Body)
	}
}
