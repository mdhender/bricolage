// Copyright (c) 2026 Michael D Henderson.

// Package service holds the use cases. It owns transactions, orchestrates
// operations, writes events, and enqueues jobs (DESIGN.md 3).
//
// Permitted imports: internal/domain, internal/store, and the subsystem
// packages beside it. Never internal/api or internal/web.
//
// Empty until M2.
package service
