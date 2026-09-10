// Copyright (c) 2026 Michael D Henderson.

package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/mdhender/bricolage/internal/migrate"
)

// TestBackupWhileTheServerIsRunning is #10 acceptance 1, at the level the
// criterion is written at: the point of the command is that an operator can
// take a backup without an outage, and the previous answer — a cp of the
// three files — is only sound with the service stopped.
func TestBackupWhileTheServerIsRunning(t *testing.T) {
	if testing.Short() {
		t.Skip("builds three binaries")
	}
	bin := buildCommands(t, t.TempDir())

	const email, password = "admin@example.com", "correct horse battery staple"
	dir := bootstrapped(t, bin["cmsdb"], email, password)

	proc := start(t, bin["cmsd"], nil, "serve", "--db", dir,
		"--env", "development", "--addr", "127.0.0.1:0", "--timeout", "120s")

	// Sessions, committed by the running server and still in its write-ahead
	// log. A snapshot that read around the log would come back without them.
	token := devLogins(t, proc, email, 25)
	if n := walSize(t, filepath.Join(dir, "cms.db-wal")); n <= 0 {
		t.Fatalf("cms.db-wal is %d bytes; the backup would not be proving anything", n)
	}

	backups := t.TempDir()
	to := filepath.Join(backups, "cms-2026-09-09.db")
	stdout, stderr, code := run(t, bin["cmsdb"], nil, "backup", "--db", dir, "--to", to)
	if code != 0 {
		t.Fatalf("cmsdb backup exited %d while cmsd was running\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	// The line in a deploy log is evidence rather than reassurance: the path,
	// the size, and the version.
	for _, want := range []string{
		"backup: " + to,
		"user_version: " + strconv.Itoa(migrate.Count()),
		"verified",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("cmsdb backup printed no %q\nstdout: %s", want, stdout)
		}
	}
	info, err := os.Stat(to)
	if err != nil {
		t.Fatalf("the backup was not written: %v", err)
	}
	if !strings.Contains(stdout, "size: "+strconv.FormatInt(info.Size(), 10)) {
		t.Errorf("cmsdb backup reported a size that is not the file's %d\nstdout: %s", info.Size(), stdout)
	}
	// Nothing beside it: the file appears under the name asked for only once
	// it is written in full and checked.
	if entries, err := os.ReadDir(backups); err != nil {
		t.Fatal(err)
	} else if len(entries) != 1 {
		t.Errorf("the backup directory holds %d entries, want just the backup", len(entries))
	}

	// The server is still up and still serving; the backup took no outage.
	if body, status := getWithToken(t, proc.url+"/api/v1/me", token); status != http.StatusOK {
		t.Errorf("GET /api/v1/me after the backup = %d\nbody: %s", status, body)
	}

	// It verifies under its own name, which is the thing that used to need a
	// temporary directory and a rename.
	if out, stderr, code := run(t, bin["cmsdb"], nil, "check", "--file", to); code != 0 {
		t.Errorf("cmsdb check --file exited %d\nstdout: %s\nstderr: %s", code, out, stderr)
	}

	// And it restores. Restore is a cp with the service stopped and
	// deliberately not a command, so what has to be true is that the file is
	// one cmsd can be pointed at — VACUUM INTO leaves it in rollback-journal
	// mode and cmsd requires WAL.
	restored := t.TempDir()
	b, err := os.ReadFile(to)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(restored, "cms.db"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	second := start(t, bin["cmsd"], nil, "serve", "--db", restored,
		"--env", "development", "--addr", "127.0.0.1:0", "--timeout", "30s")
	if body, status := getWithToken(t, second.url+"/api/v1/me", token); status != http.StatusOK {
		t.Errorf("GET /api/v1/me on the restored backup = %d, want 200; the session the running server committed is missing\nbody: %s",
			status, body)
	}
}

// TestBackupRefusals is #10 acceptance 2, 3, and 4 through the command, since
// all three are about what an operator typed.
func TestBackupRefusals(t *testing.T) {
	if testing.Short() {
		t.Skip("builds three binaries")
	}
	bin := buildCommands(t, t.TempDir())
	cmsdb := bin["cmsdb"]
	dir := initDB(t, cmsdb)
	backups := t.TempDir()

	t.Run("a missing directory names it and creates nothing", func(t *testing.T) {
		missing := filepath.Join(backups, "nightly")
		_, stderr, code := run(t, cmsdb, nil, "backup", "--db", dir, "--to", filepath.Join(missing, "cms.db"))
		if code == 0 {
			t.Fatal("exited 0")
		}
		if !strings.Contains(stderr, missing) {
			t.Errorf("stderr does not name the directory\nstderr: %s", stderr)
		}
		if _, err := os.Stat(missing); !os.IsNotExist(err) {
			t.Errorf("%q was created (invariant 19)", missing)
		}
	})

	t.Run("an existing file is refused and the flag is named", func(t *testing.T) {
		to := filepath.Join(backups, "twice.db")
		if _, stderr, code := run(t, cmsdb, nil, "backup", "--db", dir, "--to", to); code != 0 {
			t.Fatalf("first backup: %s", stderr)
		}
		before, err := os.ReadFile(to)
		if err != nil {
			t.Fatal(err)
		}

		_, stderr, code := run(t, cmsdb, nil, "backup", "--db", dir, "--to", to)
		if code == 0 {
			t.Fatal("the second backup exited 0 and replaced the first")
		}
		if !strings.Contains(stderr, "--overwrite") {
			t.Errorf("the refusal does not name the flag that permits it\nstderr: %s", stderr)
		}
		if after, err := os.ReadFile(to); err != nil {
			t.Fatal(err)
		} else if len(after) != len(before) {
			t.Errorf("the refused backup changed the file from %d to %d bytes", len(before), len(after))
		}

		if _, stderr, code := run(t, cmsdb, nil, "backup", "--db", dir, "--to", to, "--overwrite"); code != 0 {
			t.Errorf("--overwrite was refused: %s", stderr)
		}
	})

	t.Run("a --db that is not ours is refused the usual way", func(t *testing.T) {
		foreign := t.TempDir()
		stampForeign(t, filepath.Join(foreign, "cms.db"), 0x41424344)
		_, stderr, code := run(t, cmsdb, nil,
			"backup", "--db", foreign, "--to", filepath.Join(backups, "foreign.db"))
		if code == 0 {
			t.Fatal("exited 0")
		}
		if !strings.Contains(stderr, "application_id") {
			t.Errorf("stderr = %q, want the application_id refusal every other cmsdb command gives", stderr)
		}
		if _, err := os.Stat(filepath.Join(backups, "foreign.db")); !os.IsNotExist(err) {
			t.Error("a backup was written from a database that was refused")
		}
	})

	t.Run("check wants exactly one of --db and --file", func(t *testing.T) {
		_, stderr, code := run(t, cmsdb, nil, "check")
		if code == 0 || !strings.Contains(stderr, "--file") {
			t.Errorf("cmsdb check with no target exited %d\nstderr: %s", code, stderr)
		}
		_, stderr, code = run(t, cmsdb, nil, "check", "--db", dir, "--file", filepath.Join(backups, "twice.db"))
		if code == 0 {
			t.Errorf("cmsdb check accepted both --db and --file\nstderr: %s", stderr)
		}
	})
}

// TestBackupAcrossAPendingMigration is issue #25 as it was actually met: the
// deploy of 0.17.0-beta to the rehearsal droplet, where the binaries were
// already uploaded and the backup step refused with the service stopped.
//
// The deploy order moved so that the question does not come up (#26), and this
// is the other half of the answer: it should not have been a question. A
// backup is a copy of a file. What it has to agree with the file about is the
// application ID.
func TestBackupAcrossAPendingMigration(t *testing.T) {
	if testing.Short() {
		t.Skip("builds three binaries")
	}
	bin := buildCommands(t, t.TempDir())
	cmsdb := bin["cmsdb"]
	dir := initDB(t, cmsdb)
	db := filepath.Join(dir, "cms.db")
	backups := t.TempDir()

	current := versionOf(t, db)
	if current < 2 {
		t.Skip("needs at least two migrations to have a version to be behind")
	}
	behind := current - 1

	// The database an operator has mid-deploy: new binaries in place, schema
	// still at the version the old ones left.
	setVersion(t, db, behind)

	t.Run("the backup is taken and reports the schema in the file", func(t *testing.T) {
		to := filepath.Join(backups, "pre-migration.db")
		stdout, stderr, code := run(t, cmsdb, nil, "backup", "--db", dir, "--to", to)
		if code != 0 {
			t.Fatalf("backup across a pending migration exited %d\nstderr: %s", code, stderr)
		}
		if !strings.Contains(stdout, "verified") {
			t.Errorf("the backup was not verified\nstdout: %s", stdout)
		}
		want := fmt.Sprintf("user_version: %d", behind)
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout does not report %q, which is what somebody restoring it needs\nstdout: %s",
				want, stdout)
		}
		if info, err := os.Stat(to); err != nil {
			t.Fatalf("the backup file: %v", err)
		} else if info.Size() == 0 {
			t.Error("the backup is zero bytes")
		}
	})

	t.Run("and the backup can be read back", func(t *testing.T) {
		to := filepath.Join(backups, "readable.db")
		if _, stderr, code := run(t, cmsdb, nil, "backup", "--db", dir, "--to", to); code != 0 {
			t.Fatalf("backup: %s", stderr)
		}
		stdout, stderr, code := run(t, cmsdb, nil, "check", "--file", to)
		if code != 0 {
			t.Fatalf("check --file on a backup taken at an older schema exited %d\nstderr: %s", code, stderr)
		}
		if !strings.Contains(stdout, "ok") {
			t.Errorf("the backup does not check out\nstdout: %s", stdout)
		}
	})

	t.Run("serving still refuses the same database", func(t *testing.T) {
		// The policy is per-operation, not global. cmsd interprets rows, so
		// invariant 21 is untouched by any of this.
		_, stderr, code := run(t, bin["cmsd"], nil, "serve", "--db", dir,
			"--addr", "127.0.0.1:0", "--timeout", "5s")
		if code == 0 {
			t.Fatal("cmsd started against a database at the wrong version")
		}
		if !strings.Contains(stderr, "user_version") {
			t.Errorf("the refusal does not name the version\nstderr: %s", stderr)
		}
	})
}
