// Copyright (c) 2026 Michael D Henderson.

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mdhender/bricolage/internal/domain"
)

// The invitation routes, against issue #6's acceptance criteria.
//
// The criterion that needs the most care is "all four are indistinguishable
// from outside", and it is asserted as one test over every failure rather than
// as a line in each: a refusal that differs in its status, its kind, or its
// detail is an oracle, and the way to notice one is to compare them with each
// other.

// invite creates an invitation and returns the response, failing the test if
// the server refused.
func (h *harness) invite(t *testing.T, token, email string) createdInvitationResponse {
	t.Helper()
	w := h.do(t, http.MethodPost, "/api/v1/invitations", token, map[string]string{"email": email})
	if w.Code != http.StatusCreated {
		t.Fatalf("invite %s: %d %s", email, w.Code, w.Body)
	}
	var out createdInvitationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// redeem posts a redemption. It takes no token: the route is unauthenticated,
// which is the point of it.
func (h *harness) redeem(t *testing.T, body map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	return h.do(t, http.MethodPost, "/api/v1/invitations/redemption", "", body)
}

// TestInvitationRoundTrip is the first three acceptance criteria: an
// administrator invites an address, the link redeems exactly once, and no
// session comes of it.
func TestInvitationRoundTrip(t *testing.T) {
	h := newHarness(t)
	h.user(t, "admin@example.com", domain.Create)
	admin := h.login(t, "admin@example.com")

	created := h.invite(t, admin, "rose@example.com")
	if created.Token == "" {
		t.Fatal("the response carried no token; the link is returned once and this was the once")
	}
	if created.Link == "" || created.Link[0] == '/' {
		t.Errorf("link = %q, want an absolute URL somebody can be sent", created.Link)
	}
	if created.Invitation.Status != string(domain.InvitationPending) {
		t.Errorf("status = %q, want pending", created.Invitation.Status)
	}
	if !created.Invitation.Redeemable {
		t.Error("a new invitation is not redeemable")
	}

	// Reading it back never shows the link again, because what is stored is a
	// hash and there is nothing to show.
	w := h.do(t, http.MethodGet, "/api/v1/invitations/"+created.Invitation.UID, admin, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("show: %d %s", w.Code, w.Body)
	}
	if body := w.Body.String(); strings.Contains(body, created.Token) {
		t.Error("GET /invitations/{uid} returned the token; it is returned once, at creation, and never again")
	}

	// Redeem it.
	w = h.redeem(t, map[string]string{
		"token": created.Token, "email": "rose@example.com",
		"name": "Rose Shapiro", "password": password,
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("redeem: %d %s", w.Code, w.Body)
	}
	if cookies := w.Result().Cookies(); len(cookies) != 0 {
		t.Errorf("redemption set %d cookies; it issues no session, which is what keeps the route free of login CSRF", len(cookies))
	}
	var u userResponse
	if err := json.Unmarshal(w.Body.Bytes(), &u); err != nil {
		t.Fatal(err)
	}
	if u.Email != "rose@example.com" || u.Name != "Rose Shapiro" {
		t.Errorf("created %+v, want rose@example.com / Rose Shapiro", u)
	}

	// The password she set is the password that works.
	if token := h.login(t, "rose@example.com"); token == "" {
		t.Error("the account cannot sign in with the password redemption set")
	}

	// The invitation is settled and names the account.
	w = h.do(t, http.MethodGet, "/api/v1/invitations/"+created.Invitation.UID, admin, nil)
	var inv invitationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &inv); err != nil {
		t.Fatal(err)
	}
	if inv.Status != string(domain.InvitationRedeemed) {
		t.Errorf("status = %q, want redeemed", inv.Status)
	}
	if inv.User != u.UID {
		t.Errorf("invitation names account %q, want %q", inv.User, u.UID)
	}
	if inv.Redeemable {
		t.Error("a redeemed invitation still reports itself redeemable")
	}
}

// TestEveryRedemptionFailureLooksTheSame is issue #6's third acceptance
// criterion, and the reason it is one test: the four refusals have to be
// indistinguishable from each other, which is not something four separate
// assertions can check.
func TestEveryRedemptionFailureLooksTheSame(t *testing.T) {
	h := newHarness(t)
	h.user(t, "admin@example.com", domain.Create)
	admin := h.login(t, "admin@example.com")

	// Four invitations, each driven into one of the four failing states.
	used := h.invite(t, admin, "used@example.com")
	if w := h.redeem(t, map[string]string{
		"token": used.Token, "email": "used@example.com",
		"name": "Used Once", "password": password,
	}); w.Code != http.StatusCreated {
		t.Fatalf("setting up the used invitation: %d %s", w.Code, w.Body)
	}

	wrongEmail := h.invite(t, admin, "wrong@example.com")

	revoked := h.invite(t, admin, "revoked@example.com")
	if w := h.do(t, http.MethodPost,
		"/api/v1/invitations/"+revoked.Invitation.UID+"/revoke", admin, nil); w.Code != http.StatusOK {
		t.Fatalf("revoking: %d %s", w.Code, w.Body)
	}

	lapsed := h.invite(t, admin, "lapsed@example.com")

	attempts := []struct {
		name string
		body map[string]string
		// lapse advances the clock past the 48 hours before the attempt.
		lapse bool
	}{
		{"an unknown token", map[string]string{
			"token": "not-a-token-anybody-minted", "email": "stranger@example.com",
			"name": "Nobody", "password": password,
		}, false},
		{"a second redemption", map[string]string{
			"token": used.Token, "email": "used@example.com",
			"name": "Used Twice", "password": password,
		}, false},
		{"the wrong address", map[string]string{
			"token": wrongEmail.Token, "email": "someone.else@example.com",
			"name": "Forwarded", "password": password,
		}, false},
		{"a revoked invitation", map[string]string{
			"token": revoked.Token, "email": "revoked@example.com",
			"name": "Revoked", "password": password,
		}, false},
		{"an invitation past 48 hours", map[string]string{
			"token": lapsed.Token, "email": "lapsed@example.com",
			"name": "Late", "password": password,
		}, true},
	}

	type answer struct {
		status int
		body   string
	}
	answers := make(map[string]answer, len(attempts))
	for _, a := range attempts {
		if a.lapse {
			h.clock.Advance(domain.InvitationTTL + time.Minute)
		}
		w := h.redeem(t, a.body)
		if w.Code == http.StatusCreated {
			t.Fatalf("%s was accepted", a.name)
		}
		answers[a.name] = answer{status: w.Code, body: w.Body.String()}
	}

	// Every one of them, compared with the first.
	var first string
	for name := range answers {
		if first == "" || name < first {
			first = name
		}
	}
	want := answers[first]
	if want.status != http.StatusUnprocessableEntity {
		t.Errorf("%s answered %d, want 422", first, want.status)
	}
	for name, got := range answers {
		if got.status != want.status {
			t.Errorf("%s answered %d and %s answered %d; the two are distinguishable from outside, which makes this route an oracle for who was invited",
				name, got.status, first, want.status)
		}
		if got.body != want.body {
			t.Errorf("%s answered %s and %s answered %s; the bodies differ, so the refusals are distinguishable",
				name, got.body, first, want.body)
		}
	}
}

// TestReinvitingSupersedes is the criterion that a lapsed invitation does not
// block its address, and it is the one that would fail against the partial
// unique index alone: a lapsed row is still pending, so the index would refuse
// the second invitation for ever.
func TestReinvitingSupersedes(t *testing.T) {
	h := newHarness(t)
	h.user(t, "admin@example.com", domain.Create)
	admin := h.login(t, "admin@example.com")

	first := h.invite(t, admin, "twice@example.com")

	// Let it lapse, which writes nothing: expiry is derived. Two days is also
	// longer than a session lasts, so the administrator signs in again -- which
	// is what would actually have happened.
	h.clock.Advance(domain.InvitationTTL + time.Minute)
	admin = h.login(t, "admin@example.com")

	second := h.invite(t, admin, "twice@example.com")
	if second.Invitation.UID == first.Invitation.UID {
		t.Fatal("the second invitation reused the first row; re-inviting creates a new one and supersedes the old")
	}
	if second.Token == first.Token {
		t.Error("the second invitation carried the first token; a replacement mints a new one")
	}

	// The first row is superseded rather than deleted: no invitation row is
	// ever deleted, because it is the audit record.
	w := h.do(t, http.MethodGet, "/api/v1/invitations/"+first.Invitation.UID, admin, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("the superseded invitation is gone: %d %s", w.Code, w.Body)
	}
	var inv invitationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &inv); err != nil {
		t.Fatal(err)
	}
	if inv.Status != string(domain.InvitationSuperseded) {
		t.Errorf("status = %q, want superseded", inv.Status)
	}

	// And the old link is dead at once, rather than at whatever its expiry
	// said.
	if w := h.redeem(t, map[string]string{
		"token": first.Token, "email": "twice@example.com",
		"name": "Too Late", "password": password,
	}); w.Code == http.StatusCreated {
		t.Error("the superseded link still redeems")
	}

	// The new one works.
	if w := h.redeem(t, map[string]string{
		"token": second.Token, "email": "twice@example.com",
		"name": "In Time", "password": password,
	}); w.Code != http.StatusCreated {
		t.Errorf("the replacement link does not redeem: %d %s", w.Code, w.Body)
	}
}

