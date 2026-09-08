// Copyright (c) 2026 Michael D Henderson.

package migrate

import (
	"fmt"
	"strings"
	"testing"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitemigration"
	"zombiezen.com/go/sqlite/sqlitex"
)

// TestAppID pins the constant. It is written down in DESIGN.md 13.2 three
// ways, and all three have to agree: 0x434D5330, 1129141040, and the ASCII
// bytes of "CMS0".
func TestAppID(t *testing.T) {
	if AppID != 0x434D5330 {
		t.Errorf("AppID = %#x, want 0x434D5330", AppID)
	}
	if AppID != 1129141040 {
		t.Errorf("AppID = %d, want 1129141040", AppID)
	}
	want := "CMS0"
	id := uint32(AppID)
	got := string([]byte{byte(id >> 24), byte(id >> 16), byte(id >> 8), byte(id)})
	if got != want {
		t.Errorf("AppID spells %q, want %q", got, want)
	}
	if s := AppIDString(AppID); !strings.Contains(s, "CMS0") || !strings.Contains(s, "0x434d5330") {
		t.Errorf("AppIDString = %q, want the hex and the four characters", s)
	}
}

// TestEmbeddedMigrations is the shape check the loader performs at init, run
// here so that a bad file name fails the test rather than the binary.
func TestEmbeddedMigrations(t *testing.T) {
	all := All()
	if len(all) == 0 {
		t.Fatal("no migrations are embedded")
	}
	if Count() != len(all) {
		t.Errorf("Count() = %d, len(All()) = %d", Count(), len(all))
	}
	for i, m := range all {
		if want := int32(i + 1); m.Version() != want {
			t.Errorf("%s is version %d, want %d", m.Name, m.Version(), want)
		}
		if strings.TrimSpace(m.SQL) == "" {
			t.Errorf("%s is empty", m.Name)
		}
	}
}

// TestSchemaPrefix is what makes "migrate up --to N" a prefix of the same list
// rather than a second code path that could disagree about what migration 3
// is.
func TestSchemaPrefix(t *testing.T) {
	for n := range Count() + 1 {
		s, err := Schema(n)
		if err != nil {
			t.Fatalf("Schema(%d): %v", n, err)
		}
		if len(s.Migrations) != n {
			t.Errorf("Schema(%d) has %d migrations", n, len(s.Migrations))
		}
		if s.AppID != AppID {
			t.Errorf("Schema(%d).AppID = %#x, want %#x", n, s.AppID, AppID)
		}
	}
	if s, err := Schema(-1); err != nil || len(s.Migrations) != Count() {
		t.Errorf("Schema(-1) = %d migrations, %v; want all %d and no error", len(s.Migrations), err, Count())
	}
	if _, err := Schema(Count() + 1); err == nil {
		t.Errorf("Schema(%d) succeeded; this binary embeds %d", Count()+1, Count())
	}
}

// TestApplyStampsBothMarkers is PLAN.md M1 acceptance 4, at the level of the
// runner: a fully migrated database carries the application ID and a
// user_version equal to the number of embedded migrations. There is no
// schema_migrations table and there is not to be one — the pragmas are the
// bookkeeping (DESIGN.md 13.2).
func TestApplyStampsBothMarkers(t *testing.T) {
	conn := memConn(t)
	if err := Apply(t.Context(), conn, -1); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if got := pragma(t, conn, "application_id"); got != AppID {
		t.Errorf("application_id = %#x, want %#x", got, AppID)
	}
	if got, want := pragma(t, conn, "user_version"), int32(Count()); got != want {
		t.Errorf("user_version = %d, want %d", got, want)
	}

	var found string
	err := sqlitex.ExecuteTransient(conn,
		"SELECT name FROM sqlite_schema WHERE type = 'table' AND name LIKE '%migration%';",
		&sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
			found = stmt.ColumnText(0)
			return nil
		}})
	if err != nil {
		t.Fatalf("querying sqlite_schema: %v", err)
	}
	if found != "" {
		t.Errorf("found table %q; the pragmas are the bookkeeping and a second record can disagree with the database", found)
	}
}

