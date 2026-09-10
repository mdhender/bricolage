// Copyright (c) 2026 Michael D Henderson.

package store

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/mdhender/bricolage/internal/migrate"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// These are #11: a graceful close left the write-ahead log on disk, because
// the read-only pool closed after the writer and a read-only connection cannot
// checkpoint. They run against a file, since a write-ahead log is the one
// thing an in-memory database provably does not have.

// TestCloseTruncatesTheWriteAheadLog is the symptom, asserted directly.
//
// A reader is taken from the pool first, because that is what the bug needed:
// until a reader has been used the pool holds no connection, the writer is the
// last one standing anyway, and SQLite checkpoints on its way out. The
// reproduction is a server that has served a request.
func TestCloseTruncatesTheWriteAheadLog(t *testing.T) {
	dir := t.TempDir()
	db, err := Create(t.Context(), dir)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fillWAL(t, db, 200)
	readOnce(t, db)

	wal := filepath.Join(dir, dbFileName+"-wal")
	if before := sizeOf(t, wal); before <= 0 {
		t.Fatalf("%s is %d bytes before the close; the test is not reproducing the condition", wal, before)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ck := db.CloseCheckpoint()
	if !ck.Truncated() {
		t.Errorf("CloseCheckpoint() = %+v, want a truncated write-ahead log", ck)
	}
	if got := sizeOf(t, wal); got > 0 {
		t.Errorf("%s is %d bytes after a graceful close, want absent or empty", wal, got)
	}
}

// TestTheDatabaseFileAloneIsCompleteAfterClose is why the symptom matters.
//
// deploy/README.md tells an operator to back up before migrating and the
// natural thing to copy is cms.db, so cms.db on its own has to carry
// everything that was committed. Copying only that file into a directory of
// its own is exactly that backup, and the copy has to open and read back what
// the original wrote.
func TestTheDatabaseFileAloneIsCompleteAfterClose(t *testing.T) {
	dir := t.TempDir()
	db, err := Create(t.Context(), dir)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	const rows = 200
	fillWAL(t, db, rows)
	readOnce(t, db)
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	backup := t.TempDir()
	copyFile(t, filepath.Join(dir, dbFileName), filepath.Join(backup, dbFileName))

	restored, err := Open(t.Context(), backup, Options{})
	if err != nil {
		t.Fatalf("opening a copy of %s alone: %v", dbFileName, err)
	}
	t.Cleanup(func() { _ = restored.Close() })

	if got, want := restored.SchemaVersion(), int32(migrate.Count()); got != want {
		t.Errorf("the copy is at user_version %d, want %d", got, want)
	}
	if got := countEvents(t, restored); got != rows {
		t.Errorf("the copy holds %d of the %d rows committed before the close", got, rows)
	}
	if err := integrityCheck(t, restored); err != nil {
		t.Errorf("integrity_check on the copy: %v", err)
	}
}

// TestCloseCheckpointsAnInMemoryDatabaseHarmlessly covers the store every
// other package's tests use. PRAGMA wal_checkpoint on a database that is not
// in WAL mode is a documented no-op reporting -1, and a no-op is not a
// failure: there is no log to leave behind, so Truncated is true.
func TestCloseCheckpointsAnInMemoryDatabaseHarmlessly(t *testing.T) {
	db, err := OpenMemory(t.Context())
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ck := db.CloseCheckpoint()
	if ck.Err != nil {
		t.Errorf("CloseCheckpoint().Err = %v, want the pragma to be a harmless no-op", ck.Err)
	}
	if !ck.Truncated() {
		t.Errorf("CloseCheckpoint() = %+v, want Truncated for a database with no write-ahead log", ck)
	}
}

// TestCloseCheckpointIsZeroBeforeClose keeps the accessor honest: the zero
// Checkpoint is a checkpoint that never ran, and it must not read as success.
func TestCloseCheckpointIsZeroBeforeClose(t *testing.T) {
	db := memoryStore(t)
	if ck := db.CloseCheckpoint(); ck.Ran || ck.Truncated() {
		t.Errorf("CloseCheckpoint() = %+v before Close, want the zero value", ck)
	}
}

// fillWAL commits n rows one transaction at a time, which is what puts pages
// in the log rather than in the database file.
func fillWAL(t *testing.T, db *DB, n int) {
	t.Helper()
	for i := range n {
		err := db.Write(t.Context(), func(conn *sqlite.Conn) error {
			return sqlitex.Execute(conn,
				"INSERT INTO events (type, subject_kind, subject_id, occurred_at)"+
					" VALUES ('test.filled', 'test', ?, '2026-01-01T00:00:00.000Z');",
				&sqlitex.ExecOptions{Args: []any{i}})
		})
		if err != nil {
			t.Fatalf("filling the write-ahead log: %v", err)
		}
	}
}

// readOnce takes a connection out of the reader pool and puts it back, so that
// the pool is holding an open read-only connection when Close runs.
func readOnce(t *testing.T, db *DB) {
	t.Helper()
	err := db.Read(t.Context(), func(conn *sqlite.Conn) error {
		return sqlitex.Execute(conn, "SELECT count(*) FROM events;", nil)
	})
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
}

func countEvents(t *testing.T, db *DB) int {
	t.Helper()
	var n int
	err := db.Read(t.Context(), func(conn *sqlite.Conn) error {
		return sqlitex.Execute(conn, "SELECT count(*) FROM events WHERE type = 'test.filled';",
			&sqlitex.ExecOptions{
				ResultFunc: func(stmt *sqlite.Stmt) error {
					n = stmt.ColumnInt(0)
					return nil
				},
			})
	})
	if err != nil {
		t.Fatalf("counting: %v", err)
	}
	return n
}

func integrityCheck(t *testing.T, db *DB) error {
	t.Helper()
	var bad []string
	err := db.Read(t.Context(), func(conn *sqlite.Conn) error {
		return sqlitex.ExecuteTransient(conn, "PRAGMA integrity_check;", &sqlitex.ExecOptions{
			ResultFunc: func(stmt *sqlite.Stmt) error {
				if got := stmt.ColumnText(0); got != "ok" {
					bad = append(bad, got)
				}
				return nil
			},
		})
	})
	if err != nil {
		return err
	}
	if len(bad) > 0 {
		return fmt.Errorf("%v", bad)
	}
	return nil
}

// sizeOf is the size of path in bytes, or -1 if it is not there. A
// write-ahead log that was truncated may be either absent or empty, depending
// on the platform, and both are the answer this system wants.
func sizeOf(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if os.IsNotExist(err) {
		return -1
	}
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Size()
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatalf("reading %s: %v", from, err)
	}
	if err := os.WriteFile(to, b, 0o600); err != nil {
		t.Fatalf("writing %s: %v", to, err)
	}
}
