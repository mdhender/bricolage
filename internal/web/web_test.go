// Copyright (c) 2026 Michael D Henderson.

package web

import (
	"go/build"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mdhender/bricolage/internal/clock"
	"github.com/mdhender/bricolage/internal/config"
	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/ids"
	"github.com/mdhender/bricolage/internal/service"
	"github.com/mdhender/bricolage/internal/store"
)

const password = "correct horse battery"

var start = time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)

// storySchema is the schema "cmsdb seed" writes.
const storySchema = `{"fields":[{"name":"body","type":"block"},{"name":"deck","type":"text"},` +
	`{"name":"related","type":"document","repeatable":true}]}`

// harness is the UI over a real in-memory store with a fake clock. The
// transport is what is under test, so everything below it is real: a mocked
// service would prove that the mock returns what the mock was told to.
type harness struct {
	mux   *http.ServeMux
	svc   *service.Service
	db    *store.DB
	clock *clock.Fake
}

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
	return &harness{mux: mux, svc: svc, db: db, clock: c}
}

// seeded adds the one site and the one element type every document needs.
func (h *harness) seeded(t *testing.T) *harness {
	t.Helper()
	if _, err := h.db.CreateElementType(t.Context(), store.NewElementType{
		UID: ids.MustNew(start), KeyName: "story", Name: "Story",
		Kind: domain.KindStory, TopLevel: true, Schema: storySchema, CreatedAt: start,
	}); err != nil {
		t.Fatalf("CreateElementType: %v", err)
	}
	if _, err := h.db.CreateSite(t.Context(), store.NewSite{
		UID: ids.MustNew(start), Name: "Default", Domain: "example.com",
	}); err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	return h
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
	if p != domain.NoPrivilege {
		if _, err := h.db.CreateGrant(t.Context(), domain.Grant{
			RoleID: role.ID, Privilege: p, CreatedAt: start,
		}); err != nil {
			t.Fatalf("CreateGrant: %v", err)
		}
	}
	return u
}

// session logs in and returns the cookie a browser would then send.
func (h *harness) session(t *testing.T, email string) *http.Cookie {
	t.Helper()
	body := url.Values{"email": {email}, "password": {password}}
	w := h.post(t, "/login", nil, body)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("POST /login = %d, want 303: %s", w.Code, w.Body)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == config.SessionCookieName {
			return c
		}
	}
	t.Fatalf("POST /login set no %s cookie", config.SessionCookieName)
	return nil
}

// get performs a GET, with the session cookie when there is one.
func (h *harness) get(t *testing.T, path string, session *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if session != nil {
		req.AddCookie(session)
	}
	w := httptest.NewRecorder()
	h.mux.ServeHTTP(w, req)
	return w
}

// post performs a form POST.
func (h *harness) post(t *testing.T, path string, session *http.Cookie, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if session != nil {
		req.AddCookie(session)
	}
	w := httptest.NewRecorder()
	h.mux.ServeHTTP(w, req)
	return w
}

// postHX performs a form POST the way HTMX does, which is what asks a handler
// for a fragment instead of a redirect.
func (h *harness) postHX(t *testing.T, path string, session *http.Cookie, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	if session != nil {
		req.AddCookie(session)
	}
	w := httptest.NewRecorder()
	h.mux.ServeHTTP(w, req)
	return w
}

// document creates one through the UI and returns its uid.
//
// The session it is given is whoever creates it, which is not always whoever
// the test then looks at it as: creating needs Create, and a bar with a
// refusal in it needs somebody holding less than that.
func (h *harness) document(t *testing.T, session *http.Cookie, title string) string {
	t.Helper()
	w := h.post(t, "/documents", session, url.Values{
		"site": {"1"}, "kind": {"story"}, "element_type": {"story"},
		"title": {title}, "slug": {"quick-brown-fox"},
		"field.body": {"The quick brown fox."},
	})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("POST /documents = %d, want 303: %s", w.Code, w.Body)
	}
	to := w.Header().Get("Location")
	uid := strings.TrimPrefix(strings.SplitN(to, "?", 2)[0], "/documents/")
	if uid == "" || uid == to {
		t.Fatalf("POST /documents redirected to %q, which names no document", to)
	}
	return uid
}

