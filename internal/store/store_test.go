// Copyright (c) 2026 Michael D Henderson.

package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mdhender/bricolage/internal/clock"
	"github.com/mdhender/bricolage/internal/ids"
	"github.com/mdhender/bricolage/internal/migrate"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// TestCreate is PLAN.md M1 acceptance 1 and 4: init creates DIR/cms.db,
// running it twice is safe, and the result carries both markers.
func TestCreate(t *testing.T) {
	dir := t.TempDir()

	db, err := Create(t.Context(), dir)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if want := filepath.Join(dir, "cms.db"); db.Path() != want {
		t.Errorf("Path() = %q, want %q; the file name is a constant", db.Path(), want)
	}
	if _, err := os.Stat(db.Path()); err != nil {
		t.Fatalf("the database was not created: %v", err)
	}

	// Acceptance 4: read both pragmas from a second, independent connection.
	// The one Create used is not evidence about the file.
	conn := independent(t, db.Path())
	if got := readPragma(t, conn, "application_id"); got != migrate.AppID {
		t.Errorf("application_id = %#x, want %#x", got, migrate.AppID)
	}
	if got, want := readPragma(t, conn, "user_version"), int32(migrate.Count()); got != want {
		t.Errorf("user_version = %d, want %d", got, want)
	}

	// Acceptance 8: WAL on a persistent store.
	if got := readPragmaText(t, conn, "journal_mode"); got != "wal" {
		t.Errorf("journal_mode = %q, want \"wal\" (invariant 22)", got)
	}

	// Acceptance 1: running init twice is safe. Create refuses, and it is that
	// refusal cmsdb turns into "already initialised".
	var exists *ExistsError
	if _, err := Create(t.Context(), dir); !errors.As(err, &exists) {
		t.Errorf("the second Create returned %v, want an *ExistsError", err)
	}
}

// TestNothingCreatesADirectory is PLAN.md M1 acceptance 2 and invariant 19,
// for every entry point. The directory must still not exist afterwards.
func TestNothingCreatesADirectory(t *testing.T) {
	entries := map[string]func(dir string) error{
		"Create": func(dir string) error {
			db, err := Create(t.Context(), dir)
			if db != nil {
				_ = db.Close()
			}
			return err
		},
		"Open": func(dir string) error {
			db, err := Open(t.Context(), dir, Options{})
			if db != nil {
				_ = db.Close()
			}
			return err
		},
		"Exists": func(dir string) error {
			_, err := Exists(dir)
			return err
		},
	}

	for name, fn := range entries {
		t.Run(name, func(t *testing.T) {
			missing := filepath.Join(t.TempDir(), "does-not-exist")

			err := fn(missing)
			var dirErr *DirError
			if !errors.As(err, &dirErr) {
				t.Fatalf("%s(%q) = %v, want a *DirError", name, missing, err)
			}
			if !strings.Contains(err.Error(), missing) {
				t.Errorf("the error does not name the directory: %v", err)
			}
			if _, statErr := os.Stat(missing); !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("%s created %q; nothing in this system creates a directory", name, missing)
			}
		})
	}
}

// TestOpenRefusesAFile covers the other half of "--db names a directory".
func TestOpenRefusesAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var dirErr *DirError
	if _, err := Open(t.Context(), path, Options{}); !errors.As(err, &dirErr) {
		t.Errorf("Open on a regular file = %v, want a *DirError", err)
	}
}

// TestOpenMissingDatabase is PLAN.md M1 acceptance 10 at the store level: a
// directory with no cms.db is a failure, and the failure creates nothing.
//
// The detection is by result code — the open names no OpenCreate, so SQLite
// answers SQLITE_CANTOPEN — never by matching the message (invariant 11).
func TestOpenMissingDatabase(t *testing.T) {
	dir := t.TempDir()

	_, err := Open(t.Context(), dir, Options{})
	var notFound *NotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("Open = %v, want a *NotFoundError", err)
	}
	if code := sqlite.ErrCode(notFound.Err).ToPrimary(); code != sqlite.ResultCantOpen {
		t.Errorf("underlying result code = %v, want SQLITE_CANTOPEN", code)
	}
	// Acceptance 13: the message names the path it opened and what to do.
	for _, want := range []string{Path(dir), "cmsdb init", dir} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("a failed Open left %d files behind; it must create nothing", len(entries))
	}
}

