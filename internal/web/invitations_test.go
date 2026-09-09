// Copyright (c) 2026 Michael D Henderson.

package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/mdhender/bricolage/internal/config"
	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/service"
)

// The invitation screens (issue #6). What is under test here and not in
// internal/api is the browser's half: that the redemption page renders without
// a session, that redeeming issues no cookie, and that the link is shown on the
// page that created it and on no other.

// invite creates an invitation through the service and returns it, which is the
// setup for the screens rather than the thing being tested.
func (h *harness) invitation(t *testing.T, email string) service.CreatedInvitation {
	t.Helper()
	admin := h.identity(t, "admin@example.com")
	created, err := h.svc.CreateInvitation(t.Context(), admin, service.NewInvitation{Email: email})
	if err != nil {
		t.Fatalf("CreateInvitation: %v", err)
	}
	return created
}

// identity resolves a user to the identity the service takes.
func (h *harness) identity(t *testing.T, email string) domain.Identity {
	t.Helper()
	u, err := h.db.UserByEmail(t.Context(), email)
	if err != nil {
		t.Fatalf("UserByEmail: %v", err)
	}
	id, err := h.db.Identity(t.Context(), u.ID)
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	return id
}

// TestRedemptionScreenNeedsNoSession is what makes the whole flow possible: the
// page a link lands on is one of the two in this package that render for a
// browser with no cookie.
func TestRedemptionScreenNeedsNoSession(t *testing.T) {
	h := newHarness(t)
	h.user(t, "admin@example.com", domain.Create)
	created := h.invitation(t, "rose@example.com")

	w := h.get(t, config.InvitationPath+created.Token, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET the invitation link with no session = %d, want 200: %s", w.Code, w.Body)
	}
	body := w.Body.String()
	if !strings.Contains(body, `name="token"`) {
		t.Error("the form carries no token field; the POST must not put the credential in a second URL")
	}
	for _, field := range []string{`name="email"`, `name="name"`, `name="password"`, `name="confirm"`} {
		if !strings.Contains(body, field) {
			t.Errorf("the form has no %s", field)
		}
	}

	// A link nobody minted renders the same page. A 404 here would say whether
	// a token exists, which is the question this flow exists not to answer.
	w = h.get(t, config.InvitationPath+"not-a-real-token", nil)
	if w.Code != http.StatusOK {
		t.Errorf("GET an unknown invitation link = %d, want 200; a page that differed would say whether the token exists", w.Code)
	}
}

// TestRedemptionIssuesNoSession is issue #6's security decision, asserted the
// only way it can be: by looking for the cookie that must not be there.
func TestRedemptionIssuesNoSession(t *testing.T) {
	h := newHarness(t)
	h.user(t, "admin@example.com", domain.Create)
	created := h.invitation(t, "rose@example.com")

	w := h.post(t, "/invite", nil, url.Values{
		"token":    {created.Token},
		"email":    {"rose@example.com"},
		"name":     {"Rose Shapiro"},
		"password": {password},
		"confirm":  {password},
	})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("POST /invite = %d, want 303: %s", w.Code, w.Body)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == config.SessionCookieName {
			t.Fatal("redemption set a session cookie; ending at the login form with no session is what keeps this route free of login CSRF (issue #6)")
		}
	}
	if to := w.Header().Get("Location"); !strings.HasPrefix(to, "/login") {
		t.Errorf("redemption redirected to %q, want the login form", to)
	}

	// The account exists and the password works.
	if h.session(t, "rose@example.com") == nil {
		t.Error("the account cannot sign in with the password redemption set")
	}
}

