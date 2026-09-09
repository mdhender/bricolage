// Copyright (c) 2026 Michael D Henderson.

package main

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// M3's end-to-end test: earl driven against a cmsd on a temporary database
// (AGENTS.md, "Testing"). It builds the three binaries, seeds a database,
// bootstraps an administrator, and drives the whole editorial cycle through
// the command line, because "if earl cannot do it, the API is incomplete".
//
// It runs against the listener rather than through the Caddy proxy, for the
// reason TestEarlLoginAndWhoami gives: CI has neither the Homebrew service nor
// a *.localhost certificate, and nothing here may start Caddy.

// updateGolden rewrites the golden files:
//
//	go test ./cmd/cmsd/ -run TestEarlDocumentCycle -update
var updateGolden = flag.Bool("update", false, "rewrite the golden files")

// earlEnv points earl at a server and at a credentials file of its own, so
// that a test never writes to the person running it. The directory is
// t.TempDir, which already exists: earl creates no directory either
// (invariant 19).
func earlEnv(t *testing.T, url string) []string {
	t.Helper()
	return []string{
		"EARL_CREDENTIALS=" + filepath.Join(t.TempDir(), "credentials.json"),
		"EARL_SERVER=" + url,
	}
}

// TestEarlDocumentCycle is PLAN.md M3 acceptance 1, 5, and 7 through the
// commands, plus the history that acceptance 6 asserts on inside the service.
func TestEarlDocumentCycle(t *testing.T) {
	if testing.Short() {
		t.Skip("builds three binaries")
	}
	bin := buildCommands(t, t.TempDir())

	const (
		email    = "admin@example.com"
		password = "correct horse battery"
	)
	dir := bootstrapped(t, bin["cmsdb"], email, password)
	proc := start(t, bin["cmsd"], nil,
		"serve", "--db", dir, "--env", "development", "--addr", "127.0.0.1:0", "--timeout", "120s")
	env := earlEnv(t, proc.url)

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

	// Create, with the content on standard input, which is how an agent hands
	// over a document without a temporary file.
	created := earl(t, "doc", "create", "--json",
		"--title", "The Quick Brown Fox",
		"--slug", "quick-brown-fox",
		"--cover-date", "2026-03-01",
		"--content", `{"body":"The quick brown fox jumps over the lazy dog."}`)
	var doc struct {
		UID     string `json:"uid"`
		Version struct {
			Version int  `json:"version"`
			Draft   bool `json:"draft"`
		} `json:"version"`
	}
	if err := json.Unmarshal([]byte(created), &doc); err != nil {
		t.Fatalf("earl doc create --json: %v\n%s", err, created)
	}
	if doc.UID == "" || doc.Version.Version != 1 || !doc.Version.Draft {
		t.Fatalf("the created document is %+v", doc)
	}

	// Acceptance 1: check out, edit, check in produces version 1.
	earl(t, "doc", "checkout", doc.UID)
	earl(t, "doc", "checkin", doc.UID, "--note", "first pass")

	// A second cycle, so that there are two versions to compare.
	earl(t, "doc", "checkout", doc.UID)
	earl(t, "doc", "edit", doc.UID,
		"--title", "The Slow Brown Fox",
		"--content", `{"body":"The slow brown fox ambles past the lazy sleeping dog."}`)
	earl(t, "doc", "checkin", doc.UID, "--note", "second pass")

	// Acceptance 7: the diff matches a golden file.
	compareGolden(t, filepath.Join("testdata", "doc_diff.golden"),
		earl(t, "doc", "diff", doc.UID, "--from", "1", "--to", "2"))

	t.Run("the history is a query", func(t *testing.T) {
		got := earl(t, "doc", "events", doc.UID)
		for _, want := range []string{"Document created", "Checked out", "Draft edited", "Checked in", "Admin"} {
			if !strings.Contains(got, want) {
				t.Errorf("the history does not mention %q:\n%s", want, got)
			}
		}
	})

	t.Run("a version is readable by number", func(t *testing.T) {
		got := earl(t, "doc", "show", doc.UID, "--version", "1")
		if !strings.Contains(got, "The Quick Brown Fox") {
			t.Errorf("version 1 reads:\n%s", got)
		}
		if strings.Contains(got, "working draft") {
			t.Errorf("version 1 is reported as a draft:\n%s", got)
		}
	})

	// Acceptance 5, second half: reverting leaves the latest checked-in
	// version intact.
	t.Run("revert keeps the latest checked-in version", func(t *testing.T) {
		earl(t, "doc", "checkout", doc.UID)
		earl(t, "doc", "edit", doc.UID, "--title", "Discarded")
		got := earl(t, "doc", "revert", doc.UID)
		if !strings.Contains(got, "The Slow Brown Fox") {
			t.Errorf("revert left:\n%s", got)
		}
		if strings.Contains(got, "Discarded") {
			t.Errorf("the discarded draft survived:\n%s", got)
		}
	})

	// Acceptance 5, first half: a document with no checked-in version is
	// deleted by a revert.
	t.Run("revert deletes a document that was never checked in", func(t *testing.T) {
		out := earl(t, "doc", "create", "--json", "--title", "Never Saved")
		var fresh struct {
			UID string `json:"uid"`
		}
		if err := json.Unmarshal([]byte(out), &fresh); err != nil {
			t.Fatal(err)
		}

		got := earl(t, "doc", "revert", fresh.UID)
		if !strings.Contains(got, "deleted") {
			t.Errorf("revert printed %q", got)
		}
		if _, _, code := run(t, bin["earl"], env, "doc", "show", fresh.UID); code == 0 {
			t.Error("the document is still readable after the revert that deleted it")
		}
	})

	// Cancelling is not reverting. Getting this wrong is how somebody loses an
	// afternoon to a button they thought closed a form.
	t.Run("cancel releases the lease and keeps the work", func(t *testing.T) {
		earl(t, "doc", "checkout", doc.UID)
		earl(t, "doc", "edit", doc.UID, "--title", "Half Written")
		got := earl(t, "doc", "cancel", doc.UID)
		if !strings.Contains(got, "Half Written") {
			t.Errorf("cancelling discarded the work:\n%s", got)
		}
		if !strings.Contains(got, "(nobody)") {
			t.Errorf("cancelling left the lease held:\n%s", got)
		}
		// Put the document back as it was, so the subtests do not depend on
		// the order they run in.
		earl(t, "doc", "revert", doc.UID)
	})

	t.Run("a second checkout is refused", func(t *testing.T) {
		earl(t, "doc", "checkout", doc.UID)
		t.Cleanup(func() { earl(t, "doc", "cancel", doc.UID) })

		stdout, stderr, code := run(t, bin["earl"], env, "doc", "checkout", doc.UID)
		if code == 0 {
			t.Fatalf("the second checkout succeeded:\n%s", stdout)
		}
		if !strings.Contains(stderr, "checked out by Admin") {
			t.Errorf("stderr = %q, want it to name who holds the lease", stderr)
		}
	})

	t.Run("the list shows what was created", func(t *testing.T) {
		got := earl(t, "doc", "list")
		if !strings.Contains(got, doc.UID) {
			t.Errorf("the list does not contain the document:\n%s", got)
		}
	})
}

