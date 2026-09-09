// Copyright (c) 2026 Michael D Henderson.

package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// M6's end-to-end test: earl driven against a cmsd on a temporary database
// (AGENTS.md, "Testing").
//
// The job it watches comes from "cmsdb seed --demo", because nothing else can
// put one there: work is scheduled by the operation that needs it, and until
// M9 no operation needs any. That is deliberate rather than a gap -- an API
// route taking a kind and a payload would be a way to run any handler in the
// server with arguments the client chose -- so --demo seeds one noop job and
// this drives the whole loop over it.

// earlJob is the part of a job response these assertions read.
type earlJob struct {
	UID         string `json:"uid"`
	Kind        string `json:"kind"`
	Status      string `json:"status"`
	Priority    int    `json:"priority"`
	Attempts    int    `json:"attempts"`
	MaxAttempts int    `json:"max_attempts"`
	Worker      string `json:"worker"`
	LastError   string `json:"last_error"`
}

type earlJobList struct {
	Jobs  []earlJob `json:"jobs"`
	Count int       `json:"count"`
	Asks  string    `json:"asks"`
}

// TestEarlJobs is PLAN.md M6 through the commands: the queue is visible, the
// workers are hosted in cmsd behind --workers, and the retry route refuses
// what it should.
func TestEarlJobs(t *testing.T) {
	if testing.Short() {
		t.Skip("builds three binaries")
	}
	bin := buildCommands(t, t.TempDir())

	const (
		email    = "admin@example.com"
		password = "correct horse battery"
	)
	dir := bootstrapped(t, bin["cmsdb"], email, password)

	// --demo seeds the sample document and the one queued job this drives.
	if stdout, stderr, code := run(t, bin["cmsdb"], nil, "seed", "--demo", "--db", dir); code != 0 {
		t.Fatalf("cmsdb seed --demo exited %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	} else if !strings.Contains(stdout, "noop") {
		t.Fatalf("cmsdb seed --demo queued no job:\n%s", stdout)
	}

	// --workers 0 first, so that the job is still there to look at. It is a
	// supported configuration and not a mistake: an operator running several
	// cmsd processes behind one proxy wants the workers in one of them.
	idle := start(t, bin["cmsd"], nil, "serve", "--db", dir,
		"--env", "development", "--addr", "127.0.0.1:0", "--timeout", "120s", "--workers", "0")
	env := earlEnv(t, idle.url)

	if _, stderr, code := run(t, bin["earl"], env, "login", "--dev", "--email", email); code != 0 {
		t.Fatalf("earl login --dev: %s", stderr)
	}
	earl := func(t *testing.T, args ...string) string {
		t.Helper()
		stdout, stderr, code := run(t, bin["earl"], env, args...)
		if code != 0 {
			t.Fatalf("earl %s exited %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), code, stdout, stderr)
		}
		return stdout
	}
	list := func(t *testing.T, args ...string) earlJobList {
		t.Helper()
		out := earl(t, append([]string{"job", "list", "--json"}, args...)...)
		var got earlJobList
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("earl job list --json: %v\n%s", err, out)
		}
		return got
	}

	pending := list(t, "--pending")
	if pending.Count != 1 {
		t.Fatalf("%d pending jobs with --workers 0, want the one --demo queued: %+v", pending.Count, pending.Jobs)
	}
	job := pending.Jobs[0]
	if job.Kind != "noop" {
		t.Errorf("kind = %q, want noop", job.Kind)
	}
	if job.Status != "pending" {
		t.Errorf("status = %q with no workers running, want pending", job.Status)
	}
	if job.Attempts != 0 {
		t.Errorf("attempts = %d with no workers running", job.Attempts)
	}

	t.Run("--workers 0 really runs none", func(t *testing.T) {
		// Long enough that a worker polling at its shortest interval would
		// have claimed it several times over.
		time.Sleep(500 * time.Millisecond)
		if got := list(t, "--pending"); got.Count != 1 || got.Jobs[0].Attempts != 0 {
			t.Errorf("something claimed a job on a server started with --workers 0: %+v", got.Jobs)
		}
	})

	t.Run("the failed list is empty and says which question it asked", func(t *testing.T) {
		if got := list(t, "--failed"); got.Count != 0 {
			t.Errorf("%d failed jobs on a fresh database: %+v", got.Count, got.Jobs)
		}
		// The human rendering names the question, so an empty list explains
		// itself rather than looking like a broken queue.
		if out := earl(t, "job", "list", "--failed"); !strings.Contains(out, "failed") {
			t.Errorf("the empty list does not say what was asked:\n%s", out)
		}
	})

	t.Run("retrying a job that has not failed is refused", func(t *testing.T) {
		stdout, stderr, code := run(t, bin["earl"], env, "job", "retry", job.UID)
		if code == 0 {
			t.Fatalf("retrying a pending job succeeded:\n%s", stdout)
		}
		if !strings.Contains(stderr, "has not failed") {
			t.Errorf("stderr = %q, want it to say the job has not failed", stderr)
		}
	})

	t.Run("--pending and --failed together are refused", func(t *testing.T) {
		_, stderr, code := run(t, bin["earl"], env, "job", "list", "--pending", "--failed")
		if code == 0 {
			t.Error("--pending and --failed together were accepted")
		}
		if !strings.Contains(stderr, "not both") {
			t.Errorf("stderr = %q, want it to explain the contradiction", stderr)
		}
	})

	// Now the same database with a worker, through the one shutdown path so
	// that the SQLite lock is released before the second server opens it.
	resp, err := http.Get(idle.url + "/__development/shut-it-down")
	if err != nil {
		t.Fatalf("shut-it-down: %v", err)
	}
	_ = resp.Body.Close()
	if code := idle.wait(t, 30*time.Second); code != 0 {
		t.Fatalf("cmsd exited %d after a shutdown request\nstderr: %s", code, idle.stderr())
	}

	working := start(t, bin["cmsd"], nil, "serve", "--db", dir,
		"--env", "development", "--addr", "127.0.0.1:0", "--timeout", "120s", "--workers", "2")
	env = earlEnv(t, working.url)
	if _, stderr, code := run(t, bin["earl"], env, "login", "--dev", "--email", email); code != 0 {
		t.Fatalf("earl login --dev against the second server: %s", stderr)
	}

	t.Run("the workers hosted in cmsd run the job", func(t *testing.T) {
		deadline := time.Now().Add(30 * time.Second)
		for {
			got := list(t)
			if got.Count == 1 && got.Jobs[0].Status == "completed" {
				if got.Jobs[0].Attempts != 1 {
					t.Errorf("attempts = %d for a job that succeeded first time", got.Jobs[0].Attempts)
				}
				if got.Jobs[0].Worker != "" {
					t.Errorf("a completed job still names a worker: %+v", got.Jobs[0])
				}
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("the workers never completed the job: %+v\nstderr: %s", got.Jobs, working.stderr())
			}
			time.Sleep(50 * time.Millisecond)
		}
	})

	t.Run("nothing is pending once it has run", func(t *testing.T) {
		if got := list(t, "--pending"); got.Count != 0 {
			t.Errorf("%d jobs still pending: %+v", got.Count, got.Jobs)
		}
	})

	t.Run("cmsdb check reports the queue", func(t *testing.T) {
		// It runs against a database another process has open, which is what
		// WAL and the busy timeout are for.
		stdout, stderr, code := run(t, bin["cmsdb"], nil, "check", "--db", dir)
		if code != 0 {
			t.Fatalf("cmsdb check exited %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		if !strings.Contains(stdout, "stuck job leases: 0") {
			t.Errorf("check does not report the leases:\n%s", stdout)
		}
	})
}
