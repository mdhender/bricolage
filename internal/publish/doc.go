// Copyright (c) 2026 Michael D Henderson.

// Package publish owns rendering to output channels, resource tracking,
// stale expiry, and the related-asset cascade (DESIGN.md 8). Publish jobs pin
// a version id, never a document id (invariant 8).
//
// Permitted imports: internal/domain, internal/store, internal/render.
//
// Empty until M7.
package publish