// TestRevocation covers the two rules revocation carries: it is idempotent, and
// it refuses an invitation that has already been redeemed.
func TestRevocation(t *testing.T) {
	h := newHarness(t)
	h.user(t, "admin@example.com", domain.Create)
	admin := h.login(t, "admin@example.com")

	inv := h.invite(t, admin, "gone@example.com")
	path := "/api/v1/invitations/" + inv.Invitation.UID + "/revoke"

	w := h.do(t, http.MethodPost, path, admin, map[string]string{"reason": "hired somebody else"})
	if w.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", w.Code, w.Body)
	}
	var out invitationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Status != string(domain.InvitationRevoked) {
		t.Errorf("status = %q, want revoked", out.Status)
	}
	if out.Reason != "hired somebody else" {
		t.Errorf("reason = %q, want the one that was given -- it is what stands in for a separate \"force expiry\" verb", out.Reason)
	}

	// Twice is not an error: nothing changed, so there is nothing to report as
	// a conflict.
	if w := h.do(t, http.MethodPost, path, admin, nil); w.Code != http.StatusOK {
		t.Errorf("revoking twice answered %d, want 200: %s", w.Code, w.Body)
	}

	// A redeemed invitation is a different matter: the account exists.
	redeemed := h.invite(t, admin, "here@example.com")
	if w := h.redeem(t, map[string]string{
		"token": redeemed.Token, "email": "here@example.com",
		"name": "Already Here", "password": password,
	}); w.Code != http.StatusCreated {
		t.Fatalf("redeem: %d %s", w.Code, w.Body)
	}
	w = h.do(t, http.MethodPost,
		"/api/v1/invitations/"+redeemed.Invitation.UID+"/revoke", admin, nil)
	if w.Code != http.StatusConflict {
		t.Errorf("revoking a redeemed invitation answered %d, want 409: %s", w.Code, w.Body)
	}
}