// TestRedemptionRefusalsAreOneSentence is the anti-oracle rule as the browser
// sees it: the page comes back with the same message whichever of the causes it
// was.
func TestRedemptionRefusalsAreOneSentence(t *testing.T) {
	h := newHarness(t)
	h.user(t, "admin@example.com", domain.Create)
	created := h.invitation(t, "rose@example.com")

	bodies := map[string]url.Values{
		"an unknown token": {
			"token": {"nothing-like-a-token"}, "email": {"rose@example.com"},
			"name": {"Rose"}, "password": {password}, "confirm": {password},
		},
		"the wrong address": {
			"token": {created.Token}, "email": {"someone.else@example.com"},
			"name": {"Rose"}, "password": {password}, "confirm": {password},
		},
	}

	// The pages are compared by the sentence they show and not byte for byte:
	// a redrawn form echoes the address and the token back, so two attempts
	// that typed different things render different HTML for a reason that
	// tells an attacker only what they already knew. What must not differ is
	// what the server says about them.
	seen := map[string]string{}
	for name, form := range bodies {
		w := h.post(t, "/invite", nil, form)
		if w.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s answered %d, want 422", name, w.Code)
		}
		seen[name] = problemSentence(t, w.Body.String())
	}
	if a, b := seen["an unknown token"], seen["the wrong address"]; a != b {
		t.Errorf("an unknown token says %q and the wrong address says %q; the two are distinguishable, which makes the form an oracle for who was invited", a, b)
	}
	if s := seen["an unknown token"]; s == "" {
		t.Error("a refused redemption said nothing at all; the person needs one sentence, and one sentence is all they get")
	}

	// The password rules are a different matter: whoever is reading has already
	// shown they hold the link and know the address, so they may be told.
	w := h.post(t, "/invite", nil, url.Values{
		"token": {created.Token}, "email": {"rose@example.com"},
		"name": {"Rose"}, "password": {password}, "confirm": {"something else"},
	})
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("mismatched passwords answered %d, want 422", w.Code)
	}
	if !strings.Contains(w.Body.String(), "not the same") {
		t.Error("mismatched passwords did not say so; a person who cannot be told what is wrong cannot fix it")
	}
}

// TestInvitationLinkIsShownOnce is the criterion about the link: it appears on
// the page that created it, with something to copy it, and on no page after.
func TestInvitationLinkIsShownOnce(t *testing.T) {
	h := newHarness(t)
	h.user(t, "admin@example.com", domain.Create)
	session := h.session(t, "admin@example.com")

	w := h.post(t, "/admin/invitations", session, url.Values{"email": {"rose@example.com"}})
	if w.Code != http.StatusCreated {
		t.Fatalf("POST /admin/invitations = %d, want 201: %s", w.Code, w.Body)
	}
	body := w.Body.String()
	if !strings.Contains(body, config.InvitationPath) {
		t.Fatal("the page that created the invitation does not show the link")
	}
	if !strings.Contains(body, "data-copy=") {
		t.Error("the link has no copy affordance; 43 characters of base64 selected by hand is how an invitation gets reported as broken")
	}

	// Reloading the list shows the invitation and not the link.
	w = h.get(t, "/admin/invitations", session)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /admin/invitations = %d: %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), config.InvitationPath) {
		t.Error("the listing shows a link; it is returned once, at creation, and what is stored is a hash so there is nothing to show again")
	}
	if !strings.Contains(w.Body.String(), "rose@example.com") {
		t.Error("the listing does not show the invitation")
	}
}

// TestInvitationScreensNeedTheAdminPrivilege is the authorization rule at the
// screen, which is the service's rule reaching the browser unchanged.
func TestInvitationScreensNeedTheAdminPrivilege(t *testing.T) {
	h := newHarness(t)
	h.user(t, "editor@example.com", domain.Edit)
	session := h.session(t, "editor@example.com")

	if w := h.get(t, "/admin/invitations", session); w.Code != http.StatusForbidden {
		t.Errorf("an editor opening the invitation screen = %d, want 403", w.Code)
	}
	if w := h.post(t, "/admin/invitations", session,
		url.Values{"email": {"nope@example.com"}}); w.Code != http.StatusForbidden {
		t.Errorf("an editor creating an invitation = %d, want 403", w.Code)
	}
	// The user list is read rather than create, so an editor may open it: they
	// need a uid to hand work over.
	if w := h.get(t, "/admin/users", session); w.Code != http.StatusOK {
		t.Errorf("an editor opening the user list = %d, want 200: %s", w.Code, w.Body)
	}
}

// problemSentence pulls the one sentence a refused form shows, so that two
// refusals can be compared on what the server said rather than on what the
// person typed.
func problemSentence(t *testing.T, body string) string {
	t.Helper()
	const open = `<p class="problem">`
	i := strings.Index(body, open)
	if i < 0 {
		return ""
	}
	rest := body[i+len(open):]
	j := strings.Index(rest, "</p>")
	if j < 0 {
		t.Fatalf("the problem paragraph is not closed: %s", rest)
	}
	return rest[:j]
}
