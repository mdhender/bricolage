// Copyright (c) 2026 Michael D Henderson.

// Package api is the JSON REST transport (DESIGN.md 12). Handlers parse a
// request, call one service method, and render a response; they hold no
// business logic and never touch internal/store directly (DESIGN.md 3).
//
// Mapping a domain error to an HTTP status happens here and only here, in
// statusFor, and the result is an RFC 9457 problem document. One table in one
// function is what keeps two handlers from disagreeing about what "conflict"
// means.
//
// Authentication is a bearer token or the session cookie; a bearer token wins
// when both are present, which is the same rule the CSRF wrapper in
// internal/server applies when it decides whether to exempt a request
// (DESIGN.md 11). It is stated once, in credential and BearerToken, so the two
// cannot drift apart in the direction of exempting a cookie.
//
// Permitted imports: internal/service, internal/domain, internal/authz,
// internal/config, internal/reqctx, internal/events. The last is the event
// vocabulary's display-name registry: DESIGN.md 10 says the screens list event
// types from code rather than from a SELECT, and a transport rendering a
// history is one of those screens. It is a leaf of constants either way, so
// the dependency still points downward.
package api
