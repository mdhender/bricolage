// Copyright (c) 2026 Michael D Henderson.

package api

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/mdhender/bricolage/internal/config"
	"github.com/mdhender/bricolage/internal/domain"
	"github.com/mdhender/bricolage/internal/reqctx"
)

// authenticated wraps a handler so that it runs only for a caller with a live
// session, and puts the identity on the request context.
//
// The identity carries the user, the roles, and the grants, loaded once here
// (DESIGN.md 7.2). A handler that needs to authorize something calls
// authz.Resolve against what is already on the context rather than issuing a
// query of its own.
func (h *Handler) authenticated(next func(http.ResponseWriter, *http.Request, domain.Identity)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, err := credential(r)
		if err != nil {
			h.unauthenticated(w, r, err)
			return
		}
		identity, err := h.svc.Authenticate(r.Context(), token)
		if err != nil {
			h.unauthenticated(w, r, err)
			return
		}
		next(w, r.WithContext(reqctx.WithIdentity(r.Context(), identity)), identity)
	})
}

// unauthenticated answers a 401 with the WWW-Authenticate header the status
// code requires of us.
func (h *Handler) unauthenticated(w http.ResponseWriter, r *http.Request, err error) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="cms"`)
	h.writeError(w, r, err)
}

// credential extracts the caller's token from the Authorization header or the
// session cookie, in that order.
//
// A bearer token wins over a cookie when both are present. That is not
// arbitrary: the CSRF protection exempts bearer-token requests and does not
// exempt cookie ones (DESIGN.md 11), so the two must agree about which
// credential is in use, and the rule is stated once, here.
func credential(r *http.Request) (string, error) {
	if tok, ok := BearerToken(r); ok {
		return tok, nil
	}
	if c, err := r.Cookie(config.SessionCookieName); err == nil && c.Value != "" {
		return c.Value, nil
	}
	return "", fmt.Errorf("no bearer token and no session cookie: %w", domain.ErrUnauthenticated)
}

// BearerToken returns the token from an Authorization header, if there is one.
//
// It is exported because the CSRF wrapper in internal/server asks the same
// question -- "is this request authenticated by a token rather than by a
// cookie" -- and two implementations of that question would eventually
// disagree, in the direction of exempting a cookie request from CSRF.
func BearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", false
	}
	const prefix = "bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	tok := strings.TrimSpace(h[len(prefix):])
	if tok == "" {
		return "", false
	}
	return tok, true
}
