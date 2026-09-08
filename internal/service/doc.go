// Copyright (c) 2026 Michael D Henderson.

// Package service holds the use cases. It owns transactions, orchestrates
// operations, writes events, and enqueues jobs (DESIGN.md 3).
//
// Two rules give this package its shape. Every state-changing method writes an
// event carrying enough to reconstruct what happened (invariant 7), so a
// method that returns without one is not finished. And every policy decision
// is made here rather than in a transport or in the store: the anti-escalation
// check lives in the same function that writes a grant (invariant 12), because
// a check beside the write is a check somebody routes around.
//
// It takes a Clock rather than reading one, so session expiry, leases, and due
// dates are testable at an arbitrary instant (invariant 3).
//
// Permitted imports: internal/domain, internal/store, and the subsystem
// packages beside it. Never internal/api or internal/web.
package service
