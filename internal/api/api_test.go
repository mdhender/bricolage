// Copyright (c) 2026 Michael D Henderson.

package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mdhender/bricolage/internal/clock"
	"github.com/mdhender/bricolage/internal/config"
	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/service"
	"github.com/mdhender/bricolage/internal/store"
)

const password = "correct horse battery"

// harness is the API over a real in-memory store with a fake clock, served by
// httptest. The transport is what is under test, so everything below it is
// real: a mocked service would prove that the mock returns what the mock was
// told to.
type harness struct {
	mux   *http.ServeMux
	svc   *service.Service
	clock *clock.Fake
	db    *store.DB
}

var start = time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)

func newHarness(t *testing.T) *harness {
	t.Helper()

	db, err := store.OpenMemory(t.Context())
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	c := clock.NewFake(start)
	svc, err := service.New(db, service.Options{Clock: c})
	if err != nil {
		t.Fatalf("service.New: %v", err)
	}

	mux := http.NewServeMux()
	Register(mux, Deps{Service: svc, Environment: config.Production})

	return &harness{mux: mux, svc: svc, clock: c, db: db}
}

// user creates a user with a password and one global grant.
func (h *harness) user(t *testing.T, email string, p domain.Privilege) domain.User {
	t.Helper()
	u, err := h.svc.CreateUser(t.Context(), service.NewUser{Email: email, Name: "Test User", Password: password})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	role, err := h.db.CreateRole(t.Context(), email, "Role for "+email)
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	if err := h.db.AssignRole(t.Context(), u.ID, role.ID); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}
	if _, err := h.db.CreateGrant(t.Context(), domain.Grant{
		RoleID: role.ID, Privilege: p, CreatedAt: start,
	}); err != nil {
		t.Fatalf("CreateGrant: %v", err)
	}
	return u
}

// do performs a request against the mux. The body is JSON when it is not nil.
func (h *harness) do(t *testing.T, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = strings.NewReader(string(encoded))
	}

	var req *http.Request
	if reader != nil {
		req = httptest.NewRequest(method, path, reader)
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	w := httptest.NewRecorder()
	h.mux.ServeHTTP(w, req)
	return w
}

// login returns a token for email.
func (h *harness) login(t *testing.T, email string) string {
	t.Helper()
	w := h.do(t, http.MethodPost, "/api/v1/sessions", "", map[string]string{
		"email": email, "password": password,
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("login: %d %s", w.Code, w.Body)
	}
	var resp sessionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp.Token
}

// TestSessionCookieIsAlwaysSecure is PLAN.md M2 acceptance 4 and invariant 13.
//
// httptest speaks plain HTTP, so r.TLS is nil on every request here -- which is
// exactly the situation behind the proxy, and exactly the situation in which an
// "if r.TLS != nil" guard would ship an insecure cookie while looking careful.
// The attribute is asserted directly.
func TestSessionCookieIsAlwaysSecure(t *testing.T) {
	h := newHarness(t)
	h.user(t, "admin@example.com", domain.Publish)

	w := h.do(t, http.MethodPost, "/api/v1/sessions", "", map[string]string{
		"email": "admin@example.com", "password": password,
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("login: %d %s", w.Code, w.Body)
	}

	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("the response set %d cookies, want 1", len(cookies))
	}
	c := cookies[0]
	if c.Name != config.SessionCookieName {
		t.Errorf("cookie name = %q, want %q", c.Name, config.SessionCookieName)
	}
	if !c.Secure {
		t.Error("the session cookie is not Secure, over a connection with no TLS -- which is the whole point (invariant 13)")
	}
	if !c.HttpOnly {
		t.Error("the session cookie is not HttpOnly")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", c.SameSite)
	}
	// The "__Host-" prefix is only accepted on a cookie with no Domain and
	// Path=/, so these are what make the name legal.
	if c.Domain != "" {
		t.Errorf("Domain = %q, want empty", c.Domain)
	}
	if c.Path != "/" {
		t.Errorf("Path = %q, want \"/\"", c.Path)
	}
}

// TestNoTLSGuardAnywhere is invariant 13 as a source check.
//
// The test above passes whether or not the guard is there, because httptest
// leaves the request's TLS field nil either way. What the design forbids is
// the expression itself: behind the proxy it is always false, so code that
// consults it ships insecure cookies while looking careful.
//
// The tree is parsed rather than grepped, so that a comment explaining the
// rule -- like the one on SetSessionCookie -- does not trip the test that
// enforces it. What is searched for is a selector reading the field, in code.
func TestNoTLSGuardAnywhere(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".github", "deploy", "docs", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "TLS" {
				return true
			}
			// http.Request is the only thing in this repository with a TLS
			// field, and cmsd never terminates TLS (invariant 15), so any
			// read of one is the guard this forbids.
			t.Errorf("%s reads a TLS field; the Secure flag describes the browser's connection, not the loopback hop, and that expression is always false behind the proxy (invariant 13)",
				fset.Position(sel.Pos()))
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking the tree: %v", err)
	}
}

