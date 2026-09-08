// Copyright (c) 2026 Michael D Henderson.

// Package authz resolves grants, matches scopes, and computes effective
// privilege. Resolution is MAX with deny winning, and nobody may grant a
// privilege they do not hold (invariant 12, DESIGN.md 7).
//
// Permitted imports: internal/domain, internal/store.
//
// Empty until M2.
package authz
