// Copyright (c) 2026 Michael D Henderson.

package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mdhender/bricolage/internal/migrate"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// These are #10: cmsdb had no backup command, and the deploy doc's sqlite3 is
// not installed on the host.

// TestBackupWritesAVerifiedSnapshot is the shape of the whole thing, and the
// part that matters most is that fillWAL is never checkpointed first.
//
// Commits live in the write-ahead log until something moves them, so a backup
// that read only cms.db would come back missing every row written here. That
// is the failure #11 turned out to be, arriving from the other direction.
func TestBackupWritesAVerifiedSnapshot(t *testing.T) {
	dir := t.TempDir()
	db, err := Create(t.Context(), dir)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	const rows = 200
	fillWAL(t, db, rows)
	readOnce(t, db)
	if n := sizeOf(t, filepath.Join(dir, dbFileName+"-wal")); n <= 0 {
		t.Fatalf("cms.db-wal is %d bytes; the snapshot would not be proving anything", n)
	}

	to := filepath.Join(t.TempDir(), "cms-2026-09-09.db")
	report, err := db.Backup(t.Context(), to, BackupOptions{Now: time.Now().UTC()})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	if report.Path != to {
		t.Errorf("Path = %q, want %q", report.Path, to)
	}
	if report.Size <= 0 {
		t.Errorf("Size = %d", report.Size)
	}
	if got := sizeOf(t, to); got != report.Size {
		t.Errorf("the file is %d bytes and the report says %d", got, report.Size)
	}
	if !report.Check.OK() {
		t.Errorf("the backup did not verify: %+v", report.Check)
	}
	if want := int32(migrate.Count()); report.Check.SchemaVersion != want {
		t.Errorf("user_version = %d, want %d", report.Check.SchemaVersion, want)
	}
	if report.Check.AppID != migrate.AppID {
		t.Errorf("application_id = %#x, want %#x", report.Check.AppID, migrate.AppID)
	}

	// The rows, which is what a backup is for.
	if got := rowsIn(t, to); got != rows {
		t.Errorf("the snapshot holds %d of the %d rows, which were still in the write-ahead log when it was taken", got, rows)
	}

	// And nothing is left beside it.
	if n := sizeOf(t, to+partialSuffix); n >= 0 {
		t.Errorf("a %d-byte %s was left behind", n, to+partialSuffix)
	}
}

// TestBackupRunsWhileTheDatabaseIsBeingWritten is the point of VACUUM INTO
// over a cp: a backup nobody can take without an outage is a backup nobody
// takes between deploys.
func TestBackupRunsWhileTheDatabaseIsBeingWritten(t *testing.T) {
	dir := t.TempDir()
	db, err := Create(t.Context(), dir)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	fillWAL(t, db, 50)

	// Writes continue for as long as the backup takes.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 200 {
			_ = db.Write(t.Context(), func(conn *sqlite.Conn) error {
				return sqlitex.Execute(conn,
					"INSERT INTO events (type, subject_kind, subject_id, occurred_at)"+
						" VALUES ('test.concurrent', 'test', 1, '2026-01-01T00:00:00.000Z');", nil)
			})
		}
	}()

	to := filepath.Join(t.TempDir(), "live.db")
	report, err := db.Backup(t.Context(), to, BackupOptions{Now: time.Now().UTC()})
	<-done
	if err != nil {
		t.Fatalf("Backup while writing: %v", err)
	}
	if !report.Check.OK() {
		t.Errorf("a snapshot taken under write load did not verify: %+v", report.Check)
	}
	// Whatever it caught, it caught a whole transaction's worth: the fifty
	// rows committed before the backup started are all there.
	if got := rowsIn(t, to); got != 50 {
		t.Errorf("the snapshot holds %d of the 50 rows committed before it started", got)
	}
}

// TestBackupRefusesAnExistingFile is #10 acceptance 3. The documented backup
// name carries a date, so a second deploy in one day would otherwise replace
// the backup taken before the first attempt — the one wanted after the second
// goes wrong.
func TestBackupRefusesAnExistingFile(t *testing.T) {
	db, dir := fileStore(t)
	fillWAL(t, db, 20)

	to := filepath.Join(dir, "cms-2026-09-09.db")
	if _, err := db.Backup(t.Context(), to, BackupOptions{Now: time.Now().UTC()}); err != nil {
		t.Fatalf("first Backup: %v", err)
	}
	first := sizeOf(t, to)

	_, err := db.Backup(t.Context(), to, BackupOptions{Now: time.Now().UTC()})
	var exists *ExistsError
	if !errors.As(err, &exists) {
		t.Fatalf("second Backup = %v, want an *ExistsError", err)
	}
	if got := sizeOf(t, to); got != first {
		t.Errorf("the refused backup changed the file from %d to %d bytes", first, got)
	}

	// And --overwrite is what permits it.
	if _, err := db.Backup(t.Context(), to, BackupOptions{Overwrite: true, Now: time.Now().UTC()}); err != nil {
		t.Fatalf("Backup with Overwrite: %v", err)
	}
}

