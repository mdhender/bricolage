// Copyright (c) 2026 Michael D Henderson.

package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// M5's end-to-end test: earl driven against a cmsd on a temporary database
// (AGENTS.md, "Testing"). It hands a document around, gives it a deadline, and
// finds it again from the command line, because "if earl cannot do it, the API
// is incomplete".

// TestEarlQueues is PLAN.md M5 acceptance 1, 2, and 3 through the commands.
func TestEarlQueues(t *testing.T) {
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

	// The administrator's own uid, which is what an assignment names
	// (invariant 10).
	var me struct {
		User struct {
			UID string `json:"uid"`
		} `json:"user"`
	}
	if err := json.Unmarshal([]byte(earl(t, "whoami", "--json")), &me); err != nil {
		t.Fatalf("earl whoami --json: %v", err)
	}
	if me.User.UID == "" {
		t.Fatal("whoami reports no uid")
	}

	create := func(t *testing.T, title, slug string) string {
		t.Helper()
		var doc struct {
			UID string `json:"uid"`
		}
		out := earl(t, "doc", "create", "--json", "--title", title, "--slug", slug,
			"--cover-date", "2026-03-01", "--content", `{"body":"Words."}`)
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatalf("earl doc create --json: %v\n%s", err, out)
		}
		return doc.UID
	}

	// Two documents in review: one handed to somebody, one left in the pile.
	taken := create(t, "Taken", "taken")
	loose := create(t, "Nobody's", "nobodys")
	earl(t, "doc", "do", taken, "--to", "review")
	earl(t, "doc", "do", loose, "--to", "review")

	// Submit declares clear_assignee, so both are unassigned now whatever they
	// were before. This is PLAN.md M5 acceptance 3 seen from the outside.
	t.Run("submit cleared the assignee and set a due date", func(t *testing.T) {
		doc := show(t, earl, taken)
		if doc.AssignedTo != "" {
			t.Errorf("assigned_to = %q after a transition declaring clear_assignee", doc.AssignedTo)
		}
		if doc.DueAt == "" {
			t.Error("due_at is empty after a transition declaring set_due_in")
		}
	})

	earl(t, "doc", "assign", taken, "--to", me.User.UID)

	// Acceptance 1: the query the system we learned from could not express.
	t.Run("in review and unassigned", func(t *testing.T) {
		got := earl(t, "doc", "list", "--state", "review", "--unassigned")
		if !strings.Contains(got, loose) {
			t.Errorf("the unassigned document is missing from the list:\n%s", got)
		}
		if strings.Contains(got, taken) {
			t.Errorf("an assigned document is in the unassigned list:\n%s", got)
		}
	})

	t.Run("assigned to me", func(t *testing.T) {
		got := earl(t, "doc", "list", "--assignee", "me")
		if !strings.Contains(got, taken) || strings.Contains(got, loose) {
			t.Errorf("--assignee me = \n%s\nwant just %s", got, taken)
		}
	})

	t.Run("the filters refuse a contradiction", func(t *testing.T) {
		_, stderr, code := run(t, bin["earl"], env,
			"doc", "list", "--assignee", "me", "--unassigned")
		if code == 0 {
			t.Error("--assignee and --unassigned together were accepted")
		}
		if !strings.Contains(stderr, "nobody") {
			t.Errorf("stderr = %q, want it to explain the contradiction", stderr)
		}
	})

	// Acceptance 2, from the outside: both changes are in the history.
	t.Run("assignment and due dates are in the history", func(t *testing.T) {
		earl(t, "doc", "due", taken, "--at", "2026-03-01")
		got := earl(t, "doc", "events", taken)
		for _, want := range []string{"Assigned", "Due date changed", "Moved"} {
			if !strings.Contains(got, want) {
				t.Errorf("the history does not record %q:\n%s", want, got)
			}
		}
	})

	t.Run("a due date can be cleared without unassigning", func(t *testing.T) {
		earl(t, "doc", "due", taken, "--clear")
		doc := show(t, earl, taken)
		if doc.DueAt != "" {
			t.Errorf("due_at = %q after --clear", doc.DueAt)
		}
		if doc.AssignedTo != me.User.UID {
			t.Errorf("assigned_to = %q after clearing the due date, want %q", doc.AssignedTo, me.User.UID)
		}
	})

	t.Run("a document can be returned to the pile", func(t *testing.T) {
		earl(t, "doc", "assign", taken, "--nobody")
		if doc := show(t, earl, taken); doc.AssignedTo != "" {
			t.Errorf("assigned_to = %q after --nobody", doc.AssignedTo)
		}
		got := earl(t, "doc", "list", "--state", "review", "--unassigned")
		if !strings.Contains(got, taken) {
			t.Errorf("the returned document is not in the unassigned list:\n%s", got)
		}
	})

	// The saved queues, which live in configuration rather than in the schema,
	// so asking the server is the only way to know what it serves.
	t.Run("the saved queues", func(t *testing.T) {
		menu := earl(t, "queue")
		for _, want := range []string{"mine", "unassigned", "needs-editor", "overdue"} {
			if !strings.Contains(menu, want) {
				t.Errorf("the menu does not list %q:\n%s", want, menu)
			}
		}

		// "needs-editor" is the named form of acceptance 1's query, and it
		// must agree with the same filter asked directly.
		queue := earl(t, "queue", "needs-editor")
		if !strings.Contains(queue, loose) || !strings.Contains(queue, taken) {
			t.Errorf("needs-editor = \n%s\nwant both unassigned documents", queue)
		}

		if _, stderr, code := run(t, bin["earl"], env, "queue", "everything-important"); code == 0 {
			t.Error("a queue nobody defined was served")
		} else if !strings.Contains(stderr, "mine") {
			t.Errorf("stderr = %q, want it to name the queues that do exist", stderr)
		}
	})
}

// earlDoc is the part of a document response these assertions read.
type earlDoc struct {
	UID        string `json:"uid"`
	State      string `json:"state"`
	AssignedTo string `json:"assigned_to"`
	DueAt      string `json:"due_at"`
}

// show reads a document back as JSON, because the human rendering is
// column-aligned with spaces and a test that matched it would be a test of the
// tab writer.
func show(t *testing.T, earl func(*testing.T, ...string) string, uid string) earlDoc {
	t.Helper()
	out := earl(t, "doc", "show", "--json", uid)
	var doc earlDoc
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("earl doc show --json: %v\n%s", err, out)
	}
	return doc
}
