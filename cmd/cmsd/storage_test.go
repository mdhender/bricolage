// Copyright (c) 2026 Michael D Henderson.

package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mdhender/bricolage/internal/migrate"
	"github.com/mdhender/bricolage/internal/store"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// These are the PLAN.md M1 acceptance criteria that are about the processes
// rather than about a function: what cmsdb prints, and the four ways cmsd must
// refuse to start. They live in cmd/cmsd because that is where the harness
// that builds and runs the binaries already is.

// TestCmsdb covers PLAN.md M1 acceptance 1, 2, and 5.
func TestCmsdb(t *testing.T) {
	if testing.Short() {
		t.Skip("builds three binaries")
	}
	bin := buildCommands(t, t.TempDir())
	cmsdb := bin["cmsdb"]

	// Acceptance 1: init creates DIR/cms.db, and running it twice is safe and
	// says "already initialised".
	t.Run("init", func(t *testing.T) {
		dir := t.TempDir()

		stdout, stderr, code := run(t, cmsdb, nil, "init", "--db", dir)
		if code != 0 {
			t.Fatalf("init exited %d\nstderr: %s", code, stderr)
		}
		if _, err := os.Stat(filepath.Join(dir, "cms.db")); err != nil {
			t.Fatalf("cms.db was not created: %v", err)
		}
		if !strings.Contains(stdout, "initialised") {
			t.Errorf("init printed %q", stdout)
		}

		stdout, stderr, code = run(t, cmsdb, nil, "init", "--db", dir)
		if code != 0 {
			t.Fatalf("the second init exited %d\nstderr: %s", code, stderr)
		}
		if !strings.Contains(stdout, "already initialised") {
			t.Errorf("the second init printed %q, want \"already initialised\"", stdout)
		}
	})

	// Acceptance 2, for every subcommand: a missing directory is a hard
	// failure that names the directory and creates nothing (invariant 19).
	t.Run("a missing directory creates nothing", func(t *testing.T) {
		for _, args := range [][]string{
			{"init"},
			{"migrate", "status"},
			{"migrate", "up"},
			{"check"},
			{"vacuum"},
		} {
			name := strings.Join(args, " ")
			missing := filepath.Join(t.TempDir(), "does-not-exist")

			_, stderr, code := run(t, cmsdb, nil, append(args, "--db", missing)...)
			if code == 0 {
				t.Errorf("cmsdb %s --db %s exited 0", name, missing)
			}
			if !strings.Contains(stderr, missing) {
				t.Errorf("cmsdb %s did not name the directory: %s", name, stderr)
			}
			if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("cmsdb %s created %s; nothing in this system creates a directory", name, missing)
			}
		}
	})

	// Acceptance 5: migrate status lists applied and pending migrations and
	// prints the application ID and the version it read.
	t.Run("migrate status", func(t *testing.T) {
		dir := t.TempDir()
		if _, stderr, code := run(t, cmsdb, nil, "init", "--db", dir); code != 0 {
			t.Fatalf("init: %s", stderr)
		}

		stdout, stderr, code := run(t, cmsdb, nil, "migrate", "status", "--db", dir)
		if code != 0 {
			t.Fatalf("migrate status exited %d\nstderr: %s", code, stderr)
		}
		for _, want := range []string{
			"application_id",
			migrate.AppIDString(migrate.AppID),
			fmt.Sprintf("user_version: %d", migrate.Count()),
		} {
			if !strings.Contains(stdout, want) {
				t.Errorf("migrate status printed %q, want it to contain %q", stdout, want)
			}
		}
		for _, m := range migrate.All() {
			if !strings.Contains(stdout, "applied  "+m.Name) {
				t.Errorf("migrate status did not list %s as applied:\n%s", m.Name, stdout)
			}
		}
		if strings.Contains(stdout, "pending") {
			t.Errorf("migrate status listed something pending on a fresh database:\n%s", stdout)
		}
	})

	t.Run("check and vacuum", func(t *testing.T) {
		dir := t.TempDir()
		if _, stderr, code := run(t, cmsdb, nil, "init", "--db", dir); code != 0 {
			t.Fatalf("init: %s", stderr)
		}

		stdout, stderr, code := run(t, cmsdb, nil, "check", "--db", dir)
		if code != 0 {
			t.Fatalf("check exited %d\nstderr: %s", code, stderr)
		}
		for _, want := range []string{
			"application_id",
			"user_version",
			"stuck job leases: 0",
			// Without --output the output tree is not walked, and the check
			// says the question was not asked rather than answering it with a
			// zero it did not earn (PLAN.md M9 acceptance 7).
			"orphaned resources: not checked",
			"ok",
		} {
			if !strings.Contains(stdout, want) {
				t.Errorf("check printed %q, want it to contain %q", stdout, want)
			}
		}

		if _, stderr, code := run(t, cmsdb, nil, "vacuum", "--db", dir); code != 0 {
			t.Fatalf("vacuum exited %d\nstderr: %s", code, stderr)
		}
	})
}

