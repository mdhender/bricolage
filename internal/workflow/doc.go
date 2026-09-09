// Copyright (c) 2026 Michael D Henderson.

// Package workflow is the transition engine: guards, effects, and
// availability (DESIGN.md 6).
//
// It is the only writer of documents.state (invariant 4). No other code path
// assigns to that column -- not a service method, not a migration fix-up --
// and internal/store exposes no way to set it that is not a whole transition
// authorised by the check function here.
//
// Available and Do share one check function (invariant 5, DESIGN.md 6.2). That
// is not a style preference: it is the structural fix for the worst defect in
// the system we learned from, where the permission check lived only in the
// template that drew the menu, so the UI and the server could disagree about
// what was allowed. Here the UI renders from Available, Do re-runs the
// identical checks against the same facts, and "the UI offered something the
// server refuses" is unrepresentable.
//
// The vocabulary types -- Guard, Effect, Transition, Workflow -- live in
// internal/domain rather than here. DESIGN.md 6.1 shows them under this
// package, and the engine is here; the types are one level down because
// internal/store converts rows to domain types and this package imports
// internal/store, so a Transition declared here could never be read out of
// workflow_transitions. The split changes nothing about the rules: the guards
// are still a closed set, and there is still exactly one check.
//
// Permitted imports: internal/domain, internal/store, internal/authz,
// internal/clock, internal/events.
package workflow