// TestInvitationListingHidesTheSettled is the retention criterion: pending by
// default, everything on request, and nothing deleted.
func TestInvitationListingHidesTheSettled(t *testing.T) {
	h := newHarness(t)
	h.user(t, "admin@example.com", domain.Create)
	admin := h.login(t, "admin@example.com")

	open := h.invite(t, admin, "open@example.com")
	closed := h.invite(t, admin, "closed@example.com")
	if w := h.do(t, http.MethodPost,
		"/api/v1/invitations/"+closed.Invitation.UID+"/revoke", admin, nil); w.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", w.Code, w.Body)
	}

	list := h.invitations(t, admin, "")
	if len(list.Invitations) != 1 || list.Invitations[0].UID != open.Invitation.UID {
		t.Errorf("the default listing returned %d rows, want only the pending one", len(list.Invitations))
	}
	if list.Status != string(domain.InvitationPending) {
		t.Errorf("status = %q, want pending so a client can tell an empty queue from an empty database", list.Status)
	}

	all := h.invitations(t, admin, "?status=all")
	if len(all.Invitations) != 2 {
		t.Errorf("the full listing returned %d rows, want 2; no invitation row is ever deleted", len(all.Invitations))
	}

	revoked := h.invitations(t, admin, "?status=revoked")
	if len(revoked.Invitations) != 1 || revoked.Invitations[0].UID != closed.Invitation.UID {
		t.Errorf("the revoked filter returned %d rows, want the one", len(revoked.Invitations))
	}

	if w := h.do(t, http.MethodGet, "/api/v1/invitations?status=expired", admin, nil); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("status=expired answered %d, want 422: expiry is derived and is not a status", w.Code)
	}
}

