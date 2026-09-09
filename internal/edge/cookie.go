// Copyright (c) 2026 Michael D Henderson.

package edge

import (
	"net/http"
	"time"

	"github.com/mdhender/bricolage/internal/config"
)

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
// Three callers write a session: the JSON API's login, the HTML UI's login,
// and the development log-me-in route. A session any of them issues is an
// ordinary session, so it gets an ordinary cookie, and there is one function
// that says what that means. A second one is where the Secure flag goes
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

// ClearSessionCookie expires the session cookie. The attributes must match the
// ones it was set with, or the browser keeps the old cookie beside the new
// one.
func ClearSessionCookie(w http.ResponseWriter) {
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