// TestNoStoreImport is PLAN.md M13 acceptance 1.
//
// The handlers call service methods and nothing below them. It is asserted on
// the package's imports rather than by reading the code, because the rule is
// about the dependency and not about a spelling: a handler that reached the
// store through a helper in another file would still have to import it.
func TestNoStoreImport(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatalf("reading this package: %v", err)
	}
	forbidden := map[string]string{
		"github.com/mdhender/bricolage/internal/store":   "a transport does not query (DESIGN.md 3, PLAN.md M13 acceptance 1)",
		"github.com/mdhender/bricolage/internal/migrate": "a transport does not migrate",
		"database/sql":            "all SQL lives in internal/store (invariant 2)",
		"zombiezen.com/go/sqlite": "all SQL lives in internal/store (invariant 2)",
	}
	for _, imported := range pkg.Imports {
		if why, bad := forbidden[imported]; bad {
			t.Errorf("internal/web imports %s: %s", imported, why)
		}
	}
}

// TestSignedOutBrowserIsSentToTheForm asserts the two answers a signed-out
// caller gets: a page redirects to the form, a write refuses.
func TestSignedOutBrowserIsSentToTheForm(t *testing.T) {
	h := newHarness(t)

	w := h.get(t, "/documents", nil)
	if w.Code != http.StatusSeeOther {
		t.Errorf("GET /documents signed out = %d, want 303", w.Code)
	}
	if to := w.Header().Get("Location"); !strings.HasPrefix(to, "/login?returnTo=") {
		t.Errorf("GET /documents signed out redirected to %q, which forgets where they were going", to)
	}

	// A POST is not redirected: a redirect would discard what was typed and
	// land somewhere that looks like it worked.
	if got := h.post(t, "/documents", nil, url.Values{}).Code; got != http.StatusUnauthorized {
		t.Errorf("POST /documents signed out = %d, want 401", got)
	}
}

// TestLoginWritesTheOrdinarySessionCookie is invariant 13 at this transport.
//
// The UI's login is the same service call the JSON route makes and writes the
// same cookie, through internal/edge. There is one cookie-writing path in the
// process, and this asserts that the UI is on it.
func TestLoginWritesTheOrdinarySessionCookie(t *testing.T) {
	h := newHarness(t)
	h.user(t, "editor@example.com", domain.Read)

	c := h.session(t, "editor@example.com")
	if !c.Secure {
		t.Error("the session cookie is not Secure, over a connection with no TLS -- which is the whole point (invariant 13)")
	}
	if !c.HttpOnly {
		t.Error("the session cookie is not HttpOnly")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", c.SameSite)
	}
	if c.Path != "/" || c.Domain != "" {
		t.Errorf("Path = %q, Domain = %q; the __Host- prefix requires Path=/ and no Domain", c.Path, c.Domain)
	}
}

// TestLoginRefusesAnOffOriginReturnTo is the open redirect this form would
// otherwise be. The refusals are config.PublicOrigin's, and the point of the
// test is that the UI asks it rather than trusting the parameter.
func TestLoginRefusesAnOffOriginReturnTo(t *testing.T) {
	h := newHarness(t)
	h.user(t, "editor@example.com", domain.Read)

	for _, returnTo := range []string{
		"//evil.example.com/",
		"https://evil.example.com/",
		"/\\evil.example.com",
	} {
		w := h.post(t, "/login", nil, url.Values{
			"email": {"editor@example.com"}, "password": {password}, "returnTo": {returnTo},
		})
		if got := w.Header().Get("Location"); got != "/" {
			t.Errorf("returnTo %q redirected to %q, want the dashboard", returnTo, got)
		}
	}
}

// TestBadPasswordRedrawsTheForm asserts that a refused sign-in answers 401 and
// says nothing about which half was wrong.
func TestBadPasswordRedrawsTheForm(t *testing.T) {
	h := newHarness(t)
	h.user(t, "editor@example.com", domain.Read)

	w := h.post(t, "/login", nil, url.Values{
		"email": {"editor@example.com"}, "password": {"wrong"},
	})
	if w.Code != http.StatusUnauthorized {
		t.Errorf("POST /login with a bad password = %d, want 401", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "do not match an account") {
		t.Errorf("the refusal does not redraw the form: %s", body)
	}
	if len(w.Result().Cookies()) != 0 {
		t.Error("a refused sign-in set a cookie")
	}
}
