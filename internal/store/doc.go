// Copyright (c) 2026 Michael D Henderson.

// Package store is the SQLite persistence layer. It contains all the SQL in
// this repository and no business decisions, converting between rows and
// domain types (DESIGN.md 13, invariant 2).
//
// Two entry points and one shared name. Both take a directory that must
// already exist and join the constant cms.db to it (DESIGN.md 13.1):
//
//   - Create applies every migration to a database it brings into existence.
//     "cmsdb init" is its only caller, and it is the only place in this
//     repository that names OpenCreate against a file.
//   - Open verifies an existing database and never names OpenCreate, so a
//     missing file is SQLITE_CANTOPEN rather than a new, empty CMS.
//
// The difference between them is the point. A server that creates a database
// comes up healthy and empty; a server that migrates one upgrades production
// because somebody restarted it. Neither failure is distinguishable from
// success in a health check (DESIGN.md 13.4).
//
// Nothing here creates a directory (invariant 19), foreign keys are on for
// every connection of every store including the in-memory one, and persistent
// stores are in WAL (invariant 22).
//
// Permitted imports: the standard library, zombiezen.com/go/sqlite,
// internal/domain, and internal/migrate.
package store
