// Copyright (c) 2026 Michael D Henderson.

package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/mdhender/bricolage/internal/migrate"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// CheckReport is what "cmsdb check" found. It is a value rather than printed
// output so that the command formats it and a test asserts on it.
type CheckReport struct {
	// Path is the database that was checked.
	Path string

	// AppID and SchemaVersion are the two markers this system keeps, reported
	// so that an operator can see what they are without knowing the pragmas.
	AppID         int32
	SchemaVersion int32

	// Migrations is the number this binary embeds. It equals SchemaVersion on
	// a healthy database, because check opens with RequireExact.
	Migrations int

	// ForeignKeyViolations is one entry per row PRAGMA foreign_key_check
	// returned.
	ForeignKeyViolations []ForeignKeyViolation

	// IntegrityProblems is what PRAGMA integrity_check said, with the single
	// "ok" row removed. Empty means the database is structurally sound.
	IntegrityProblems []string

	// StuckJobLeases and OrphanedResources are the two application-level
	// checks DESIGN.md 11 asks of "cmsdb check". Both are zero until the
	// tables exist: jobs arrive in M6 and published resources in M9. They are
	// reported rather than omitted so that the report does not change shape
	// when they do.
	StuckJobLeases    int
	OrphanedResources int
}

// OK reports whether the check found nothing wrong.
func (r *CheckReport) OK() bool {
	return len(r.ForeignKeyViolations) == 0 &&
		len(r.IntegrityProblems) == 0 &&
		r.StuckJobLeases == 0 &&
		r.OrphanedResources == 0
}

// ForeignKeyViolation is one row of PRAGMA foreign_key_check: a child row that
// points at a parent that is not there.
type ForeignKeyViolation struct {
	Table  string
	RowID  int64
	Parent string
	FKID   int64
}

func (v ForeignKeyViolation) String() string {
	return fmt.Sprintf("%s rowid %d references a missing row in %s (foreign key %d)",
		v.Table, v.RowID, v.Parent, v.FKID)
}

// Check runs the integrity checks (DESIGN.md 11).
//
// It reads, so it runs on a reader connection: a check that took the write
// connection would queue behind whatever the server is doing, and this is a
// question about the file rather than an operation on it.
func (db *DB) Check(ctx context.Context) (*CheckReport, error) {
	report := &CheckReport{Path: db.path, Migrations: migrate.Count()}

	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		var err error
		if report.AppID, err = pragmaInt32(conn, "application_id"); err != nil {
			return err
		}
		if report.SchemaVersion, err = pragmaInt32(conn, "user_version"); err != nil {
			return err
		}

		err = sqlitex.ExecuteTransient(conn, "PRAGMA foreign_key_check;", &sqlitex.ExecOptions{
			ResultFunc: func(stmt *sqlite.Stmt) error {
				report.ForeignKeyViolations = append(report.ForeignKeyViolations, ForeignKeyViolation{
					Table:  stmt.ColumnText(0),
					RowID:  stmt.ColumnInt64(1),
					Parent: stmt.ColumnText(2),
					FKID:   stmt.ColumnInt64(3),
				})
				return nil
			},
		})
		if err != nil {
			return fmt.Errorf("foreign_key_check: %w", err)
		}

		err = sqlitex.ExecuteTransient(conn, "PRAGMA integrity_check;", &sqlitex.ExecOptions{
			ResultFunc: func(stmt *sqlite.Stmt) error {
				// A sound database answers with the single row "ok".
				if line := strings.TrimSpace(stmt.ColumnText(0)); line != "ok" && line != "" {
					report.IntegrityProblems = append(report.IntegrityProblems, line)
				}
				return nil
			},
		})
		if err != nil {
			return fmt.Errorf("integrity_check: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("checking %q: %w", db.path, err)
	}
	return report, nil
}

// Vacuum rebuilds the database file, reclaiming space.
//
// It goes through the write connection because it rewrites the file, and it
// cannot run inside a transaction, which is why Write hands out a connection
// rather than opening one.
func (db *DB) Vacuum(ctx context.Context) error {
	err := db.Write(ctx, func(conn *sqlite.Conn) error {
		return sqlitex.ExecuteTransient(conn, "VACUUM;", nil)
	})
	if err != nil {
		return fmt.Errorf("vacuuming %q: %w", db.path, err)
	}
	return nil
}
