// Copyright (c) 2026 Michael D Henderson.

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// M9's end-to-end test: earl driven against a cmsd on a temporary database
// (AGENTS.md, "Testing").
//
// It publishes a story, reads what appeared in the output tree, changes the
// slug and watches the file at the old address go away, and asks "cmsdb check"
// what the tree and the database disagree about.
//
// The workers are running in this cmsd, so the publish and the expiry happen
// the way they will in production: a job is scheduled and something else picks
// it up. That is why this test waits for a file rather than asserting on one
// immediately -- and the waiting is bounded, so a publish that never happens
// fails rather than hangs.

// earlPublication is a scheduled publish as earl --json prints it.
type earlPublication struct {
	UID          string    `json:"uid"`
	Version      int       `json:"version"`
	Job          string    `json:"job"`
	ScheduledFor time.Time `json:"scheduled_for"`
}

// earlResources is what the publisher has written for a document.
type earlResources struct {
	UID       string `json:"uid"`
	Live      int64  `json:"live_version_id"`
	Resources []struct {
		Channel  string `json:"channel_name"`
		URI      string `json:"uri"`
		Path     string `json:"path"`
		Checksum string `json:"checksum"`
		Bytes    int64  `json:"bytes"`
	} `json:"resources"`
}

// TestEarlPublish is PLAN.md M9 acceptance 2, 5, and 7 through the commands,
// with acceptance 1's pin visible in what "earl doc publish" prints.
func TestEarlPublish(t *testing.T) {
	if testing.Short() {
		t.Skip("builds three binaries")
	}
	bin := buildCommands(t, t.TempDir())

	const (
		email    = "admin@example.com"
		password = "correct horse battery"
	)
	dir := bootstrapped(t, bin["cmsdb"], email, password)

	// t.TempDir already exists, which is the only reason these need no mkdir:
	// cmsd refuses to create either root (invariant 19).
	scratch := t.TempDir()
	output := t.TempDir()

	proc := start(t, bin["cmsd"], nil,
		"serve", "--db", dir, "--env", "development", "--addr", "127.0.0.1:0",
		"--templates", templateTree(t), "--preview", scratch, "--output", output,
		"--workers", "1", "--timeout", "120s")
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

	earl(t, "category", "create", "--parent", "/", "--directory", "features", "--name", "Features")

	// A story that is filed, checked in, and approved, which is the shape a
	// publish needs: a publishable state and a checked-in version to pin.
	out := earl(t, "doc", "create", "--json", "--title", "A Feature", "--slug", "a-feature",
		"--cover-date", "2026-03-01", "--content", `{"body":"<p>Words.</p>"}`)
	var doc struct {
		UID string `json:"uid"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("earl doc create --json: %v\n%s", err, out)
	}
	earl(t, "doc", "categories", doc.UID, "--set", "/features/")
	earl(t, "doc", "checkout", doc.UID)
	earl(t, "doc", "checkin", doc.UID, "--note", "first cut")
	earl(t, "doc", "do", doc.UID, "--to", "review")
	// The sign-off M11 added: the review state now asks for one, and an
	// approval attaches to the version checked in above.
	earl(t, "doc", "approve", doc.UID)
	earl(t, "doc", "do", doc.UID, "--to", "approved")

	var scheduled earlPublication
	if err := json.Unmarshal([]byte(earl(t, "doc", "publish", "--json", doc.UID)), &scheduled); err != nil {
		t.Fatalf("earl doc publish --json: %v", err)
	}
	if scheduled.Version != 1 {
		t.Errorf("the publish pinned version %d, want the checked-in version 1", scheduled.Version)
	}
	if scheduled.Job == "" {
		t.Error("the publish scheduled no job")
	}

	const published = "features/2026/03/01/a-feature/index.html"
	waitForFile(t, output, published, true)

	t.Run("the file is what the template rendered", func(t *testing.T) {
		body, err := os.ReadFile(filepath.Join(output, filepath.FromSlash(published)))
		if err != nil {
			t.Fatalf("reading the published file: %v", err)
		}
		for _, want := range []string{
			"<title>A Feature</title>",
			"the features template",
			"<p>Words.</p>", // raw, not escaped
		} {
			if !strings.Contains(string(body), want) {
				t.Errorf("the published page does not contain %q:\n%s", want, body)
			}
		}
	})

	t.Run("and the resources say where it went", func(t *testing.T) {
		var got earlResources
		if err := json.Unmarshal([]byte(earl(t, "doc", "resources", "--json", doc.UID)), &got); err != nil {
			t.Fatalf("earl doc resources --json: %v", err)
		}
		if len(got.Resources) != 1 {
			t.Fatalf("resources = %+v, want one", got.Resources)
		}
		r := got.Resources[0]
		if r.URI != "/features/2026/03/01/a-feature" {
			t.Errorf("uri = %q, want the seeded format's answer", r.URI)
		}
		if r.Path != published {
			t.Errorf("path = %q, want %q", r.Path, published)
		}
		if r.Bytes == 0 || len(r.Checksum) != 64 {
			t.Errorf("resource = %+v, want a byte count and a checksum", r)
		}
		// PLAN.md M9 acceptance 5: live_version_id is set on success.
		if got.Live == 0 {
			t.Error("live_version_id is unset after a successful publish")
		}
	})

	// PLAN.md M9 acceptance 2: republishing after a slug change deletes the
	// file at the old URI and creates the new one.
	t.Run("a slug change moves the file", func(t *testing.T) {
		earl(t, "doc", "checkout", doc.UID)
		earl(t, "doc", "edit", doc.UID, "--slug", "a-renamed-feature")
		earl(t, "doc", "checkin", doc.UID, "--note", "renamed")
		earl(t, "doc", "publish", doc.UID)

		const renamed = "features/2026/03/01/a-renamed-feature/index.html"
		waitForFile(t, output, renamed, true)
		waitForFile(t, output, published, false)

		var got earlResources
		if err := json.Unmarshal([]byte(earl(t, "doc", "resources", "--json", doc.UID)), &got); err != nil {
			t.Fatalf("earl doc resources --json: %v", err)
		}
		if len(got.Resources) != 1 || got.Resources[0].Path != renamed {
			t.Errorf("resources = %+v, want only the new address", got.Resources)
		}
	})

	// PLAN.md M9 acceptance 7: the check reports both kinds of orphan.
	t.Run("cmsdb check reconciles the tree", func(t *testing.T) {
		stdout, stderr, code := run(t, bin["cmsdb"], nil, "check", "--db", dir, "--output", output)
		if code != 0 {
			t.Fatalf("cmsdb check exited %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		if !strings.Contains(stdout, "orphaned resources: 0") {
			t.Errorf("check printed %q, want it to reconcile the tree", stdout)
		}

		// A file nothing claims, and a row whose file is gone.
		stray := filepath.Join(output, "stray.html")
		if err := os.WriteFile(stray, []byte("nobody claims this"), 0o644); err != nil {
			t.Fatalf("writing a stray file: %v", err)
		}
		const renamed = "features/2026/03/01/a-renamed-feature/index.html"
		if err := os.Remove(filepath.Join(output, filepath.FromSlash(renamed))); err != nil {
			t.Fatalf("removing the published file: %v", err)
		}

		stdout, _, code = run(t, bin["cmsdb"], nil, "check", "--db", dir, "--output", output)
		if code == 0 {
			t.Errorf("a check that found orphans exited 0:\n%s", stdout)
		}
		for _, want := range []string{
			"orphaned resources: 2",
			"unknown file: stray.html",
			"missing file: " + renamed,
		} {
			if !strings.Contains(stdout, want) {
				t.Errorf("check printed %q, want it to contain %q", stdout, want)
			}
		}
	})
}

// TestPublishIsUnavailableWithoutTheOutputFlag is the other half of the
// wiring: a server started without --output serves everything else and says
// which flag it was not given.
func TestPublishIsUnavailableWithoutTheOutputFlag(t *testing.T) {
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
		"serve", "--db", dir, "--env", "development", "--addr", "127.0.0.1:0",
		"--templates", templateTree(t), "--timeout", "120s")
	env := earlEnv(t, proc.url)

	if _, stderr, code := run(t, bin["earl"], env, "login", "--dev", "--email", email); code != 0 {
		t.Fatalf("earl login --dev: %s", stderr)
	}

	out, _, code := run(t, bin["earl"], env, "doc", "create", "--json",
		"--title", "A Story", "--slug", "a-story", "--cover-date", "2026-03-01",
		"--content", `{"body":"Words."}`)
	if code != 0 {
		t.Fatalf("earl doc create: %s", out)
	}
	var doc struct {
		UID string `json:"uid"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("earl doc create --json: %v\n%s", err, out)
	}

	_, stderr, code := run(t, bin["earl"], env, "doc", "publish", doc.UID)
	if code == 0 {
		t.Fatal("a server with no output tree published something")
	}
	if !strings.Contains(stderr, "--output") {
		t.Errorf("stderr = %q, want it to name the flag that was not given", stderr)
	}
}

