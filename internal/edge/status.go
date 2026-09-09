// Copyright (c) 2026 Michael D Henderson.

package edge

import (
	"errors"
	"net/http"

	"github.com/mdhender/bricolage/internal/domain"
)

// StatusFor maps a domain error to an HTTP status, a short kind, and a title.
//
// This function is the whole of the mapping (DESIGN.md 14). Nothing else in
// this repository turns an error into a status code: one table, in one place,
// so that a new sentinel is one edit and no transport can quietly disagree
// about what "conflict" means.
//
// The table is DESIGN.md 12's, unchanged:
//
//	unauthenticated                        401
//	authenticated, privilege insufficient  403
//	unknown uid                            404
//	guard refused, lock held, conflict     409
//	malformed body, invalid content        422
//
// One row is an addition to it: a request this server was not configured to
// answer -- rendering with no template tree -- is 503. It is not in
// DESIGN.md 12's table because until M8 nothing could be half configured.
//
// The kind is the last segment of the JSON API's problem type and the class on
// the HTML error page. It is returned rather than derived twice because the
// two transports name the same refusal, and a client that has learnt
// "guard-failed" from one should read the same word in the other.
func StatusFor(err error) (status int, kind, title string) {
	switch {
	case errors.Is(err, domain.ErrUnauthenticated):
		return http.StatusUnauthorized, "unauthenticated", "Not signed in"
	case errors.Is(err, domain.ErrForbidden):
		return http.StatusForbidden, "forbidden", "Not permitted"
	case errors.Is(err, domain.ErrNotFound):
		return http.StatusNotFound, "not-found", "Not found"
	case errors.Is(err, domain.ErrGuardFailed):
		return http.StatusConflict, "guard-failed", "Transition refused"
	case errors.Is(err, domain.ErrConflict):
		return http.StatusConflict, "conflict", "Conflict"
	case errors.Is(err, domain.ErrInvalid):
		return http.StatusUnprocessableEntity, "invalid", "Invalid request"
	case errors.Is(err, domain.ErrUnavailable):
		return http.StatusServiceUnavailable, "unavailable", "Not available"
	default:
		return http.StatusInternalServerError, "internal", "Internal error"
	}
}