// TestConvergence is PLAN.md M1 acceptance 6: applying migrations to an empty
// database and to a partially-migrated one converge to the same schema.
//
// It runs against the embedded schema at every intermediate version, so it
// grows a case with every migration added rather than needing to be revisited.
func TestConvergence(t *testing.T) {
	fresh := memConn(t)
	if err := Apply(t.Context(), fresh, -1); err != nil {
		t.Fatalf("migrating a fresh database: %v", err)
	}
	want := dumpSchema(t, fresh)

	for stop := range Count() {
		t.Run(fmt.Sprintf("stopped_at_%d", stop), func(t *testing.T) {
			conn := memConn(t)
			if err := Apply(t.Context(), conn, stop); err != nil {
				t.Fatalf("migrating to %d: %v", stop, err)
			}
			if got, want := pragma(t, conn, "user_version"), int32(stop); got != want {
				t.Fatalf("after --to %d, user_version = %d", stop, got)
			}
			if err := Apply(t.Context(), conn, -1); err != nil {
				t.Fatalf("migrating the rest of the way: %v", err)
			}
			if got := dumpSchema(t, conn); got != want {
				t.Errorf("a partially-migrated database converged to a different schema:\n%s\nwant:\n%s", got, want)
			}
		})
	}
}

// TestConvergenceOfTheRunner covers the same criterion for the runner rather
// than for today's two migrations, so the guarantee does not depend on how
// many files happen to be embedded.
func TestConvergenceOfTheRunner(t *testing.T) {
	schema := sqlitemigration.Schema{
		AppID: AppID,
		Migrations: []string{
			"CREATE TABLE a (id INTEGER PRIMARY KEY, v TEXT) STRICT",
			"CREATE TABLE b (id INTEGER PRIMARY KEY, a_id INTEGER NOT NULL REFERENCES a(id)) STRICT",
			"CREATE INDEX b_a ON b (a_id)",
			"ALTER TABLE a ADD COLUMN note TEXT",
		},
	}

	all := memConn(t)
	if err := sqlitemigration.Migrate(t.Context(), all, schema); err != nil {
		t.Fatalf("migrating all: %v", err)
	}
	want := dumpSchema(t, all)

	for stop := range len(schema.Migrations) {
		partial := schema
		partial.Migrations = schema.Migrations[:stop]
		conn := memConn(t)
		if err := sqlitemigration.Migrate(t.Context(), conn, partial); err != nil {
			t.Fatalf("migrating to %d: %v", stop, err)
		}
		if err := sqlitemigration.Migrate(t.Context(), conn, schema); err != nil {
			t.Fatalf("migrating the rest from %d: %v", stop, err)
		}
		if got := dumpSchema(t, conn); got != want {
			t.Errorf("stopping at %d converged to a different schema:\n%s\nwant:\n%s", stop, got, want)
		}
	}
}

// TestPending is what "cmsdb migrate status" reports.
func TestPending(t *testing.T) {
	for v := range int32(Count() + 1) {
		applied, pending := Pending(v)
		if len(applied) != int(v) {
			t.Errorf("at user_version %d, %d applied; want %d", v, len(applied), v)
		}
		if want := Count() - int(v); len(pending) != want {
			t.Errorf("at user_version %d, %d pending; want %d", v, len(pending), want)
		}
	}
	if s := Describe(0); !strings.Contains(s, "pending") || strings.Contains(s, "applied") {
		t.Errorf("Describe(0) = %q, want every migration pending", s)
	}
	if s := Describe(int32(Count())); strings.Contains(s, "pending") {
		t.Errorf("Describe(%d) = %q, want nothing pending", Count(), s)
	}
}

// memConn opens a private in-memory database with foreign keys on, which is
// how every connection in this system is configured (invariant 22).
func memConn(t *testing.T) *sqlite.Conn {
	t.Helper()
	conn, err := sqlite.OpenConn(":memory:", sqlite.OpenReadWrite|sqlite.OpenCreate)
	if err != nil {
		t.Fatalf("opening an in-memory database: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := sqlitex.ExecuteTransient(conn, "PRAGMA foreign_keys = ON;", nil); err != nil {
		t.Fatalf("PRAGMA foreign_keys: %v", err)
	}
	return conn
}

func pragma(t *testing.T, conn *sqlite.Conn, name string) int32 {
	t.Helper()
	var v int32
	err := sqlitex.ExecuteTransient(conn, "PRAGMA "+name+";", &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			v = stmt.ColumnInt32(0)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("PRAGMA %s: %v", name, err)
	}
	return v
}

// dumpSchema renders sqlite_schema in a stable order, which is what "converge
// to the same schema" is measured against.
func dumpSchema(t *testing.T, conn *sqlite.Conn) string {
	t.Helper()
	var b strings.Builder
	err := sqlitex.ExecuteTransient(conn,
		"SELECT type, name, tbl_name, COALESCE(sql, '') FROM sqlite_schema"+
			" WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name;",
		&sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
			fmt.Fprintf(&b, "%s %s %s\n%s\n", stmt.ColumnText(0), stmt.ColumnText(1),
				stmt.ColumnText(2), stmt.ColumnText(3))
			return nil
		}})
	if err != nil {
		t.Fatalf("dumping sqlite_schema: %v", err)
	}
	return b.String()
}