// TestMeRequiresACredential is PLAN.md M2 acceptance 8 at the transport edge.
func TestMeRequiresACredential(t *testing.T) {
	h := newHarness(t)
	h.user(t, "admin@example.com", domain.Publish)

	for _, tc := range []struct {
		name  string
		token string
	}{
		{"no credential", ""},
		{"a token that names nothing", "not-a-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := h.do(t, http.MethodGet, "/api/v1/me", tc.token, nil)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401\n%s", w.Code, w.Body)
			}
			if got := w.Header().Get("WWW-Authenticate"); got == "" {
				t.Error("a 401 with no WWW-Authenticate header")
			}
			assertProblem(t, w, http.StatusUnauthorized, "unauthenticated")
		})
	}

	t.Run("an expired session", func(t *testing.T) {
		token := h.login(t, "admin@example.com")
		h.clock.Advance(config.DefaultSessionTTL)

		w := h.do(t, http.MethodGet, "/api/v1/me", token, nil)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401\n%s", w.Code, w.Body)
		}
		assertProblem(t, w, http.StatusUnauthorized, "unauthenticated")
	})
}

// TestMe covers what the document carries, which is what makes "no elevation"
// checkable from outside the process.
func TestMe(t *testing.T) {
	h := newHarness(t)
	u := h.user(t, "admin@example.com", domain.Publish)
	token := h.login(t, "admin@example.com")

	w := h.do(t, http.MethodGet, "/api/v1/me", token, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", w.Code, w.Body)
	}

	var me meResponse
	if err := json.Unmarshal(w.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if me.User.UID != u.UID || me.User.Email != u.Email {
		t.Errorf("me = %+v, want %s <%s>", me.User, u.UID, u.Email)
	}
	if len(me.Roles) != 1 || len(me.Grants) != 1 {
		t.Fatalf("roles = %d and grants = %d, want 1 and 1", len(me.Roles), len(me.Grants))
	}
	if me.Grants[0].Privilege != "publish" || me.Grants[0].Description != "everything" {
		t.Errorf("grant = %+v", me.Grants[0])
	}

	// Invariant 10: the integer key never appears in a response.
	if strings.Contains(w.Body.String(), `"id"`) {
		t.Errorf("the response carries an \"id\" field; the API speaks uid only:\n%s", w.Body)
	}
	// And the hash never leaves the store.
	if strings.Contains(w.Body.String(), "password") {
		t.Errorf("the response mentions a password:\n%s", w.Body)
	}
}

func TestLogout(t *testing.T) {
	h := newHarness(t)
	h.user(t, "admin@example.com", domain.Read)
	token := h.login(t, "admin@example.com")

	w := h.do(t, http.MethodDelete, "/api/v1/sessions/current", token, nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204\n%s", w.Code, w.Body)
	}
	// The cookie is cleared with the attributes it was set with, or the
	// browser keeps the old one beside the new.
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].MaxAge >= 0 || !cookies[0].Secure {
		t.Errorf("logout set %+v, want an expired, Secure cookie", cookies)
	}

	if w := h.do(t, http.MethodGet, "/api/v1/me", token, nil); w.Code != http.StatusUnauthorized {
		t.Errorf("the token still works after logout: %d", w.Code)
	}
}

