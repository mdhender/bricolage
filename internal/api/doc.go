// Copyright (c) 2026 Michael D Henderson.

// Package api is the JSON REST transport. Handlers parse a request, call one
// service method, and render a response; they hold no business logic and never
// touch internal/store directly (DESIGN.md 3, 12). Mapping a domain error to
// an HTTP status happens here and only here.
//
// Permitted imports: internal/service, internal/domain.
//
// Empty until M2.
package api