// TestOpenWrongApplicationID is PLAN.md M1 acceptance 11, in both variants.
//
// The zero-length file is the one that matters: its application_id reads as 0,
// which is exactly the value sqlitemigration adopts for a database with no
// schema, so this check has to be ours (DESIGN.md 13.2, invariant 21).
func TestOpenWrongApplicationID(t *testing.T) {
	t.Run("zero-length file", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(Path(dir), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		assertAppIDError(t, dir, 0)
	})

	t.Run("another program's database", func(t *testing.T) {
		dir := t.TempDir()
		const otherID = int32(0x4f544852) // "OTHR"
		conn, err := sqlite.OpenConn(Path(dir), sqlite.OpenReadWrite|sqlite.OpenCreate|sqlite.OpenWAL)
		if err != nil {
			t.Fatal(err)
		}
		if err := sqlitex.ExecuteTransient(conn, "CREATE TABLE theirs (id INTEGER PRIMARY KEY);", nil); err != nil {
			t.Fatal(err)
		}
		if err := sqlitex.ExecuteTransient(conn, fmt.Sprintf("PRAGMA application_id = %d;", otherID), nil); err != nil {
			t.Fatal(err)
		}
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
		assertAppIDError(t, dir, otherID)
	})
}

func assertAppIDError(t *testing.T, dir string, got int32) {
	t.Helper()
	_, err := Open(t.Context(), dir, Options{})
	var appErr *AppIDError
	if !errors.As(err, &appErr) {
		t.Fatalf("Open = %v, want an *AppIDError", err)
	}
	if appErr.Got != got || appErr.Want != migrate.AppID {
		t.Errorf("AppIDError{Got: %#x, Want: %#x}, want {Got: %#x, Want: %#x}",
			appErr.Got, appErr.Want, got, migrate.AppID)
	}
	// Acceptance 13: expected, actual, and the path.
	for _, want := range []string{Path(dir), migrate.AppIDString(migrate.AppID), migrate.AppIDString(got)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

// TestOpenVersionMismatch is PLAN.md M1 acceptance 12: behind and ahead are
// both hard failures, and neither changes user_version. A store that migrated
// and then failed for another reason must not pass this test.
func TestOpenVersionMismatch(t *testing.T) {
	behind := int32(migrate.Count() - 1)
	ahead := int32(migrate.Count() + 1)

	for _, tc := range []struct {
		name    string
		version int32

		// genuine says the database can be produced by applying migrations
		// rather than by rewriting the pragma. "Behind" can be; "ahead" is a
		// database written by a newer binary, which this one cannot make.
		genuine bool
	}{
		{name: "one migration behind", version: behind, genuine: true},
		{name: "one migration ahead", version: ahead},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.genuine {
				createAt(t, dir, int(tc.version))
			} else {
				db, err := Create(t.Context(), dir)
				if err != nil {
					t.Fatal(err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				setUserVersion(t, Path(dir), tc.version)
			}

			_, err := Open(t.Context(), dir, Options{})
			var verErr *VersionError
			if !errors.As(err, &verErr) {
				t.Fatalf("Open = %v, want a *VersionError", err)
			}
			if verErr.Got != tc.version || verErr.Want != int32(migrate.Count()) {
				t.Errorf("VersionError{Got: %d, Want: %d}, want {Got: %d, Want: %d}",
					verErr.Got, verErr.Want, tc.version, migrate.Count())
			}
			if verErr.Behind() != (tc.version < int32(migrate.Count())) {
				t.Errorf("Behind() = %v for version %d", verErr.Behind(), tc.version)
			}

			// Acceptance 13: expected, actual, the path, and what to do.
			msg := err.Error()
			for _, want := range []string{
				Path(dir),
				fmt.Sprintf("user_version is %d", tc.version),
				fmt.Sprintf("expected %d", migrate.Count()),
			} {
				if !strings.Contains(msg, want) {
					t.Errorf("error %q does not contain %q", msg, want)
				}
			}
			if verErr.Behind() && !strings.Contains(msg, "cmsdb migrate up") {
				t.Errorf("error %q does not name the remedy", msg)
			}

			// Acceptance 12: the version afterwards is untouched.
			conn := independent(t, Path(dir))
			if got := readPragma(t, conn, "user_version"); got != tc.version {
				t.Errorf("user_version is %d after a refused Open, want %d unchanged", got, tc.version)
			}
		})
	}
}

// TestOpenAllowBehind is the other half of the policy: "cmsdb migrate status"
// and "cmsdb migrate up" must be able to open the database they are there to
// report on and repair. Ahead is still an error, because migrating forward
// does not make a binary newer.
func TestOpenAllowBehind(t *testing.T) {
	dir := t.TempDir()
	db, err := Create(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	setUserVersion(t, Path(dir), 0)
	behind, err := Open(t.Context(), dir, Options{Version: AllowBehind})
	if err != nil {
		t.Fatalf("Open with AllowBehind on a database at version 0: %v", err)
	}
	if behind.SchemaVersion() != 0 {
		t.Errorf("SchemaVersion() = %d, want 0", behind.SchemaVersion())
	}
	if err := behind.Close(); err != nil {
		t.Fatal(err)
	}

	setUserVersion(t, Path(dir), int32(migrate.Count()+1))
	var verErr *VersionError
	if _, err := Open(t.Context(), dir, Options{Version: AllowBehind}); !errors.As(err, &verErr) {
		t.Errorf("Open with AllowBehind on a database ahead of the binary = %v, want a *VersionError", err)
	}
}

// TestMigrateUp is what "cmsdb migrate up" does, including --to.
func TestMigrateUp(t *testing.T) {
	// An empty database, stamped and migrated no further: what an older
	// binary would have left behind.
	dir := t.TempDir()
	createAt(t, dir, 0)

	db, err := Open(t.Context(), dir, Options{Version: AllowBehind})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	before, after, err := db.Migrate(t.Context(), 1)
	if err != nil {
		t.Fatalf("Migrate to 1: %v", err)
	}
	if before != 0 || after != 1 {
		t.Errorf("Migrate(1) = (%d, %d), want (0, 1)", before, after)
	}

	before, after, err = db.Migrate(t.Context(), -1)
	if err != nil {
		t.Fatalf("Migrate to the end: %v", err)
	}
	if before != 1 || after != int32(migrate.Count()) {
		t.Errorf("Migrate(-1) = (%d, %d), want (1, %d)", before, after, migrate.Count())
	}
	if db.SchemaVersion() != int32(migrate.Count()) {
		t.Errorf("SchemaVersion() = %d after migrating, want %d", db.SchemaVersion(), migrate.Count())
	}

	// Idempotent: a second run has nothing to do.
	if before, after, err = db.Migrate(t.Context(), -1); err != nil || before != after {
		t.Errorf("a second Migrate = (%d, %d, %v), want no change", before, after, err)
	}
}

// TestMemoryStoreWritesAndReadsARow is PLAN.md M1 acceptance 7, first half: a
// real in-memory database with all migrations applied through the same runner
// cmsdb uses. Not a mock.
func TestMemoryStoreWritesAndReadsARow(t *testing.T) {
	db := memoryStore(t)

	c := clock.NewFake(time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC))
	uid := ids.MustNew(c.Now())

	err := db.Write(t.Context(), func(conn *sqlite.Conn) error {
		return sqlitex.Execute(conn,
			"INSERT INTO users (uid, email, name, created_at) VALUES (:uid, :email, :name, :created_at);",
			&sqlitex.ExecOptions{Named: map[string]any{
				":uid":        uid,
				":email":      "admin@example.com",
				":name":       "Admin",
				":created_at": c.Now().Format(timeFormat),
			}})
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	var gotEmail, gotCreated string
	err = db.Read(t.Context(), func(conn *sqlite.Conn) error {
		return sqlitex.Execute(conn, "SELECT email, created_at FROM users WHERE uid = :uid;",
			&sqlitex.ExecOptions{
				Named: map[string]any{":uid": uid},
				ResultFunc: func(stmt *sqlite.Stmt) error {
					gotEmail, gotCreated = stmt.ColumnText(0), stmt.ColumnText(1)
					return nil
				},
			})
	})
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if gotEmail != "admin@example.com" {
		t.Errorf("email = %q, want %q", gotEmail, "admin@example.com")
	}
	if want := "2026-02-03T04:05:06.000Z"; gotCreated != want {
		t.Errorf("created_at = %q, want %q (DESIGN.md 13.5)", gotCreated, want)
	}
}

// timeFormat is the timestamp shape DESIGN.md 13.5 requires: ISO-8601 UTC,
// sortable and comparable in SQL. It is here rather than exported because
// nothing outside store writes a timestamp column.
const timeFormat = "2006-01-02T15:04:05.000Z"

// TestForeignKeysAreOnEveryConnection is PLAN.md M1 acceptance 7, second half,
// and invariant 22. Foreign keys are a per-connection setting, so one
// connection that skips them is silently wrong for its whole life while the
// rest of the process looks correct. A test that passes with them off proves
// nothing, which is why the violating insert is checked by result code.
func TestForeignKeysAreOnEveryConnection(t *testing.T) {
	db := memoryStore(t)

	// Every reader in the pool, not just the first one handed out.
	var wg sync.WaitGroup
	errs := make(chan error, DefaultReadPoolSize)
	for range DefaultReadPoolSize {
		wg.Go(func() {
			errs <- db.Read(t.Context(), func(conn *sqlite.Conn) error {
				on, err := pragmaBool(conn, "foreign_keys")
				if err != nil {
					return err
				}
				if !on {
					return errors.New("PRAGMA foreign_keys is 0 on a reader drawn from the pool")
				}
				// Hold the connection so the next goroutine gets a different
				// one; otherwise this checks one connection eight times.
				time.Sleep(5 * time.Millisecond)
				return nil
			})
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}

	err := db.Write(t.Context(), func(conn *sqlite.Conn) error {
		on, err := pragmaBool(conn, "foreign_keys")
		if err != nil {
			return err
		}
		if !on {
			return errors.New("PRAGMA foreign_keys is 0 on the writer")
		}
		return nil
	})
	if err != nil {
		t.Error(err)
	}

	// A violating insert is rejected, and it is rejected by result code
	// (invariant 11), not by the text of the message.
	err = db.Write(t.Context(), func(conn *sqlite.Conn) error {
		return sqlitex.Execute(conn,
			"INSERT INTO events (type, actor_id, subject_kind, subject_id, occurred_at)"+
				" VALUES ('test.violation', 999999, 'user', 1, '2026-01-01T00:00:00.000Z');", nil)
	})
	if err == nil {
		t.Fatal("an event referencing a user that does not exist was accepted; foreign keys are not being enforced")
	}
	if code := sqlite.ErrCode(err); code != sqlite.ResultConstraintForeignKey {
		t.Errorf("result code = %v, want SQLITE_CONSTRAINT_FOREIGNKEY", code)
	}
}

// TestConcurrentWriters is PLAN.md M1 acceptance 9: 20 goroutines each doing
// 50 writes, with no SQLITE_BUSY. There is one write connection and it is
// serialized, so contention is a queue we control rather than an error SQLite
// reports (DESIGN.md 13.3).
func TestConcurrentWriters(t *testing.T) {
	db := memoryStore(t)

	const (
		writers = 20
		each    = 50
	)
	c := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	now := c.Now().Format(timeFormat)

	var wg sync.WaitGroup
	errs := make(chan error, writers*each)
	for w := range writers {
		wg.Go(func() {
			for i := range each {
				err := db.Write(t.Context(), func(conn *sqlite.Conn) error {
					return sqlitex.Execute(conn,
						"INSERT INTO events (type, subject_kind, subject_id, occurred_at)"+
							" VALUES (:type, 'test', :subject_id, :at);",
						&sqlitex.ExecOptions{Named: map[string]any{
							":type":       "test.write",
							":subject_id": int64(w*each + i),
							":at":         now,
						}})
				})
				if err != nil {
					errs <- fmt.Errorf("writer %d row %d: %w (result code %v)", w, i, err, sqlite.ErrCode(err))
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
		if sqlite.ErrCode(err) == sqlite.ResultBusy {
			t.Error("SQLITE_BUSY: the single serialized writer is not doing its job")
		}
	}

	var count int64
	err := db.Read(t.Context(), func(conn *sqlite.Conn) error {
		return sqlitex.Execute(conn, "SELECT count(*) FROM events;", &sqlitex.ExecOptions{
			ResultFunc: func(stmt *sqlite.Stmt) error {
				count = stmt.ColumnInt64(0)
				return nil
			},
		})
	})
	if err != nil {
		t.Fatalf("counting: %v", err)
	}
	if want := int64(writers * each); count != want {
		t.Errorf("wrote %d rows, want %d", count, want)
	}
}

// TestReadConnectionsAreReadOnly is the structural half of "one writer": a
// write that reaches a reader by mistake fails rather than racing the writer.
//
// It runs against a file, because that is where the guarantee has to hold.
// SQLite does not enforce OpenReadOnly on a shared-cache in-memory database,
// so asserting it there would be asserting something about the test harness.
func TestReadConnectionsAreReadOnly(t *testing.T) {
	dir := t.TempDir()
	db, err := Create(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	err = db.Read(t.Context(), func(conn *sqlite.Conn) error {
		return sqlitex.Execute(conn,
			"INSERT INTO events (type, subject_kind, subject_id, occurred_at)"+
				" VALUES ('x', 'y', 1, '2026-01-01T00:00:00.000Z');", nil)
	})
	if err == nil {
		t.Error("a reader accepted an INSERT; readers are opened read-only so this cannot race the writer")
	}
}

// TestCloseIsIdempotent matters because a command's defer and a server's
// shutdown may both reach it.
func TestCloseIsIdempotent(t *testing.T) {
	db, err := OpenMemory(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestCheck is what "cmsdb check" reports on a healthy database.
func TestCheck(t *testing.T) {
	db := memoryStore(t)

	report, err := db.Check(t.Context())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !report.OK() {
		t.Errorf("a freshly migrated database is not OK: %+v", report)
	}
	if report.AppID != migrate.AppID {
		t.Errorf("AppID = %#x, want %#x", report.AppID, migrate.AppID)
	}
	if want := int32(migrate.Count()); report.SchemaVersion != want {
		t.Errorf("SchemaVersion = %d, want %d", report.SchemaVersion, want)
	}
	if report.Migrations != migrate.Count() {
		t.Errorf("Migrations = %d, want %d", report.Migrations, migrate.Count())
	}
}

// TestVacuum runs the real thing against a real file; an in-memory database
// has nothing to reclaim.
func TestVacuum(t *testing.T) {
	dir := t.TempDir()
	db, err := Create(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Vacuum(t.Context()); err != nil {
		t.Errorf("Vacuum: %v", err)
	}
}

// memoryStore is the in-memory store every store test that does not need a
// file uses: all migrations applied through the same runner cmsdb uses, and
// foreign keys on (invariant 22).
func memoryStore(t *testing.T) *DB {
	t.Helper()
	db, err := OpenMemory(t.Context())
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// independent opens a second connection to path, so that a test asserting on
// what is in the file is not asking the connection that wrote it.
func independent(t *testing.T, path string) *sqlite.Conn {
	t.Helper()
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadOnly)
	if err != nil {
		t.Fatalf("opening %q independently: %v", path, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func readPragma(t *testing.T, conn *sqlite.Conn, name string) int32 {
	t.Helper()
	v, err := pragmaInt32(conn, name)
	if err != nil {
		t.Fatalf("PRAGMA %s: %v", name, err)
	}
	return v
}

func readPragmaText(t *testing.T, conn *sqlite.Conn, name string) string {
	t.Helper()
	v, err := pragmaText(conn, name)
	if err != nil {
		t.Fatalf("PRAGMA %s: %v", name, err)
	}
	return v
}

// createAt builds a database in dir with only the first n migrations applied:
// the database an older binary would have left behind. store.Create always
// applies every migration, on purpose, so a test that needs a half-migrated
// file writes one directly.
func createAt(t *testing.T, dir string, n int) {
	t.Helper()
	conn, err := sqlite.OpenConn(Path(dir), createFlags)
	if err != nil {
		t.Fatalf("creating %q: %v", Path(dir), err)
	}
	defer conn.Close()
	if err := prepareConn(conn, DefaultBusyTimeout); err != nil {
		t.Fatalf("preparing %q: %v", Path(dir), err)
	}
	if err := migrate.Apply(t.Context(), conn, n); err != nil {
		t.Fatalf("applying %d migrations: %v", n, err)
	}
}

// setUserVersion rewinds or advances the schema version behind the store's
// back, which is how a test produces the database an operator has after a
// half-finished deploy.
func setUserVersion(t *testing.T, path string, v int32) {
	t.Helper()
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite|sqlite.OpenWAL)
	if err != nil {
		t.Fatalf("opening %q: %v", path, err)
	}
	defer conn.Close()
	if err := sqlitex.ExecuteTransient(conn, fmt.Sprintf("PRAGMA user_version = %d;", v), nil); err != nil {
		t.Fatalf("setting user_version: %v", err)
	}
}
