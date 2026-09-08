// Copyright (c) 2026 Michael D Henderson.

// Package ids generates the external identifiers the API speaks: lowercase
// ULIDs in the uid column. Internal integer primary keys never leave the store
// (invariant 10, DESIGN.md 13.5).
//
// A ULID is 48 bits of Unix milliseconds followed by 80 random bits, rendered
// in Crockford base32. Two properties earn it over a random string: it sorts
// by creation time, so an index on uid is not a random walk, and the time is
// readable back out of it without a query.
//
// The caller supplies the instant. This package never reads the clock, because
// time.Now belongs to main and internal/clock (invariant 3).
//
// Permitted imports: the standard library.
package ids
