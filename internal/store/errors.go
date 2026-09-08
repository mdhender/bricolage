// Copyright (c) 2026 Michael D Henderson.

package store

import (
	"fmt"

	"github.com/mdhender/bricolage/internal/migrate"
)

// The errors below are the messages an operator reads at three in the morning
// (PLAN.md M1 acceptance 13). Each one names the path it opened, what it
// expected, and what it found, and each one says what to do about it. They are
// types rather than formatted strings so that a test can assert on the values
// without matching prose, and so that a caller can tell "behind" from "ahead"
// without parsing anything.

// DirError reports a --db directory that is missing or is not a directory.
//
// Nothing in this system creates a directory (invariant 19). A mistyped path
// is the most common way to end up with a second, empty database that looks
// exactly like the first, so this is a hard failure rather than a mkdir.
type DirError struct {
	Dir string
	Err error
}

func (e *DirError) Error() string {
	return fmt.Sprintf("database directory %q: %v (nothing in this system creates a directory; create it yourself and try again)", e.Dir, e.Err)
}

func (e *DirError) Unwrap() error { return e.Err }

// NotFoundError reports that the directory exists but holds no cms.db.
//
// It is detected by SQLite's result code rather than by matching the message
// (invariant 11): the open is made without OpenCreate, so a missing file is
// SQLITE_CANTOPEN.
type NotFoundError struct {
	Dir  string
	Path string
	Err  error
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("database %q does not exist: run \"cmsdb init --db %s\"", e.Path, e.Dir)
}

func (e *NotFoundError) Unwrap() error { return e.Err }

// ExistsError reports that "cmsdb init" was pointed at a directory that
// already holds a database. Creating one is init's job and only init's, and
// doing it twice is not creation.
type ExistsError struct {
	Path string
}

func (e *ExistsError) Error() string {
	return fmt.Sprintf("database %q already exists", e.Path)
}

// AppIDError reports a PRAGMA application_id that is not this system's.
//
// It catches three real cases with one check: an empty file, which reads as 0
// and which sqlitemigration would have adopted; a database belonging to
// another program; and a database belonging to a different CMS
// (DESIGN.md 13.2, invariant 21).
type AppIDError struct {
	Path string
	Want int32
	Got  int32
}

func (e *AppIDError) Error() string {
	return fmt.Sprintf("database %q: application_id is %s, expected %s; this file was not created by cmsdb init",
		e.Path, migrate.AppIDString(e.Got), migrate.AppIDString(e.Want))
}

// VersionError reports a PRAGMA user_version that does not match the number of
// migrations this binary embeds.
//
// Behind means a migration is pending; ahead means the binary is older than
// the database. Neither is cmsd's to repair: a server that migrates on startup
// upgrades a production database because somebody restarted it
// (DESIGN.md 13.4).
type VersionError struct {
	Dir  string
	Path string
	Want int32
	Got  int32
}

// Behind reports whether the database is older than the binary, which is the
// case an operator fixes with "cmsdb migrate up".
func (e *VersionError) Behind() bool { return e.Got < e.Want }

func (e *VersionError) Error() string {
	remedy := "deploy the binary that matches it, or rebuild the database"
	if e.Behind() {
		remedy = fmt.Sprintf("run \"cmsdb migrate up --db %s\"", e.Dir)
	}
	return fmt.Sprintf("database %q: user_version is %d, expected %d, which is the number of migrations this binary embeds: %s",
		e.Path, e.Got, e.Want, remedy)
}
