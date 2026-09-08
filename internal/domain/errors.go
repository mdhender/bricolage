// Copyright (c) 2026 Michael D Henderson.

package domain

import "errors"

// The sentinel errors. Everything below the transport edge wraps one of these
// with %w and callers inspect them with errors.Is; mapping them to an HTTP
// status happens in exactly one function, in internal/api (DESIGN.md 14).
//
// They live here because the decision "this was not found" is a domain
// decision, and because a store, a service, and a handler all need to agree on
// what it means.
var (
	// ErrNotFound is returned when a uid names nothing the caller may see.
	ErrNotFound = errors.New("not found")

	// ErrConflict is returned when the request is well formed but the current
	// state refuses it: a lock is held, a version moved, a unique key exists.
	ErrConflict = errors.New("conflict")

	// ErrForbidden is returned when the caller is known but lacks the
	// privilege. Distinguishing it from ErrNotFound is a deliberate choice
	// made per resource at the transport edge, not here.
	ErrForbidden = errors.New("forbidden")

	// ErrGuardFailed is returned when a workflow guard refused a transition.
	// It is separate from ErrForbidden because a guard is a statement about
	// the document, not about the person (DESIGN.md 6.1).
	ErrGuardFailed = errors.New("guard failed")
)
