// Copyright (c) 2026 Michael D Henderson.

// Package authz answers two questions about a caller: who they are, and what
// they may do.
//
// Authorization is grants over scopes (DESIGN.md 7). Resolution is MAX with
// deny winning, and nobody may grant a privilege they do not hold over that
// scope (invariant 12). Both are pure functions over domain types, evaluated
// once per request against grants the store loaded, because the SQL equivalent
// is the definition of the rule rather than the hot path.
//
// Authentication is the credential primitives in credentials.go: bcrypt
// password hashing, session token minting, and the SHA-256 the sessions table
// is keyed by (DESIGN.md 14). DESIGN.md 4 does not name a package for them.
// They are here rather than in a package of their own because they are small,
// because they have no other client, and because a package named for "who may
// do what" is where somebody looks for "who is this" -- and they are here
// rather than in internal/service so that the primitive and its rules cannot
// be quietly reimplemented by the second caller.
//
// Permitted imports: internal/domain, and golang.org/x/crypto for bcrypt.
// Nothing here performs I/O, reads the clock, or touches the store: every
// function takes what it needs.
package authz
