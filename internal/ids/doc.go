// Copyright (c) 2026 Michael D Henderson.

// Package ids generates the external identifiers the API speaks: lowercase
// ULIDs in the uid column. Internal integer primary keys never leave the store
// (invariant 10, DESIGN.md 13.5).
//
// Permitted imports: the standard library.
//
// Empty until M1.
package ids