// TestCmsdRefusesToStart is PLAN.md M1 acceptance 10, 11, 12, and 13.
//
// Every case asserts three things: a non-zero exit, nothing listening, and a
// message naming the expected value, the actual value, and the path. The last
// is not decoration — these are the messages an operator reads at three in the
// morning.
func TestCmsdRefusesToStart(t *testing.T) {
	if testing.Short() {
		t.Skip("builds three binaries")
	}
	bin := buildCommands(t, t.TempDir())

	// A port nobody is listening on. cmsd is told to bind it and must not, so
	// the test can assert on silence rather than on the absence of a log line.
	addr := reservedAddr(t)

	for _, tc := range []struct {
		name string

		// setup prepares the --db directory and returns it.
		setup func(t *testing.T) string

		// want are substrings the message must contain.
		want []string
	}{
		{
			// Acceptance 10.
			name:  "no cms.db in the directory",
			setup: func(t *testing.T) string { return t.TempDir() },
			want:  []string{"cms.db", "does not exist", "cmsdb init"},
		},
		{
			// Acceptance 2, for cmsd.
			name: "the directory does not exist",
			setup: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "does-not-exist")
			},
			want: []string{"does-not-exist", "creates a directory"},
		},
		{
			// Acceptance 11, first variant: the empty file, whose
			// application_id reads as 0 — the value sqlitemigration adopts.
			name: "a zero-length file",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				if err := os.WriteFile(filepath.Join(dir, "cms.db"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
				return dir
			},
			want: []string{"application_id", "0x00000000", "0x434d5330"},
		},
		{
			// Acceptance 11, second variant: a real SQLite database that
			// belongs to something else.
			name: "another program's database",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				stampForeign(t, filepath.Join(dir, "cms.db"), 0x4f544852)
				return dir
			},
			want: []string{"application_id", "0x4f544852", "0x434d5330"},
		},
		{
			// Acceptance 12, behind.
			name: "one migration behind",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				initTo(t, bin["cmsdb"], dir, migrate.Count()-1)
				return dir
			},
			want: []string{
				"cms.db",
				fmt.Sprintf("user_version is %d", migrate.Count()-1),
				fmt.Sprintf("expected %d", migrate.Count()),
				"cmsdb migrate up",
			},
		},
		{
			// Acceptance 12, ahead.
			name: "one migration ahead",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				if _, stderr, code := run(t, bin["cmsdb"], nil, "init", "--db", dir); code != 0 {
					t.Fatalf("init: %s", stderr)
				}
				setVersion(t, filepath.Join(dir, "cms.db"), int32(migrate.Count()+1))
				return dir
			},
			want: []string{
				"cms.db",
				fmt.Sprintf("user_version is %d", migrate.Count()+1),
				fmt.Sprintf("expected %d", migrate.Count()),
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := tc.setup(t)
			before := versionOf(t, filepath.Join(dir, "cms.db"))

			_, stderr, code := run(t, bin["cmsd"], nil,
				"serve", "--db", dir, "--addr", addr, "--timeout", "5s")

			if code == 0 {
				t.Fatalf("cmsd serve exited 0\nstderr: %s", stderr)
			}
			// Acceptance 13.
			for _, want := range tc.want {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr does not contain %q:\n%s", want, stderr)
				}
			}
			// Acceptance 10 through 12: it bound nothing.
			if conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond); err == nil {
				_ = conn.Close()
				t.Errorf("something is listening on %s; a refused start binds nothing", addr)
			}
			// Acceptance 12: the version afterwards is untouched. A server
			// that migrated and then failed for another reason must not pass.
			if after := versionOf(t, filepath.Join(dir, "cms.db")); after != before {
				t.Errorf("user_version went from %d to %d; cmsd never migrates (invariant 20)", before, after)
			}
		})
	}
}

