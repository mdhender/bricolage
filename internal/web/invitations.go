// Copyright (c) 2026 Michael D Henderson.

package web

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mdhender/bricolage/internal/config"
	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/edge"
	"github.com/mdhender/bricolage/internal/reqctx"
	"github.com/mdhender/bricolage/internal/service"
)

// The invitation screens (issue #6): the administrator's list, and the public
// page a link lands on.
//
// The redemption page is the only screen in this package that renders without a
// session, and the second route in the whole UI that does -- the other is the
// login form. It needs no CSRF token of its own and writing one would be a
// defect: withCSRF in internal/server wraps the whole mux and exempts only
// bearer-token requests, so this form is protected by being registered
// (DESIGN.md 11).
//
// Redemption ends at /login with no session issued. That is issue #6's security
// decision and not a stylistic one: a redemption that signed the person in
// would have login CSRF, because an attacker holding an invitation could make a
// victim's browser redeem it with a password the attacker chose.

// invitationsPage is GET /admin/invitations.
type invitationsPage struct {
	Base

	Invitations []domain.Invitation
	Statuses    []domain.InvitationStatus

	// Filter is the status being shown, "all" when every one of them is.
	Filter string

	// Now is the instant expiry is derived against, so that the template can
	// say "expired" about a row the database still calls pending. It is the
	// service's clock and never time.Now (invariant 3).
	Now time.Time

	// Created is the invitation just made and the link that redeems it, shown
	// once and never retrievable afterwards. The zero value means this is an
	// ordinary listing.
	Created *service.CreatedInvitation

	Problem string
}

// invitations draws the administrator's list: pending by default, the rest on
// request.
func (h *Handler) invitations(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	h.drawInvitations(w, r, identity, nil, http.StatusOK)
}

// drawInvitations renders the list, optionally with a just-created invitation
// above it.
func (h *Handler) drawInvitations(w http.ResponseWriter, r *http.Request, identity domain.Identity, created *service.CreatedInvitation, status int) {
	filter := domain.InvitationPending
	switch raw := r.URL.Query().Get("status"); raw {
	case "", "pending":
	case "all":
		filter = ""
	default:
		parsed, err := domain.ParseInvitationStatus(raw)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		filter = parsed
	}

	list, err := h.svc.Invitations(r.Context(), identity, filter)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	p := invitationsPage{
		Base:        h.base(r, identity, "Invitations"),
		Invitations: list,
		Statuses:    domain.InvitationStatuses,
		Filter:      string(filter),
		Now:         h.svc.Now(),
		Created:     created,
		Problem:     r.URL.Query().Get("problem"),
	}
	if p.Filter == "" {
		p.Filter = "all"
	}
	h.render(w, r, "invitations.gohtml", status, p)
}