// TestServeRefusesAnOutputDirectoryThatIsNotThere is invariant 19's half that
// does not bend. A mistyped --output is a refusal at startup rather than a
// site published into a directory nobody can find, and nothing is created on
// the way to saying so.
func TestServeRefusesAnOutputDirectoryThatIsNotThere(t *testing.T) {
	if testing.Short() {
		t.Skip("builds three binaries")
	}
	bin := buildCommands(t, t.TempDir())

	const (
		email    = "admin@example.com"
		password = "correct horse battery"
	)
	dir := bootstrapped(t, bin["cmsdb"], email, password)
	missing := filepath.Join(t.TempDir(), "not-there")

	_, stderr, code := run(t, bin["cmsd"], nil,
		"serve", "--db", dir, "--env", "development", "--addr", "127.0.0.1:0",
		"--templates", templateTree(t), "--output", missing, "--timeout", "5s")
	if code == 0 {
		t.Fatal("cmsd started with an output directory that is not there")
	}
	if !strings.Contains(stderr, missing) {
		t.Errorf("stderr = %q, want it to name the directory", stderr)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("Stat(%q) = %v, want it still not to exist (invariant 19)", missing, err)
	}
}

// waitForFile waits for a path under root to exist, or to stop existing.
//
// The publish and the expiry are jobs, and the worker that runs them is inside
// the cmsd this test started; there is no synchronous moment to assert at. The
// wait is bounded so that a publish that never happens fails the test rather
// than hanging it.
func waitForFile(t *testing.T, root, path string, want bool) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(path))
	deadline := time.Now().Add(20 * time.Second)
	for {
		_, err := os.Stat(full)
		if (err == nil) == want {
			return
		}
		if time.Now().After(deadline) {
			verb := "appear"
			if !want {
				verb = "go away"
			}
			listed, _ := filepath.Glob(filepath.Join(root, "*"))
			t.Fatalf("%s did not %s within the deadline; the tree holds %v", path, verb, listed)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
