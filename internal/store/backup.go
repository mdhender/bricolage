// Copyright (c) 2026 Michael D Henderson.

package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// partialSuffix names the file a backup is written to before it is verified.
//
// A backup appears under the name an operator asked for only once it has been
// written in full and checked, so a crash halfway through leaves a file nobody
// will mistake for a backup, and --overwrite never destroys the backup already
// there until there is a good one to put in its place. That ordering is the
// whole point of the command: a backup command whose failure mode is "you now
// have neither" would be the same class of mistake as the one that prompted
// it (#10, #11).
const partialSuffix = ".partial"

// BackupOptions is how a backup differs from the default.
type BackupOptions struct {
	// Overwrite permits replacing a file that is already there. Without it an
	// existing target is an ExistsError.
	//
	// The default is a refusal because the documented backup name carries a
	// date: running a deploy twice in one day would otherwise replace the
	// backup taken before the first attempt, which is the one wanted after the
	// second goes wrong.
	Overwrite bool

	// Now is what a job lease is judged expired against while the backup is
	// verified. It is a parameter for the reason every instant in this package
	// is one (invariant 3).
	Now time.Time
}

// BackupReport is the file a backup wrote, and what checking it found.
type BackupReport struct {
	// Path is the file, under the name that was asked for.
	Path string

	// Size is its size in bytes. VACUUM INTO compacts, so a backup is
	// routinely smaller than the database it came from.
	Size int64

	// Check is the verification of the file that was written, not of the
	// database it came from. A backup is a file nobody reads until the day it
	// matters, and that is the wrong day to find out it is zero bytes.
	Check *CheckReport
}

// Backup writes a consistent snapshot of the database to path and verifies it.
//
// It runs while cmsd is serving. VACUUM INTO takes its own read transaction,
// so what lands is the database as of one instant with nothing half-applied,
// and it reads through the write-ahead log rather than around it — which is
// what a three-file "cp" of a live database does not do (DESIGN.md 13.1).
// It goes out through a reader connection, so a backup does not queue behind
// whatever the server is writing.
//
// The snapshot comes out compacted and in rollback-journal mode whatever the
// source was in, which is why CheckFile and not Open is what verifies it.
//
// Restoring is deliberately not here and deliberately not a command. Copying a
// file over a database a server is holding is not an operation worth making
// convenient; the asymmetry is the point.
func (db *DB) Backup(ctx context.Context, path string, opts BackupOptions) (*BackupReport, error) {
	if path == "" {
		return nil, errors.New("no backup file given")
	}

	// The directory holding it must already exist. A backup command is exactly
	// where somebody would be tempted to make an exception — "mkdir -p
	// $(dirname FILE)" looks like a courtesy — and it is the same exception
	// that turns a mistyped path into a plausible-looking empty system
	// (invariant 19).
	dir := filepath.Dir(path)
	if err := checkDir(dir); err != nil {
		return nil, err
	}

	switch info, err := os.Stat(path); {
	case err == nil && info.IsDir():
		return nil, fmt.Errorf("backup file %q is a directory", path)
	case err == nil && !opts.Overwrite:
		return nil, &ExistsError{Path: path}
	case err == nil:
		// Overwriting, which is still refused if the target is the database
		// being backed up: VACUUM INTO would not write it, but the removal
		// below would already have happened.
		if same, err := sameFile(info, db.path); err != nil {
			return nil, err
		} else if same {
			return nil, fmt.Errorf("backup file %q is the database being backed up", path)
		}
	case !errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("backup file %q: %w", path, err)
	}

	// A partial left by an interrupted run would otherwise fail the vacuum
	// with "output file already exists", which is a message about our own
	// scratch file rather than about anything the operator did.
	partial := path + partialSuffix
	if err := os.Remove(partial); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("removing a partial backup %q: %w", partial, err)
	}

	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		// Bound rather than interpolated: a path may contain a quote, and a
		// backup directory is a string somebody types.
		return sqlitex.Execute(conn, "VACUUM INTO ?;", &sqlitex.ExecOptions{Args: []any{partial}})
	})
	if err != nil {
		_ = os.Remove(partial)
		return nil, fmt.Errorf("backing up %q to %q: %w", db.path, path, err)
	}

	// Verified before it is given the name it was asked for. A file that does
	// not open, or that opens and is not this system's database, never becomes
	// somebody's backup.
	report, err := CheckFile(ctx, partial, opts.Now)
	if err != nil {
		_ = os.Remove(partial)
		return nil, fmt.Errorf("verifying the backup of %q: %w", db.path, err)
	}
	if !report.OK() {
		_ = os.Remove(partial)
		return nil, fmt.Errorf("the backup of %q is not sound: %s", db.path, problems(report))
	}

	info, err := os.Stat(partial)
	if err != nil {
		_ = os.Remove(partial)
		return nil, fmt.Errorf("backup file %q: %w", partial, err)
	}
	if err := os.Rename(partial, path); err != nil {
		_ = os.Remove(partial)
		return nil, fmt.Errorf("naming the backup %q: %w", path, err)
	}
	report.Path = path

	return &BackupReport{Path: path, Size: info.Size(), Check: report}, nil
}

// sameFile reports whether info describes the same file as path. It is the
// question "is this the database I am reading" asked the way the filesystem
// answers it, rather than by comparing two strings that may differ by a
// symlink or a trailing element.
func sameFile(info os.FileInfo, path string) (bool, error) {
	other, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("database file %q: %w", path, err)
	}
	return os.SameFile(info, other), nil
}

// problems renders what made a check fail, for an error message.
func problems(r *CheckReport) string {
	var out []string
	for _, v := range r.ForeignKeyViolations {
		out = append(out, v.String())
	}
	out = append(out, r.IntegrityProblems...)
	if len(out) == 0 {
		return "no detail reported"
	}
	return fmt.Sprint(out)
}