// createInvitation is POST /admin/invitations.
//
// It renders the list rather than redirecting to it, which is the one place this
// UI departs from post-then-redirect, and the reason is the link: it is returned
// once and a redirect would either drop it or carry a working credential in a
// query parameter, into the browser's history and the proxy's log. So the
// response to the POST is the page with the link on it, and reloading that page
// is a GET that shows the list without it.
func (h *Handler) createInvitation(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	if err := form(r); err != nil {
		h.fail(w, r, err)
		return
	}
	created, err := h.svc.CreateInvitation(r.Context(), identity, service.NewInvitation{
		Email: r.PostFormValue("email"),
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.drawInvitations(w, r, identity, &created, http.StatusCreated)
}

// revokeInvitation is POST /admin/invitations/{uid}/revoke.
//
// It is the only verb here besides creating one. There is no button that extends
// an invitation, renews an expired one, or forces one to expire: re-inviting
// replaces a lapsed invitation, and forcing expiry is this with a different word
// in the audit trail (internal/service/invitations.go says why at length).
func (h *Handler) revokeInvitation(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	if err := form(r); err != nil {
		h.fail(w, r, err)
		return
	}
	if _, err := h.svc.RevokeInvitation(r.Context(), identity,
		r.PathValue("uid"), r.PostFormValue("reason")); err != nil {
		h.fail(w, r, err)
		return
	}
	redirect(w, r, "/admin/invitations", "invitation revoked")
}

// usersPage is GET /admin/users.
type usersPage struct {
	Base

	Users []domain.User
	Query string
}

// users draws the account list, which is what makes the role form usable: it
// needs a uid, and before this screen the only way to have one was to have kept
// the output of the command that created the account.
func (h *Handler) users(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	q := r.URL.Query().Get("q")
	list, err := h.svc.Users(r.Context(), identity, q)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.render(w, r, "users.gohtml", http.StatusOK, usersPage{
		Base:  h.base(r, identity, "Users"),
		Users: list,
		Query: q,
	})
}

// redeemPage is GET /invite/{token} and the redraw of a refused POST /invite.
type redeemPage struct {
	Base

	// Token is carried in a hidden field so that the POST's path does not hold
	// it. The link has to carry it -- that is what a magic link is -- but the
	// form it leads to does not have to put it in a second URL.
	Token string

	Email string
	Name  string

	// Problem is one sentence, and it is the same sentence for every failure.
	// Six causes, one message: a page that distinguished them would be an
	// oracle for which addresses have been invited, and the e-mail check that
	// makes a forwarded link useless would become the way to enumerate them.
	Problem string
}

// redeemForm is GET /invite/{token}.
//
// It says nothing about whether the token is any good. The page is the same for
// a live invitation and for one that was used last week, because a page that
// differed would answer the question this flow exists not to answer; the
// refusal comes on the POST, by which point somebody has at least had to guess
// an address.
func (h *Handler) redeemForm(w http.ResponseWriter, r *http.Request) {
	h.render(w, r, "redeem.gohtml", http.StatusOK, redeemPage{
		Base:  Base{Title: "Accept your invitation", Development: h.env.IsDevelopment()},
		Token: r.PathValue("token"),
	})
}

// redeem is POST /invite: create the account and send the person to sign in.
//
// No session is issued and the redirect is to /login, which is the whole of the
// defence against login CSRF on this route (issue #6). The password they just
// chose is exercised immediately, while they still remember typing it.
func (h *Handler) redeem(w http.ResponseWriter, r *http.Request) {
	if err := form(r); err != nil {
		h.fail(w, r, err)
		return
	}
	in := domain.Redemption{
		Token:    r.PostFormValue("token"),
		Email:    r.PostFormValue("email"),
		Name:     r.PostFormValue("name"),
		Password: r.PostFormValue("password"),
	}
	if confirm := r.PostFormValue("confirm"); confirm != in.Password {
		// Checked here and not in the service, because it is a property of
		// this form and not of a redemption: the JSON route takes one password
		// and has nothing to compare it with.
		h.redraw(w, r, in, "those two passwords are not the same", http.StatusUnprocessableEntity)
		return
	}

	if _, err := h.svc.Redeem(r.Context(), in, reqctx.ClientAddr(r.Context())); err != nil {
		status, _, _ := edge.StatusFor(err)
		switch {
		case errors.Is(err, service.ErrRedemptionRefused):
			// One of the six, and which one is in the server's log and
			// nowhere else. A refusal redraws the form rather than rendering
			// the error page, for the reason a failed sign-in does: what the
			// person needs is the form again. The status is still the one
			// internal/edge maps the error to, so an automated caller reads a
			// refusal rather than a page.
			h.redraw(w, r, in, "that invitation cannot be accepted", status)
		case status < http.StatusInternalServerError:
			// The password or the name. Whoever is reading this has already
			// shown they hold the link and know the address, so telling them
			// what is wrong reveals nothing and is the only way they can fix
			// it.
			h.redraw(w, r, in, err.Error(), status)
		default:
			h.fail(w, r, err)
		}
		return
	}

	// The account exists and has no session. Sign in.
	redirect(w, r, "/login?email="+url.QueryEscape(domain.NormalizeEmail(in.Email)),
		"your account is ready; sign in with the password you just set")
}

// redraw renders the form again with one sentence about what went wrong, and
// without the password: a form that filled a password field back in is a form
// that puts one in the browser's saved state for a page anybody can reach.
func (h *Handler) redraw(w http.ResponseWriter, r *http.Request, in domain.Redemption, problem string, status int) {
	h.render(w, r, "redeem.gohtml", status, redeemPage{
		Base:    Base{Title: "Accept your invitation", Development: h.env.IsDevelopment()},
		Token:   in.Token,
		Email:   strings.TrimSpace(in.Email),
		Name:    strings.TrimSpace(in.Name),
		Problem: problem,
	})
}

// invitePath is where a link lands. It is config's constant rather than a
// string here, so that the route this package registers and the link
// internal/service hands out cannot drift apart.
const invitePath = config.InvitationPath
