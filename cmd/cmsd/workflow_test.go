// Copyright (c) 2026 Michael D Henderson.

package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// M4's end-to-end test: earl driven against a cmsd on a temporary database
// (AGENTS.md, "Testing"). It walks a document through the whole default story
// workflow from the command line, because "if earl cannot do it, the API is
// incomplete".

// TestEarlWorkflowCycle is PLAN.md M4 acceptance 1, 2 and 3 through the
// commands.
func TestEarlWorkflowCycle(t *testing.T) {
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

	// state reads the document back as JSON, because the human rendering is
	// column-aligned with spaces and a test that matched it would be a test of
	// the tab writer.
	state := func(t *testing.T, uid string) string {
		t.Helper()
		var got struct {
			State string `json:"state"`
		}
		if err := json.Unmarshal([]byte(earl(t, "doc", "show", "--json", uid)), &got); err != nil {
			t.Fatalf("earl doc show --json: %v", err)
		}
		return got.State
	}

	// "cmsdb seed" reports the workflow the migration created, so that "what
	// does a fresh database contain" is still one command's output.
	if got, _, _ := run(t, bin["cmsdb"], nil, "seed", "--db", dir); !strings.Contains(got, "workflow: ") {
		t.Errorf("cmsdb seed does not report the default workflow:\n%s", got)
	}

	created := earl(t, "doc", "create", "--json",
		"--title", "The Quick Brown Fox",
		"--slug", "quick-brown-fox",
		"--cover-date", "2026-03-01",
		"--content", `{"body":"The quick brown fox jumps over the lazy dog."}`)
	var doc struct {
		UID      string `json:"uid"`
		State    string `json:"state"`
		Workflow string `json:"workflow"`
	}
	if err := json.Unmarshal([]byte(created), &doc); err != nil {
		t.Fatalf("earl doc create --json: %v\n%s", err, created)
	}
	if doc.State != "draft" || doc.Workflow == "" {
		t.Fatalf("the created document is %+v, want a draft in the default workflow", doc)
	}

	// Acceptance 1: the menu lists every way out of draft, refusals included.
	t.Run("the menu lists refusals with reasons", func(t *testing.T) {
		got := earl(t, "doc", "transitions", doc.UID)
		for _, want := range []string{"state: draft", "review", "Submit", "archived", "Archive"} {
			if !strings.Contains(got, want) {
				t.Errorf("the menu does not mention %q:\n%s", want, got)
			}
		}
	})

	// Acceptance 2: a move the state machine does not contain is refused for
	// an administrator holding Publish over everything.
	t.Run("an undeclared move is refused for an administrator", func(t *testing.T) {
		stdout, stderr, code := run(t, bin["earl"], env, "doc", "do", doc.UID, "--to", "published")
		if code == 0 {
			t.Fatalf("draft -> published succeeded:\n%s", stdout)
		}
		if !strings.Contains(stderr, "not a declared transition") {
			t.Errorf("stderr = %q, want it to say the move is not in the machine", stderr)
		}
		if got := state(t, doc.UID); got != "draft" {
			t.Errorf("state = %q after a refused move, want draft", got)
		}
	})

	// Acceptance 3, at the command line: a guard refuses and names itself.
	t.Run("a guard refuses while the document is checked out", func(t *testing.T) {
		earl(t, "doc", "checkout", doc.UID)
		t.Cleanup(func() { earl(t, "doc", "cancel", doc.UID) })

		got := earl(t, "doc", "transitions", doc.UID)
		if !strings.Contains(got, "not_locked") {
			t.Errorf("the menu does not name the guard that refused:\n%s", got)
		}

		_, stderr, code := run(t, bin["earl"], env, "doc", "do", doc.UID, "--to", "review")
		if code == 0 {
			t.Fatal("a checked-out document was submitted")
		}
		if !strings.Contains(stderr, "not_locked") {
			t.Errorf("stderr = %q, want it to name the guard", stderr)
		}
	})

	// The whole default process, end to end, and back out of it again.
	t.Run("the document walks the default workflow", func(t *testing.T) {
		if got := earl(t, "doc", "do", doc.UID, "--to", "review"); !strings.Contains(got, "moved to review") {
			t.Fatalf("submit printed %q", got)
		}

		// Reject declares note_required, so the move without one is refused
		// and the move with one succeeds. That is the guard's whole shape:
		// a refusal that is a prompt rather than a verdict.
		if _, stderr, code := run(t, bin["earl"], env, "doc", "do", doc.UID, "--to", "draft"); code == 0 {
			t.Error("a note-requiring transition went through with no note")
		} else if !strings.Contains(stderr, "note_required") {
			t.Errorf("stderr = %q, want it to name note_required", stderr)
		}
		earl(t, "doc", "do", doc.UID, "--to", "draft", "--note", "the lede is buried")

		earl(t, "doc", "do", doc.UID, "--to", "review")

		// Approve declares approvals_met, and migration 0011 gave the review
		// state something to count (PLAN.md M11). The refusal is a prompt in
		// the same way note_required is: record a sign-off and the move goes
		// through.
		if _, stderr, code := run(t, bin["earl"], env, "doc", "do", doc.UID, "--to", "approved"); code == 0 {
			t.Error("an unapproved document moved out of review")
		} else if !strings.Contains(stderr, "approvals_met") {
			t.Errorf("stderr = %q, want it to name approvals_met", stderr)
		}

		// An approval attaches to a version so that a change invalidates the
		// sign-off, and this document's only version is still its open
		// working draft: approving one would sign off on something that is
		// still being written.
		//
		// This is also why the publish guard has_checked_in_version can no
		// longer refuse in the default process. Reaching "approved" now
		// requires an approval, an approval requires a checked-in version,
		// and the guard asks for exactly that; it is still enforced and still
		// necessary for a process that asks for no approvals, and it is
		// covered directly in internal/workflow (TestPublishEffectRefusesA
		// DocumentWithNoCheckedInVersion and the guard table beside it).
		if _, stderr, code := run(t, bin["earl"], env, "doc", "approve", doc.UID); code == 0 {
			t.Error("an open working draft was approved")
		} else if !strings.Contains(stderr, "working draft") {
			t.Errorf("stderr = %q, want it to say the current version is a working draft", stderr)
		}

		earl(t, "doc", "checkout", doc.UID)
		earl(t, "doc", "checkin", doc.UID, "--note", "ready")
		earl(t, "doc", "approve", doc.UID)
		earl(t, "doc", "do", doc.UID, "--to", "approved")

		if got := earl(t, "doc", "do", doc.UID, "--to", "published"); !strings.Contains(got, "moved to published") {
			t.Fatalf("publish printed %q", got)
		}

		// Published is a state, not an exit: the document is still in the
		// workflow and can be revised.
		if got := earl(t, "doc", "transitions", doc.UID); !strings.Contains(got, "draft") {
			t.Errorf("a published document has nowhere to go:\n%s", got)
		}
		earl(t, "doc", "do", doc.UID, "--to", "draft")
		if got := state(t, doc.UID); got != "draft" {
			t.Errorf("state = %q after the revise, want draft", got)
		}
	})

	// The history is a query rather than a log grep (DESIGN.md 10).
	t.Run("every move is in the history", func(t *testing.T) {
		got := earl(t, "doc", "events", doc.UID)
		if n := strings.Count(got, "Moved"); n < 6 {
			t.Errorf("the history records %d moves, want one per transition:\n%s", n, got)
		}
	})

	t.Run("the list shows the state", func(t *testing.T) {
		got := earl(t, "doc", "list")
		if !strings.Contains(got, "STATE") || !strings.Contains(got, "draft") {
			t.Errorf("the list does not show the state:\n%s", got)
		}
	})
}
