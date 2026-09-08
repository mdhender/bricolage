// Copyright (c) 2026 Michael D Henderson.

// Package events owns the event vocabulary: the type constants every
// state-changing operation writes one of, and their display names
// (DESIGN.md 10, invariant 7).
//
// The row is written by internal/store, because all SQL lives there
// (invariant 2), and inside the same transaction as the change it records: an
// event that can be rolled back separately from its state change is not an
// audit record. What lives here is what an event means.
//
// Alert rule evaluation and notification delivery arrive in M12 and belong
// here too.
//
// Permitted imports: internal/domain, internal/store, internal/clock.
package events
