// Copyright (c) 2026 Michael D Henderson.

package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mdhender/bricolage/internal/migrate"
)

// These are #11 at the process level: a graceful shutdown left cms.db-wal on
// disk, so a backup of cms.db alone silently lost everything committed since
// the last automatic checkpoint.
//
// There is one subtest per shutdown route because invariant 17's "one path" is
// the claim being relied on — a fix that only covered SIGTERM would be a
// second path that nobody had noticed was there.

// TestAGracefulShutdownCheckpointsTheWriteAheadLog is #11 acceptance 1 and 4.
func TestAGracefulShutdownCheckpointsTheWriteAheadLog(t *testing.T) {
	if testing.Short() {
		t.Skip("builds three binaries")
	}
	bin := buildCommands(t, t.TempDir())

	for _, tc := range []struct {
		name    string
		timeout string
		// stop reaches the one shutdown path by this route. A nil stop is the
		// timeout expiring on its own.
		stop func(t *testing.T, p *process)
	}{
		{
			name:    "SIGTERM",
			timeout: "120s",
			stop: func(t *testing.T, p *process) {
				t.Helper()
				if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatalf("SIGTERM: %v", err)
				}
			},
		},
		{
			name:    "the timeout expiring",
			timeout: "8s",
			stop:    nil,
		},
		{
			name:    "the development shutdown route",
			timeout: "120s",
			stop: func(t *testing.T, p *process) {
				t.Helper()
				if body, status := get(t, p.url+"/__development/shut-it-down", ""); status != http.StatusOK {
					t.Fatalf("shutdown route = %d\nbody: %s", status, body)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const email, password = "admin@example.com", "correct horse battery staple"
			dir := bootstrapped(t, bin["cmsdb"], email, password)
			wal := filepath.Join(dir, "cms.db-wal")

			proc := start(t, bin["cmsd"], nil, "serve", "--db", dir,
				"--env", "development", "--addr", "127.0.0.1:0", "--timeout", tc.timeout)

			// Sessions, written by the server's own write connection, are
			// what puts pages in the log. An idle server writes nothing and
			// would pass this test without the fix.
			token := devLogins(t, proc, email, 25)
			if before := walSize(t, wal); before <= 0 {
				t.Fatalf("cms.db-wal is %d bytes while the server is running; the test is not reproducing the condition", before)
			}

			if tc.stop != nil {
				tc.stop(t, proc)
			}
			if code := proc.wait(t, 30*time.Second); code != 0 {
				t.Fatalf("exit code = %d, want 0\nstderr: %s", code, proc.stderr())
			}

			// Acceptance 1: the log is gone or empty.
			if got := walSize(t, wal); got > 0 {
				t.Errorf("cms.db-wal is %d bytes after a graceful shutdown, want absent or empty\nstderr: %s",
					got, proc.stderr())
			}

			// Acceptance 4: an operator watching a deploy is told, and told
			// what the checkpoint did. A file size is not something anybody
			// has a reason to look at.
			if !strings.Contains(proc.stderr(), "database closed") {
				t.Errorf("stderr has no line naming the closed database\nstderr: %s", proc.stderr())
			}
			if !strings.Contains(proc.stderr(), "truncated") {
				t.Errorf("stderr does not report the checkpoint result\nstderr: %s", proc.stderr())
			}

			// Acceptance 1, the part the rest of it is for: cms.db on its own
			// is a complete database. This is the backup deploy/README.md
			// asks an operator to take, and the session issued before the
			// shutdown has to still be in it.
			backup := backupOf(t, dir)
			checkPasses(t, bin["cmsdb"], backup)
			servesTheSession(t, bin["cmsd"], backup, token)
		})
	}
}

// TestAKilledServerLeavesARecoverableDatabase is #11 acceptance 3.
//
// Truncating on a clean close must not become something correctness depends
// on. A SIGKILL is the case where there is no close at all: the log stays, and
// the next opener replays it.
func TestAKilledServerLeavesARecoverableDatabase(t *testing.T) {
	if testing.Short() {
		t.Skip("builds three binaries")
	}
	bin := buildCommands(t, t.TempDir())

	const email, password = "admin@example.com", "correct horse battery staple"
	dir := bootstrapped(t, bin["cmsdb"], email, password)
	wal := filepath.Join(dir, "cms.db-wal")

	proc := start(t, bin["cmsd"], nil, "serve", "--db", dir,
		"--env", "development", "--addr", "127.0.0.1:0", "--timeout", "120s")
	token := devLogins(t, proc, email, 25)

	if err := proc.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL: %v", err)
	}
	if code := proc.wait(t, 30*time.Second); code == 0 {
		t.Fatalf("exit code = 0 after SIGKILL\nstderr: %s", proc.stderr())
	}

	// The log survives, which is the whole point: it is the only copy of
	// those commits, and nothing has had a chance to move them.
	if got := walSize(t, wal); got <= 0 {
		t.Fatalf("cms.db-wal is %d bytes after SIGKILL, want the log left for the next open to replay", got)
	}

	// And the next opener recovers it. This one is the real database rather
	// than a copy of cms.db alone, because after a kill the log is part of it.
	checkPasses(t, bin["cmsdb"], dir)
	servesTheSession(t, bin["cmsd"], dir, token)
}

