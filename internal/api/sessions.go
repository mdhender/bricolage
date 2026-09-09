// Copyright (c) 2026 Michael D Henderson.

package api

import (
	"fmt"
	"net/http"
	"time"

	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/edge"
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

	edge.SetSessionCookie(w, result.Token, result.Session.ExpiresAt)
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
	edge.ClearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}