// TestBackupCreatesNoDirectory is #10 acceptance 2. A backup command is
// exactly where a mkdir looks like a courtesy (invariant 19).
func TestBackupCreatesNoDirectory(t *testing.T) {
	db, dir := fileStore(t)
	missing := filepath.Join(dir, "backups")

	_, err := db.Backup(t.Context(), filepath.Join(missing, "cms.db"), BackupOptions{Now: time.Now().UTC()})
	var dirErr *DirError
	if !errors.As(err, &dirErr) {
		t.Fatalf("Backup into a missing directory = %v, want a *DirError", err)
	}
	if dirErr.Dir != missing {
		t.Errorf("the error names %q, want %q", dirErr.Dir, missing)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%q exists after the refusal; nothing here creates a directory", missing)
	}
}

// TestBackupRefusesTheDatabaseItself covers the one way --overwrite could be
// aimed at something irreplaceable.
func TestBackupRefusesTheDatabaseItself(t *testing.T) {
	db, dir := fileStore(t)
	fillWAL(t, db, 20)
	source := filepath.Join(dir, dbFileName)
	before := sizeOf(t, source)

	if _, err := db.Backup(t.Context(), source, BackupOptions{Overwrite: true, Now: time.Now().UTC()}); err == nil {
		t.Fatal("Backup over the database it was reading succeeded")
	}
	if got := sizeOf(t, source); got != before {
		t.Errorf("the database went from %d to %d bytes", before, got)
	}
	if _, err := Open(t.Context(), dir, Options{}); err != nil {
		t.Errorf("the database no longer opens: %v", err)
	}
}

// TestBackupClearsAStalePartial is the interrupted run somebody retries. The
// partial is our own scratch name, so a leftover has to be replaced rather
// than reported as "output file already exists".
func TestBackupClearsAStalePartial(t *testing.T) {
	db, dir := fileStore(t)
	fillWAL(t, db, 20)

	to := filepath.Join(dir, "cms.backup.db")
	if err := os.WriteFile(to+partialSuffix, []byte("half a backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Backup(t.Context(), to, BackupOptions{Now: time.Now().UTC()}); err != nil {
		t.Fatalf("Backup with a stale partial beside it: %v", err)
	}
	if n := sizeOf(t, to+partialSuffix); n >= 0 {
		t.Errorf("the partial survived at %d bytes", n)
	}
}

// TestARestoredBackupOpens is the other half of the operation. Restore is a
// cp with the service stopped and deliberately not a command, so the thing to
// prove is that the file this writes is one cmsd can be pointed at: VACUUM
// INTO leaves it in rollback-journal mode, and Open requires WAL
// (invariant 22).
func TestARestoredBackupOpens(t *testing.T) {
	db, _ := fileStore(t)
	const rows = 40
	fillWAL(t, db, rows)

	to := filepath.Join(t.TempDir(), "cms-2026-09-09.db")
	if _, err := db.Backup(t.Context(), to, BackupOptions{Now: time.Now().UTC()}); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	restored := t.TempDir()
	copyFile(t, to, filepath.Join(restored, dbFileName))
	rdb, err := Open(t.Context(), restored, Options{})
	if err != nil {
		t.Fatalf("opening a restored backup: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	if got := countEvents(t, rdb); got != rows {
		t.Errorf("the restored database holds %d of the %d rows", got, rows)
	}
}

// TestCheckFileRefusesADatabaseThatIsNotOurs is #10 acceptance 4, on the side
// of it that is new: a file under any name gets the same application_id
// message every other entry point gives.
func TestCheckFileRefusesADatabaseThatIsNotOurs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "somebody-elses.db")
	conn, err := sqlite.OpenConn(path, createFlags)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteTransient(conn, "PRAGMA application_id = 12345;", nil); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = CheckFile(t.Context(), path, time.Now().UTC())
	var appErr *AppIDError
	if !errors.As(err, &appErr) {
		t.Fatalf("CheckFile on a foreign database = %v, want an *AppIDError", err)
	}
}

// TestCheckFileNamesAMissingFile keeps the message about the file the operator
// asked for, rather than NotFoundError's advice to run "cmsdb init" against a
// directory nobody mentioned.
func TestCheckFileNamesAMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-here.db")
	_, err := CheckFile(t.Context(), path, time.Now().UTC())
	if err == nil {
		t.Fatal("CheckFile on a missing file succeeded")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("CheckFile = %v, want it to unwrap to os.ErrNotExist", err)
	}
	var notFound *NotFoundError
	if errors.As(err, &notFound) {
		t.Error("CheckFile returned a *NotFoundError, whose remedy is \"cmsdb init\" against a --db directory")
	}
}

// fileStore is a store on a real file, which is what every test here needs:
// an in-memory database has no write-ahead log and nothing to snapshot.
func fileStore(t *testing.T) (*DB, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := Create(t.Context(), dir)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, dir
}

// rowsIn counts the filled rows in a database file, opened read-only under
// whatever name it has.
func rowsIn(t *testing.T, path string) int {
	t.Helper()
	conn, err := sqlite.OpenConn(path, readFlags)
	if err != nil {
		t.Fatalf("opening %q: %v", path, err)
	}
	defer func() { _ = conn.Close() }()
	var n int
	err = sqlitex.ExecuteTransient(conn, "SELECT count(*) FROM events WHERE type = 'test.filled';",
		&sqlitex.ExecOptions{
			ResultFunc: func(stmt *sqlite.Stmt) error {
				n = stmt.ColumnInt(0)
				return nil
			},
		})
	if err != nil {
		t.Fatalf("counting in %q: %v", path, err)
	}
	return n
}
