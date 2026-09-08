// Copyright (c) 2026 Michael D Henderson.

package api

import (
	"fmt"
	"net/http"
	"time"

	"github.com/mdhender/bricolage/internal/config"
	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/reqctx"
)

// loginRequest is the body of POST /api/v1/sessions.
type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// sessionResponse is what a successful login returns. The token appears here
// once and is never readable again: the database holds only its SHA-256.
type sessionResponse struct {
	Token     string       `json:"token"`
	ExpiresAt time.Time    `json:"expires_at"`
	User      userResponse `json:"user"`
}

// createSession is POST /api/v1/sessions: log in, return a token
// (DESIGN.md 12).
func (h *Handler) createSession(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := decodeJSON(r, &req); err != nil {
		h.writeError(w, r, err)
		return
	}
	if req.Email == "" || req.Password == "" {
		h.writeError(w, r, fmt.Errorf("email and password are required: %w", domain.ErrInvalid))
		return
	}

	// The client address was resolved once, in middleware, and is read from
	// the context rather than parsed again here (invariant 14).
	result, err := h.svc.Login(r.Context(), req.Email, req.Password, reqctx.ClientAddr(r.Context()))
	if err != nil {
		h.unauthenticated(w, r, err)
		return
	}

	SetSessionCookie(w, result.Token, result.Session.ExpiresAt)
	writeJSON(w, http.StatusCreated, sessionResponse{
		Token:     result.Token,
		ExpiresAt: result.Session.ExpiresAt,
		User:      newUserResponse(result.Identity.User),
	})
}

// deleteCurrentSession is DELETE /api/v1/sessions/current: log out.
func (h *Handler) deleteCurrentSession(w http.ResponseWriter, r *http.Request, identity domain.Identity) {
	if err := h.svc.Logout(r.Context(), identity); err != nil {
		h.writeError(w, r, err)
		return
	}
	clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

// SetSessionCookie writes the session cookie.
//
// Secure, HttpOnly, and SameSite=Lax, unconditionally (invariant 13). The
// Secure flag describes the browser's connection, which is TLS, not the
// loopback hop from the proxy -- so it is never gated on r.TLS != nil. That
// expression is always false behind the proxy, and the code it produces ships
// insecure cookies while looking careful.
//
// The name carries the "__Host-" prefix, which makes the browser enforce the
// same thing: it rejects the cookie outright unless it is Secure, has no
// Domain, and has Path=/. A mistake here fails visibly rather than silently.
//
// It is a function rather than a method because it reads nothing from the
// handler, and it is exported because the development log-me-in route issues
// an ordinary session and must write an ordinary cookie. One cookie-writing
// path in the process, not two: a second one is where the Secure flag goes
// missing.
func SetSessionCookie(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     config.SessionCookieName,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// clearSessionCookie expires the session cookie. The attributes must match the
// ones it was set with, or the browser keeps the old cookie beside the new
// one.
func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     config.SessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}