// TestSeedDemo covers "cmsdb seed --demo", which is real now that there are
// documents to seed, and which refuses rather than inventing an account when
// there is nobody to attribute the content to.
func TestSeedDemo(t *testing.T) {
	if testing.Short() {
		t.Skip("builds three binaries")
	}
	bin := buildCommands(t, t.TempDir())
	cmsdb := bin["cmsdb"]

	t.Run("with no user it refuses and says why", func(t *testing.T) {
		dir := initDB(t, cmsdb)
		if _, stderr, code := run(t, cmsdb, nil, "seed", "--db", dir, "--demo"); code == 0 {
			t.Error("seed --demo succeeded with no user to attribute content to")
		} else if !strings.Contains(stderr, "bootstrap admin") {
			t.Errorf("stderr = %q, want it to name what is missing", stderr)
		}
	})

	t.Run("with an administrator it seeds one document, idempotently", func(t *testing.T) {
		dir := bootstrapped(t, cmsdb, "admin@example.com", "correct horse battery")

		stdout, stderr, code := run(t, cmsdb, nil, "seed", "--db", dir, "--demo")
		if code != 0 {
			t.Fatalf("seed --demo exited %d\nstderr: %s", code, stderr)
		}
		if !strings.Contains(stdout, "The Quick Brown Fox") {
			t.Errorf("seed --demo printed %q", stdout)
		}

		stdout, stderr, code = run(t, cmsdb, nil, "seed", "--db", dir, "--demo")
		if code != 0 {
			t.Fatalf("the second seed --demo exited %d\nstderr: %s", code, stderr)
		}
		if !strings.Contains(stdout, "already present") {
			t.Errorf("the second run printed %q, want it to report what was already there", stdout)
		}
	})
}

// compareGolden compares got to the file at path, or rewrites it under
// -update.
func compareGolden(t *testing.T, path, got string) {
	t.Helper()
	if *updateGolden {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v (run the test with -update to create it)", path, err)
	}
	if got != string(want) {
		t.Errorf("%s does not match:\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}
