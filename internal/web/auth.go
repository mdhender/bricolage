// Copyright (c) 2026 Michael D Henderson.

package web

import (
	"fmt"
	"net/http"
	"net/url"

	"github.com/mdhender/bricolage/internal/config"
	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/edge"
	"github.com/mdhender/bricolage/internal/reqctx"
)

// The UI authenticates with the session cookie and nothing else.
//
// A bearer token is the JSON API's credential. Accepting one here would mean a
// page that could be fetched with a token and a cookie at once, and the CSRF
// wrapper in internal/server decides whether to exempt a request by asking
// which credential is in use (DESIGN.md 11) -- so a screen that took both
// would be a screen whose exemption depended on which one it happened to read
// first.

// page wraps a handler so that it runs only for a caller with a live session,
// and puts the identity on the request context.
//
// A signed-out browser asking for a page is redirected to the login form with
// a returnTo, because that is what a person needs; a signed-out browser
// posting a form gets the 401 page, because a redirect would silently discard
// what they typed and land them somewhere that looks like it worked.
func (h *Handler) page(next func(http.ResponseWriter, *http.Request, domain.Identity)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, err := h.identity(r)
		if err != nil {
			if r.Method == http.MethodGet {
				h.toLogin(w, r)
				return
			}
			h.fail(w, r, err)
			return
		}
		next(w, r.WithContext(reqctx.WithIdentity(r.Context(), identity)), identity)
	})
}

// identity resolves the caller from the session cookie.
func (h *Handler) identity(r *http.Request) (domain.Identity, error) {
	c, err := r.Cookie(config.SessionCookieName)
	if err != nil || c.Value == "" {
		return domain.Identity{}, fmt.Errorf("no session cookie: %w", domain.ErrUnauthenticated)
	}
	return h.svc.Authenticate(r.Context(), c.Value)
}

// toLogin sends a signed-out browser to the login form, remembering where it
// was going.
func (h *Handler) toLogin(w http.ResponseWriter, r *http.Request) {
	to := "/login"
	if want := r.URL.RequestURI(); want != "" && want != "/" {
		to += "?returnTo=" + url.QueryEscape(want)
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// loginPage is GET /login.
type loginPage struct {
	Base

	Email    string
	ReturnTo string

	// Problem is what went wrong with the last attempt, in the words a person
	// reading a login form should get: one sentence that does not say which
	// half was wrong. The detail is in the event log, where it belongs.
	Problem string
}

// loginForm draws the sign-in page.
//
// A caller who already has a session is sent on rather than shown a form: a
// login form that appears when you are logged in is a form people fill in.
func (h *Handler) loginForm(w http.ResponseWriter, r *http.Request) {
	if _, err := h.identity(r); err == nil {
		http.Redirect(w, r, h.returnTo(r.URL.Query().Get("returnTo")), http.StatusSeeOther)
		return
	}
	p := loginPage{
		Base:     Base{Title: "Sign in", Development: h.env.IsDevelopment()},
		ReturnTo: r.URL.Query().Get("returnTo"),
		Problem:  r.URL.Query().Get("problem"),
	}
	h.render(w, r, "login.gohtml", http.StatusOK, p)
}

// login is POST /login: the cookie half of DESIGN.md 12's POST /sessions.
//
// It calls the same service method the JSON route does, so a login through the
// UI writes the same event and mints the same session; the difference is that
// the token is written to a cookie and never rendered. The client address is
// read from the context, resolved once in middleware (invariant 14).
func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	if err := form(r); err != nil {
		h.fail(w, r, err)
		return
	}
	email := r.PostFormValue("email")
	password := r.PostFormValue("password")
	returnTo := h.returnTo(r.PostFormValue("returnTo"))

	result, err := h.svc.Login(r.Context(), email, password, reqctx.ClientAddr(r.Context()))
	if err != nil {
		// A failed sign-in redraws the form rather than rendering the error
		// page: what the person needs is the form again, and what they must
		// not be told is which half was wrong. The 401 is still the status,
		// so an automated caller reads a refusal rather than a page.
		h.log.Info("sign-in refused", "email", email, "client", reqctx.ClientAddr(r.Context()))
		p := loginPage{
			Base:     Base{Title: "Sign in", Development: h.env.IsDevelopment()},
			Email:    email,
			ReturnTo: r.PostFormValue("returnTo"),
			Problem:  "that email address and password do not match an account",
		}
		h.render(w, r, "login.gohtml", http.StatusUnauthorized, p)
		return
	}

	edge.SetSessionCookie(w, result.Token, result.Session.ExpiresAt)
	http.Redirect(w, r, returnTo, http.StatusSeeOther)
}

// logout is POST /logout.
//
// It is a POST because it changes something -- the session is revoked, not
// merely forgotten -- which also means it goes through the CSRF protection
// like every other cookie-authenticated write.
func (h *Handler) logout(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	if err := h.svc.Logout(r.Context(), identity); err != nil {
		h.fail(w, r, err)
		return
	}
	edge.ClearSessionCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// returnTo validates a redirect target against the public origin, falling back
// to the dashboard.
//
// The validation is config.PublicOrigin's, which is the same function the
// development log-me-in route uses (DESIGN.md 11). An open redirect on a login
// form is how a phishing page borrows a real origin, and the refusals that
// matter -- "//evil.example.com", a userinfo host, a backslash -- are subtle
// enough that a second implementation would get one of them wrong.
func (h *Handler) returnTo(raw string) string {
	if raw == "" {
		return "/"
	}
	to, err := h.origin.ValidateReturnTo(raw)
	if err != nil {
		h.log.Warn("returnTo refused", "returnTo", raw, "error", err)
		return "/"
	}
	return to
}
