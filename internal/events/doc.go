// Copyright (c) 2026 Michael D Henderson.

// Package events owns the event vocabulary and the alert engine over it: the
// type constants every state-changing operation writes one of, their display
// names, and the dispatcher that turns an event row into a notification
// (DESIGN.md 10, invariant 7).
//
// The row is written by internal/store, because all SQL lives there
// (invariant 2), and inside the same transaction as the change it records: an
// event that can be rolled back separately from its state change is not an
// audit record. What lives here is what an event means.
//
// Alert evaluation runs after that commit, driven off the event row and never
// from inside the operation, so that a failing notification cannot roll back
// an editorial action (DESIGN.md 6.4). alerts.go says why that is a poll over
// the events table rather than a call at the end of each service method.
//
// Permitted imports: internal/domain, internal/clock, internal/ids. Not
// internal/store: the dispatcher names the seven methods it needs as an
// interface, because internal/store's own tests use the constants in this file
// and a package cannot import the package whose tests import it.
package events
