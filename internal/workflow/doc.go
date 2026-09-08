// Copyright (c) 2026 Michael D Henderson.

// Package workflow is the transition engine: guards, effects, and
// availability. It is the only writer of documents.state (invariant 4), and
// Available and Do share one check function (invariant 5, DESIGN.md 6.2).
//
// Permitted imports: internal/domain, internal/store.
//
// Empty until M6.
package workflow