// TestCreateGrantIsForbiddenForAnEscalation is PLAN.md M2 acceptance 7 at the
// transport edge: the attempt returns 403.
func TestCreateGrantIsForbiddenForAnEscalation(t *testing.T) {
	h := newHarness(t)
	h.user(t, "editor@example.com", domain.Edit)
	if _, err := h.db.CreateRole(t.Context(), "target", "Target"); err != nil {
		t.Fatal(err)
	}
	token := h.login(t, "editor@example.com")

	w := h.do(t, http.MethodPost, "/api/v1/grants", token, map[string]any{
		"role": "target", "privilege": "publish",
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403\n%s", w.Code, w.Body)
	}
	assertProblem(t, w, http.StatusForbidden, "forbidden")

	// Nothing was written.
	role, err := h.db.RoleBySlug(t.Context(), "target")
	if err != nil {
		t.Fatal(err)
	}
	grants, err := h.db.GrantsForRole(t.Context(), role.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 0 {
		t.Errorf("the refused request wrote %d grants", len(grants))
	}

	// The privilege the caller does hold is accepted, so the refusal is the
	// rule and not a broken route.
	w = h.do(t, http.MethodPost, "/api/v1/grants", token, map[string]any{
		"role": "target", "privilege": "edit",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201\n%s", w.Code, w.Body)
	}
}

// TestStatusFor is the mapping table from DESIGN.md 12, asserted directly. It
// is the one function that turns an error into a status, so it is the one
// place the table can be wrong.
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
		{errors.New("something else"), http.StatusInternalServerError, "internal"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			status, kind, title := statusFor(tc.err)
			if status != tc.status || kind != tc.kind {
				t.Errorf("statusFor(%v) = %d %q, want %d %q", tc.err, status, kind, tc.status, tc.kind)
			}
			if title == "" {
				t.Error("no title")
			}
			// Wrapping must not change the answer: everything below the edge
			// wraps with %w and this is inspected with errors.Is.
			wrapped := errors.Join(errors.New("context"), tc.err)
			if got, _, _ := statusFor(wrapped); got != tc.status {
				t.Errorf("a wrapped error mapped to %d, want %d", got, tc.status)
			}
		})
	}
}

// TestMalformedBody is the 422 half of the table.
func TestMalformedBody(t *testing.T) {
	h := newHarness(t)

	for _, tc := range []struct {
		name string
		body string
	}{
		{"not JSON", `{`},
		{"an unknown field", `{"email":"a@b.c","password":"x","admin":true}`},
		{"two values", `{"email":"a@b.c","password":"x"}{"email":"a@b.c"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h.mux.ServeHTTP(w, req)

			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422\n%s", w.Code, w.Body)
			}
			assertProblem(t, w, http.StatusUnprocessableEntity, "invalid")
		})
	}
}

// TestProductionHidesTheDetail is DESIGN.md 14: in production an unexpected
// error is generic with a request id, because an error message is a disclosure
// channel; in development it is the underlying detail, because the person
// reading it is the person who caused it.
func TestProductionHidesTheDetail(t *testing.T) {
	for _, tc := range []struct {
		env        config.Environment
		wantDetail bool
	}{
		{config.Production, false},
		{config.Development, true},
	} {
		t.Run(string(tc.env), func(t *testing.T) {
			h := New(Deps{Environment: tc.env})
			req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
			w := httptest.NewRecorder()
			h.writeError(w, req, errors.New("the connection to the widget farm failed"))

			if w.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500", w.Code)
			}
			body := w.Body.String()
			if got := strings.Contains(body, "widget farm"); got != tc.wantDetail {
				t.Errorf("detail disclosed = %t, want %t\n%s", got, tc.wantDetail, body)
			}
		})
	}
}

// assertProblem checks that the response is an RFC 9457 problem document of
// the expected kind.
func assertProblem(t *testing.T, w *httptest.ResponseRecorder, status int, kind string) {
	t.Helper()
	if ct := w.Header().Get("Content-Type"); ct != ContentTypeProblem {
		t.Errorf("Content-Type = %q, want %q", ct, ContentTypeProblem)
	}
	var p Problem
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("the body is not a problem document: %v\n%s", err, w.Body)
	}
	if p.Status != status {
		t.Errorf("problem status = %d, want %d", p.Status, status)
	}
	if want := problemBase + kind; p.Type != want {
		t.Errorf("problem type = %q, want %q", p.Type, want)
	}
	if p.Title == "" {
		t.Error("the problem document has no title")
	}
}
