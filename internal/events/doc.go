// Copyright (c) 2026 Michael D Henderson.

// Package events records the event every state-changing operation must write
// (invariant 7), evaluates alert rules, and delivers notifications
// (DESIGN.md 10).
//
// Permitted imports: internal/domain, internal/store, internal/clock.
//
// Empty until M2.
package events
