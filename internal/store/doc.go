// Copyright (c) 2026 Michael D Henderson.

// Package store is the SQLite persistence layer. It contains all the SQL in
// this repository and no business decisions, converting between rows and
// domain types (DESIGN.md 13, invariant 2).
//
// Permitted imports: the standard library, zombiezen.com/go/sqlite, and
// internal/domain.
//
// Empty until M1, which adds pool construction and the create/open split that
// keeps cmsd from ever creating or migrating a database (DESIGN.md 13.4).
package store
