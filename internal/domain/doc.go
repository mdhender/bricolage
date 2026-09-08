// Copyright (c) 2026 Michael D Henderson.

// Package domain holds the entities, value types, invariants, and pure
// functions that decide things. It is the bottom of the dependency graph.
//
// Permitted imports: the standard library only. Not "database/sql", not
// "net/http", and never time.Now (DESIGN.md 3, invariants 1 and 3). It
// imports nothing else in this repository, and a test asserts it
// (PLAN.md M0, acceptance 3).
package domain
