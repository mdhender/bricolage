// Copyright (c) 2026 Michael D Henderson.

package store

import (
	"errors"
	"fmt"
	"time"

	"github.com/mdhender/bricolage/internal/domain"
	"zombiezen.com/go/sqlite"
)

// This file holds the small pieces every other file in this package uses: the
// timestamp format, the nullable-column helpers, and the constraint-violation
// classifier.

// timeFormat is how an instant is written to a TEXT column: ISO-8601 in UTC,
// to the millisecond, with a trailing Z (DESIGN.md 13.5).
//
// It is fixed width and it sorts lexicographically in the same order it sorts
// chronologically, which is what makes "expires_at < ?" and ORDER BY work in
// SQL without a function call on the column.
const timeFormat = "2006-01-02T15:04:05.000Z"

// formatTime renders an instant for storage.
func formatTime(t time.Time) string { return t.UTC().Format(timeFormat) }

// parseTime reads an instant back. A column this system wrote always parses;
// one edited by hand may not, and the error names the value rather than
// producing a zero time that looks like the epoch.
func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(timeFormat, s)
	if err != nil {
		// Accept the wider RFC 3339 as well, so a value written by a human
		// with "sqlite3" is read rather than rejected.
		if t2, err2 := time.Parse(time.RFC3339Nano, s); err2 == nil {
			return t2.UTC(), nil
		}
		return time.Time{}, fmt.Errorf("timestamp %q: %w", s, err)
	}
	return t.UTC(), nil
}

// The nullable-column helpers. NULL is the wildcard in a grant scope
// (DESIGN.md 7), so reading and writing "absent" correctly is not an edge case
// here; it is the common case.

func bindNullInt64(stmt *sqlite.Stmt, param string, v *int64) {
	if v == nil {
		stmt.SetNull(param)
		return
	}
	stmt.SetInt64(param, *v)
}

func bindNullText(stmt *sqlite.Stmt, param string, v *string) {
	if v == nil {
		stmt.SetNull(param)
		return
	}
	stmt.SetText(param, *v)
}

// nullInt64 reads a column that may be NULL. It returns nil for NULL, which is
// the wildcard, and never 0 -- 0 is a foreign key that matches nothing.
func nullInt64(stmt *sqlite.Stmt, col string) *int64 {
	if stmt.IsNull(col) {
		return nil
	}
	v := stmt.GetInt64(col)
	return &v
}

// nullText reads a text column that may be NULL.
func nullText(stmt *sqlite.Stmt, col string) *string {
	if stmt.IsNull(col) {
		return nil
	}
	v := stmt.GetText(col)
	return &v
}

// ConstraintError reports a row rejected by a constraint: a UNIQUE index, a
// foreign key, a CHECK, a NOT NULL.
//
// It is built from the result code and never from the message text
// (invariant 11). Message text is a property of the SQLite build; a caller
// that greps it works until somebody upgrades the library.
type ConstraintError struct {
	// What names the operation in the words of the caller, so the message
	// reads as something that happened rather than as a code.
	What string

	// Code is the extended result code, which is what says which constraint.
	Code sqlite.ResultCode

	Err error
}

func (e *ConstraintError) Error() string {
	return fmt.Sprintf("%s: %v", e.What, e.Err)
}

func (e *ConstraintError) Unwrap() error { return e.Err }

// Is makes a constraint violation answer to domain.ErrConflict, so the
// transport edge maps it with the one mapping function it already has and no
// caller has to know that SQLite was involved.
func (e *ConstraintError) Is(target error) bool { return target == domain.ErrConflict }

// IsUnique reports whether the violated constraint was a UNIQUE index or a
// primary key.
func (e *ConstraintError) IsUnique() bool {
	return e.Code == sqlite.ResultConstraintUnique || e.Code == sqlite.ResultConstraintPrimaryKey
}

// IsForeignKey reports whether the violated constraint was a foreign key.
func (e *ConstraintError) IsForeignKey() bool {
	return e.Code == sqlite.ResultConstraintForeignKey
}

// constraint classifies err, returning a *ConstraintError when SQLite refused
// the row on a constraint and err unchanged otherwise.
//
// The classification is by result code (invariant 11). sqlite.ErrCode reports
// the extended code, whose primary is SQLITE_CONSTRAINT for every constraint
// and whose extension says which one.
func constraint(what string, err error) error {
	if err == nil {
		return nil
	}
	code := sqlite.ErrCode(err)
	if code.ToPrimary() != sqlite.ResultConstraint {
		return fmt.Errorf("%s: %w", what, err)
	}
	return &ConstraintError{What: what, Code: code, Err: err}
}

// AsConstraint reports whether err is a constraint violation, and which.
func AsConstraint(err error) (*ConstraintError, bool) {
	var ce *ConstraintError
	ok := errors.As(err, &ce)
	return ce, ok
}

// notFound wraps domain.ErrNotFound with what was looked for, so the message
// an operator reads names the thing rather than the table.
func notFound(what string) error {
	return fmt.Errorf("%s: %w", what, domain.ErrNotFound)
}

// run prepares query on conn, lets bind set its parameters, steps it to
// completion, and calls row for each result row.
//
// It exists because the reflective named-argument form in sqlitex cannot bind
// NULL: it maps a Go value to a bind call by kind, and a nil interface falls
// through to fmt.Sprint, which writes the four characters "<nil>" into the
// column. NULL is the wildcard in a grant scope (DESIGN.md 7), so writing it
// correctly is not an edge case in this package -- it is most of the work.
//
// The statement is the connection's cached one, so it is reset and cleared on
// the way in as well as on the way out: a leftover binding from the previous
// call is a bug that shows up as the wrong row, not as an error.
func run(conn *sqlite.Conn, what, query string, bind func(*sqlite.Stmt), row func(*sqlite.Stmt) error) error {
	stmt, err := conn.Prepare(query)
	if err != nil {
		return fmt.Errorf("%s: preparing: %w", what, err)
	}
	defer stmt.Reset()

	if err := stmt.Reset(); err != nil {
		return fmt.Errorf("%s: resetting: %w", what, err)
	}
	if err := stmt.ClearBindings(); err != nil {
		return fmt.Errorf("%s: clearing bindings: %w", what, err)
	}
	if bind != nil {
		bind(stmt)
	}

	for {
		hasRow, err := stmt.Step()
		if err != nil {
			return constraint(what, err)
		}
		if !hasRow {
			return nil
		}
		if row == nil {
			continue
		}
		if err := row(stmt); err != nil {
			return err
		}
	}
}

// one steps a query that must return exactly one row, and reports notFound
// when it returns none.
func one(conn *sqlite.Conn, what, query string, bind func(*sqlite.Stmt), row func(*sqlite.Stmt) error) error {
	found := false
	err := run(conn, what, query, bind, func(stmt *sqlite.Stmt) error {
		if found {
			return fmt.Errorf("%s: more than one row", what)
		}
		found = true
		return row(stmt)
	})
	if err != nil {
		return err
	}
	if !found {
		return notFound(what)
	}
	return nil
}
