// Copyright (c) 2026 Michael D Henderson.

// Package migrate holds the embedded schema migrations and applies them
// through zombiezen.com/go/sqlite/sqlitemigration, which tracks the schema
// version in PRAGMA user_version and stamps the application ID
// (DESIGN.md 13.2). It defines that application ID: 0x434D5330, the ASCII
// bytes of "CMS0".
//
// The migration files live in schema/, beside this package, rather than at the
// repository root as DESIGN.md 4 first drew them: go:embed cannot reach
// outside the directory of the package that declares it, and PLAN.md M1 asks
// for "//go:embed schema/*.sql" in this package. The design document has been
// corrected to match.
//
// There is deliberately no schema_migrations table and no other hand-rolled
// version bookkeeping. The pragmas are the record; a second one is a second
// thing that can disagree with the database.
//
// Permitted imports: the standard library, zombiezen.com/go/sqlite.
package migrate