func (h *harness) invitations(t *testing.T, token, query string) invitationsResponse {
	t.Helper()
	w := h.do(t, http.MethodGet, "/api/v1/invitations"+query, token, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list%s: %d %s", query, w.Code, w.Body)
	}
	var out invitationsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestInvitationPrivileges is the authorization rule: create over the system
// subject writes one, read over it lists users, and a person with neither is
// refused.
func TestInvitationPrivileges(t *testing.T) {
	h := newHarness(t)
	h.user(t, "admin@example.com", domain.Create)
	h.user(t, "editor@example.com", domain.Edit)
	admin := h.login(t, "admin@example.com")
	editor := h.login(t, "editor@example.com")

	if w := h.do(t, http.MethodPost, "/api/v1/invitations", editor,
		map[string]string{"email": "nope@example.com"}); w.Code != http.StatusForbidden {
		t.Errorf("an editor creating an invitation answered %d, want 403: %s", w.Code, w.Body)
	}
	if w := h.do(t, http.MethodGet, "/api/v1/invitations", editor, nil); w.Code != http.StatusForbidden {
		t.Errorf("an editor listing invitations answered %d, want 403", w.Code)
	}

	// The user list is read rather than create, deliberately: somebody handing
	// work over needs a uid, and making that an administrator's errand would
	// be worse than the exposure of a list of colleagues.
	if w := h.do(t, http.MethodGet, "/api/v1/users", editor, nil); w.Code != http.StatusOK {
		t.Errorf("an editor listing users answered %d, want 200: %s", w.Code, w.Body)
	}
	if w := h.do(t, http.MethodGet, "/api/v1/invitations", admin, nil); w.Code != http.StatusOK {
		t.Errorf("an administrator listing invitations answered %d, want 200", w.Code)
	}

	// Redemption is the one unauthenticated write, and it stays one: a caller
	// with no credentials gets the ordinary refusal rather than a 401.
	if w := h.redeem(t, map[string]string{
		"token": "nonsense", "email": "nobody@example.com",
		"name": "Nobody", "password": password,
	}); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("an anonymous redemption answered %d, want 422", w.Code)
	}
}

// TestUserLookupFindsTheUIDWithoutTheCreationOutput is the criterion that makes
// the role route usable: an administrator who did not keep what created an
// account can still find it and give it a role.
func TestUserLookupFindsTheUIDWithoutTheCreationOutput(t *testing.T) {
	h := newHarness(t)
	h.user(t, "admin@example.com", domain.Create)
	admin := h.login(t, "admin@example.com")

	created := h.invite(t, admin, "sought@example.com")
	if w := h.redeem(t, map[string]string{
		"token": created.Token, "email": "sought@example.com",
		"name": "Sought After", "password": password,
	}); w.Code != http.StatusCreated {
		t.Fatalf("redeem: %d %s", w.Code, w.Body)
	}

	// Find them by a piece of the address, with nothing kept from redemption.
	w := h.do(t, http.MethodGet, "/api/v1/users?q=sought", admin, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("search: %d %s", w.Code, w.Body)
	}
	var out usersResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Users) != 1 {
		t.Fatalf("the search returned %d accounts, want 1", len(out.Users))
	}
	uid := out.Users[0].UID

	// And the uid it gives is the one the role route takes.
	if _, err := h.db.CreateRole(t.Context(), "copyeditor", "Copy editor"); err != nil {
		t.Fatal(err)
	}
	if w := h.do(t, http.MethodPost, "/api/v1/users/"+uid+"/roles", admin,
		map[string]string{"role": "copyeditor"}); w.Code != http.StatusNoContent && w.Code != http.StatusOK {
		t.Errorf("assigning a role to the uid the search found answered %d: %s", w.Code, w.Body)
	}
}