// TestEveryCmsdbCommandCheckpointsOnItsWayOut is #11 acceptance 2. They all
// run the same store.Close, so this is one assertion repeated rather than six
// different ones — which is the point: the checkpoint is in the close, not in
// each command.
func TestEveryCmsdbCommandCheckpointsOnItsWayOut(t *testing.T) {
	if testing.Short() {
		t.Skip("builds three binaries")
	}
	bin := buildCommands(t, t.TempDir())
	cmsdb := bin["cmsdb"]

	dir := initDB(t, cmsdb)
	wal := filepath.Join(dir, "cms.db-wal")
	if got := walSize(t, wal); got > 0 {
		t.Errorf("cmsdb init left a %d-byte cms.db-wal", got)
	}

	// In the order the commands themselves require: seed writes the roles
	// bootstrap attaches to, and --demo has nobody to attribute sample
	// content to until an administrator exists.
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"seed", []string{"seed", "--db", dir}},
		{"bootstrap admin", []string{"bootstrap", "admin", "--db", dir,
			"--email", "admin@example.com", "--name", "Admin"}},
		{"seed --demo", []string{"seed", "--db", dir, "--demo"}},
		{"migrate status", []string{"migrate", "status", "--db", dir}},
		{"check", []string{"check", "--db", dir}},
		{"vacuum", []string{"vacuum", "--db", dir}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if stdout, stderr, code := run(t, cmsdb, nil, tc.args...); code != 0 {
				t.Fatalf("cmsdb %s exited %d\nstdout: %s\nstderr: %s", tc.name, code, stdout, stderr)
			}
			if got := walSize(t, wal); got > 0 {
				t.Errorf("cmsdb %s left a %d-byte cms.db-wal, want absent or empty", tc.name, got)
			}
		})
	}
}

// devLogins issues n sessions through the development login route and returns
// the last token. Each one is a committed write on the server's own
// connection, which is what a log with pages in it needs.
func devLogins(t *testing.T, p *process, email string, n int) string {
	t.Helper()
	var token string
	for range n {
		body, status := get(t, p.url+"/__development/log-me-in/"+email, "application/json")
		if status != http.StatusOK {
			t.Fatalf("development login = %d\nbody: %s", status, body)
		}
		var login struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal([]byte(body), &login); err != nil {
			t.Fatalf("the login body is not JSON: %v\n%s", err, body)
		}
		if login.Token == "" {
			t.Fatalf("the login body carries no token: %s", body)
		}
		token = login.Token
	}
	return token
}

// backupOf copies cms.db and nothing else into a directory of its own, which
// is the backup deploy/README.md asks for.
func backupOf(t *testing.T, dir string) string {
	t.Helper()
	backup := t.TempDir()
	b, err := os.ReadFile(filepath.Join(dir, "cms.db"))
	if err != nil {
		t.Fatalf("reading cms.db: %v", err)
	}
	if err := os.WriteFile(filepath.Join(backup, "cms.db"), b, 0o600); err != nil {
		t.Fatalf("writing the copy: %v", err)
	}
	return backup
}

// checkPasses runs "cmsdb check" and asserts it finds a database at the
// version this binary embeds with nothing wrong with it.
func checkPasses(t *testing.T, cmsdb, dir string) {
	t.Helper()
	stdout, stderr, code := run(t, cmsdb, nil, "check", "--db", dir)
	if code != 0 {
		t.Fatalf("cmsdb check --db %s exited %d\nstdout: %s\nstderr: %s", dir, code, stdout, stderr)
	}
	if want := "user_version: " + strconv.Itoa(migrate.Count()); !strings.Contains(stdout, want) {
		t.Errorf("cmsdb check printed no %q\nstdout: %s", want, stdout)
	}
	if strings.Contains(stdout, "integrity:") {
		t.Errorf("integrity_check reported a problem\nstdout: %s", stdout)
	}
}

// servesTheSession starts a cmsd against dir and asserts that the session
// issued before the shutdown is still there. It is the strongest form of
// "the file is complete": a row committed to the log and never moved into
// cms.db would be a 401 here.
func servesTheSession(t *testing.T, cmsd, dir, token string) {
	t.Helper()
	proc := start(t, cmsd, nil, "serve", "--db", dir,
		"--env", "development", "--addr", "127.0.0.1:0", "--timeout", "30s")
	body, status := getWithToken(t, proc.url+"/api/v1/me", token)
	if status != http.StatusOK {
		t.Errorf("GET /api/v1/me on the recovered database = %d, want 200; the session committed before the shutdown is missing\nbody: %s",
			status, body)
	}
}

// walSize is the size of path in bytes, or -1 if it is not there. A log that
// was truncated may be absent or empty depending on the platform, and both are
// the answer this system wants.
func walSize(t *testing.T, path string) int64 {
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
