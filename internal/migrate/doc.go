// Copyright (c) 2026 Michael D Henderson.

// Package migrate holds the embedded schema migrations and applies them
// through zombiezen.com/go/sqlite/sqlitemigration, which tracks the schema
// version in PRAGMA user_version and stamps the application ID
// (DESIGN.md 13.2). It defines that application ID: 0x434D5330, the ASCII
// bytes of "CMS0".
//
// Permitted imports: the standard library, zombiezen.com/go/sqlite.
//
// Empty until M1.
package migrate