// TestCmsdHasNoWayToSayYes is invariant 20 spelled as a flag check. cmsd does
// not gain a --migrate flag, a --create flag, or any other way to repair what
// it found.
func TestCmsdHasNoWayToSayYes(t *testing.T) {
	if testing.Short() {
		t.Skip("builds three binaries")
	}
	bin := buildCommands(t, t.TempDir())

	stdout, stderr, _ := run(t, bin["cmsd"], nil, "serve", "--help")
	help := stdout + stderr
	for _, forbidden := range []string{"--migrate", "--create", "--init", "--auto-migrate"} {
		if strings.Contains(help, forbidden) {
			t.Errorf("cmsd serve accepts %s; only cmsdb creates and migrates (invariant 20)", forbidden)
		}
	}
}

// reservedAddr returns a loopback address that nothing is listening on. The
// listener is opened to learn a free port and closed again, which is enough:
// the assertion is that cmsd does not bind it, and a stray process taking it
// in between would fail this test loudly rather than silently.
func reservedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("releasing the port: %v", err)
	}
	return addr
}

// initTo builds a database with only the first n migrations applied: what an
// older binary would have left behind.
//
// "cmsdb init" always applies everything, on purpose, so this stamps an empty
// database and walks it forward with "migrate up --to n" — which is also the
// only exercise "--to" gets at the process level.
func initTo(t *testing.T, cmsdb, dir string, n int) {
	t.Helper()
	stampEmpty(t, filepath.Join(dir, "cms.db"))
	if _, stderr, code := run(t, cmsdb, nil, "migrate", "up", "--db", dir, "--to", fmt.Sprint(n)); code != 0 {
		t.Fatalf("migrate up --to %d: %s", n, stderr)
	}
	if got := versionOf(t, filepath.Join(dir, "cms.db")); got != int32(n) {
		t.Fatalf("migrate up --to %d left user_version at %d", n, got)
	}
}

// stampEmpty writes a database with the application ID and no schema, which is
// the state "cmsdb migrate up" is there to move forward from.
func stampEmpty(t *testing.T, path string) {
	t.Helper()
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite|sqlite.OpenCreate|sqlite.OpenWAL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := sqlitex.ExecuteTransient(conn,
		fmt.Sprintf("PRAGMA application_id = %d;", migrate.AppID), nil); err != nil {
		t.Fatal(err)
	}
}

// stampForeign writes a valid SQLite database belonging to some other program.
//
// It writes no schema, and none is needed: the check cmsd performs is on the
// application ID, and a non-zero ID is not the value sqlitemigration would
// adopt for an empty file. The schema-bearing variant is covered as a unit
// test in internal/store, where SQL belongs (invariant 2).
func stampForeign(t *testing.T, path string, appID int32) {
	t.Helper()
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite|sqlite.OpenCreate|sqlite.OpenWAL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := sqlitex.ExecuteTransient(conn, fmt.Sprintf("PRAGMA application_id = %d;", appID), nil); err != nil {
		t.Fatal(err)
	}
}

func setVersion(t *testing.T, path string, v int32) {
	t.Helper()
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite|sqlite.OpenWAL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := sqlitex.ExecuteTransient(conn, fmt.Sprintf("PRAGMA user_version = %d;", v), nil); err != nil {
		t.Fatal(err)
	}
}

// versionOf reads PRAGMA user_version, or -1 if there is no database to read.
func versionOf(t *testing.T, path string) int32 {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		return -1
	}
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadOnly)
	if err != nil {
		return -1
	}
	defer conn.Close()
	var v int32
	err = sqlitex.ExecuteTransient(conn, "PRAGMA user_version;", &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			v = stmt.ColumnInt32(0)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("reading user_version from %q: %v", path, err)
	}
	return v
}

// TestPathIsTheDirectoryPlusTheConstant is DESIGN.md 13.1 as an assertion:
// --db names a directory and the file inside it is always cms.db, so --db
// cannot address two different files depending on which command was typed.
func TestPathIsTheDirectoryPlusTheConstant(t *testing.T) {
	dir := t.TempDir()
	if got, want := store.Path(dir), filepath.Join(dir, "cms.db"); got != want {
		t.Errorf("store.Path(%q) = %q, want %q", dir, got, want)
	}
}
