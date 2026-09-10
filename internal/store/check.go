// Copyright (c) 2026 Michael D Henderson.

package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

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

	// StuckJobLeases is the number of jobs holding a lease that expired
	// before the instant the check was run (PLAN.md M6). It is not damage:
	// it is what a worker that died looks like from the outside, and the
	// queue recovers on its own because an expired lease is not a lease.
	// What it tells an operator is that something killed a worker and did
	// not restart it.
	StuckJobLeases int

	// OrphanedResources is how many ways the output tree and
	// published_resources disagree: rows whose file is gone plus files no row
	// claims (PLAN.md M9 acceptance 7).
	//
	// It is filled in by the caller and not by Check, and that is deliberate.
	// Check reads the database and nothing else; reconciling two sets of
	// paths needs the output tree, which is a directory this package has
	// never heard of and has no business opening. "cmsdb check --output DIR"
	// runs publish.Reconcile and writes the answer here, and a check run
	// without --output leaves it at zero and says so.
	OrphanedResources int

	// MissingFiles are the paths published_resources names that are not in
	// the output tree, and UnknownFiles are the paths in the tree that no row
	// claims. They are two lists rather than one count because they are
	// opposite mistakes: the first is a page the system believes it is
	// serving and is not, and the second is a page nothing will ever expire.
	MissingFiles []string
	UnknownFiles []string
}

// OK reports whether the check found nothing wrong.
//
// A stuck lease is deliberately not counted here. It is a fact worth
// reporting and not a fault: the row is intact, the queue will claim the job
// again when the lease expires, and a check that exited non-zero for it would
// turn an ordinary worker restart into a page. Damage is what OK is about,
// and damage is what the two lists above hold.
func (r *CheckReport) OK() bool {
	return len(r.ForeignKeyViolations) == 0 &&
		len(r.IntegrityProblems) == 0 &&
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
//
// now is what a job lease is judged expired against. It is a parameter rather
// than a clock this package reads, for the reason every instant here is
// (invariant 3): "cmsdb check" holds the real clock and hands it down, and a
// test can ask what the check would have said an hour later.
func (db *DB) Check(ctx context.Context, now time.Time) (*CheckReport, error) {
	var report *CheckReport
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		var err error
		report, err = checkConn(conn, db.path, now)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("checking %q: %w", db.path, err)
	}
	return report, nil
}

// CheckFile runs the same checks against a database file directly, without the
// DIR/cms.db convention every other entry point here uses.
//
// It exists for the one database in this system that is not called cms.db in a
// directory somebody named with --db: a backup, whose whole point is a
// distinguishing name with a date in it (#10). Verifying one used to mean
// moving it into a temporary directory under the expected name first.
//
// The connection is read-only and the journal mode is not checked. A file
// written by VACUUM INTO comes out in rollback-journal mode whatever the
// source was in, so requiring WAL here — as Open does, for a database this
// system is about to serve from — would reject every backup it takes.
func CheckFile(ctx context.Context, path string, now time.Time) (*CheckReport, error) {
	if path == "" {
		return nil, errors.New("no database file given")
	}
	// Stat first, so that a missing file is a message naming it rather than
	// SQLITE_CANTOPEN or, worse, NotFoundError's advice to run "cmsdb init"
	// against a directory nobody asked about.
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("database file %q: %w", path, err)
	}

	conn, err := sqlite.OpenConn(path, readFlags)
	if err != nil {
		return nil, fmt.Errorf("opening %q: %w", path, err)
	}
	defer func() { _ = conn.Close() }()
	if err := prepareConn(conn, DefaultBusyTimeout); err != nil {
		return nil, fmt.Errorf("opening %q: %w", path, err)
	}

	// The two markers, refused rather than merely reported: a file that is not
	// this system's database gets the same message here as it would from any
	// other cmsdb command, because it is the same check (DESIGN.md 13.4).
	if _, err := verify(conn, filepath.Dir(path), path, RequireExact); err != nil {
		return nil, err
	}

	report, err := checkConn(conn, path, now)
	if err != nil {
		return nil, fmt.Errorf("checking %q: %w", path, err)
	}
	return report, nil
}

// checkConn is the whole of a check, on one connection. Check and CheckFile
// differ in where the connection comes from and in nothing else, so the
// statements live here once.
func checkConn(conn *sqlite.Conn, path string, now time.Time) (*CheckReport, error) {
	report := &CheckReport{Path: path, Migrations: migrate.Count()}

	var err error
	if report.AppID, err = pragmaInt32(conn, "application_id"); err != nil {
		return nil, err
	}
	if report.SchemaVersion, err = pragmaInt32(conn, "user_version"); err != nil {
		return nil, err
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
		return nil, fmt.Errorf("foreign_key_check: %w", err)
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
		return nil, fmt.Errorf("integrity_check: %w", err)
	}

	// The application-level check DESIGN.md 11 asks for. The query lives
	// beside the rest of the queue's SQL, in jobs.go, so that there is one
	// statement of what "stuck" means.
	if report.StuckJobLeases, err = stuckLeases(conn, now); err != nil {
		return nil, err
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
