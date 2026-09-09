// Copyright (c) 2026 Michael D Henderson.

package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// M11's end-to-end test: earl driven against a cmsd on a temporary database
// (AGENTS.md, "Testing"). It is the milestone's claim made good at the command
// line -- "if earl cannot do it, the API is incomplete" -- and it walks the
// whole of what M11 adds: a thread, a reply, a refusal that names the open
// thread count, a resolution, a sign-off, a withdrawal, and the history that
// records every one of them.

// TestEarlCommentsAndApprovals covers PLAN.md M11 acceptances 1, 3, 5, and 6
// through the commands.
func TestEarlCommentsAndApprovals(t *testing.T) {
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

	created := earl(t, "doc", "create", "--json",
		"--title", "The Second Source",
		"--slug", "the-second-source",
		"--cover-date", "2026-03-01",
		"--content", `{"body":"The quick brown fox jumps over the lazy dog."}`)
	var doc struct {
		UID string `json:"uid"`
	}
	if err := json.Unmarshal([]byte(created), &doc); err != nil {
		t.Fatalf("earl doc create --json: %v\n%s", err, created)
	}

	// A checked-in version, because an approval attaches to one: sign-off on
	// a draft that is still being written is sign-off a check-in would not
	// invalidate.
	earl(t, "doc", "checkout", doc.UID)
	earl(t, "doc", "checkin", doc.UID, "--note", "first cut")
	earl(t, "doc", "do", doc.UID, "--to", "review")

	// A thread and two replies. The replies belong to the discussion; they
	// are not two more open questions.
	var thread struct {
		UID string `json:"uid"`
	}
	opened := earl(t, "doc", "comment", "--json", doc.UID, "the second source is not named")
	if err := json.Unmarshal([]byte(opened), &thread); err != nil {
		t.Fatalf("earl doc comment --json: %v\n%s", err, opened)
	}
	earl(t, "doc", "comment", doc.UID, "chasing it now", "--reply-to", thread.UID)
	earl(t, "doc", "comment", doc.UID, "named in the third graf", "--reply-to", thread.UID)

	t.Run("the listing is threads, not comments", func(t *testing.T) {
		var list struct {
			Open    int `json:"open"`
			Threads []struct {
				UID     string `json:"uid"`
				Replies []struct {
					UID string `json:"uid"`
				} `json:"replies"`
			} `json:"threads"`
		}
		if err := json.Unmarshal([]byte(earl(t, "doc", "comments", "--json", doc.UID)), &list); err != nil {
			t.Fatalf("earl doc comments --json: %v", err)
		}
		if list.Open != 1 || len(list.Threads) != 1 || len(list.Threads[0].Replies) != 2 {
			t.Errorf("the discussion is %+v, want one open thread with two replies", list)
		}
		// And the human rendering says so too, since that is what a person
		// actually reads.
		if got := earl(t, "doc", "comments", doc.UID); !strings.Contains(got, "1 thread open") {
			t.Errorf("the listing does not say how many threads are open:\n%s", got)
		}
	})

	// Acceptance 5: an unresolved thread blocks a transition guarded by
	// comments_resolved, and the refusal names the open thread count.
	t.Run("an open thread blocks the move", func(t *testing.T) {
		earl(t, "doc", "approve", doc.UID)
		_, stderr, code := run(t, bin["earl"], env, "doc", "do", doc.UID, "--to", "approved")
		if code == 0 {
			t.Fatal("a document with an open thread moved out of review")
		}
		if !strings.Contains(stderr, "comments_resolved") {
			t.Errorf("stderr = %q, want it to name the guard", stderr)
		}
		if !strings.Contains(stderr, "1 thread unresolved") {
			t.Errorf("stderr = %q, want it to name the open thread count", stderr)
		}
	})

	t.Run("a reply cannot be resolved on its own", func(t *testing.T) {
		var list struct {
			Threads []struct {
				Replies []struct {
					UID string `json:"uid"`
				} `json:"replies"`
			} `json:"threads"`
		}
		if err := json.Unmarshal([]byte(earl(t, "doc", "comments", "--json", doc.UID)), &list); err != nil {
			t.Fatal(err)
		}
		reply := list.Threads[0].Replies[0].UID
		_, stderr, code := run(t, bin["earl"], env, "doc", "resolve", reply)
		if code == 0 {
			t.Fatal("a reply was resolved on its own")
		}
		if !strings.Contains(stderr, thread.UID) {
			t.Errorf("stderr = %q, want it to name the thread to resolve instead", stderr)
		}
	})

	// Acceptance 1: approving twice is neither an error nor two approvals.
	t.Run("approving twice is idempotent", func(t *testing.T) {
		earl(t, "doc", "approve", doc.UID)
		var got struct {
			Count    int  `json:"count"`
			Required int  `json:"required"`
			Met      bool `json:"met"`
			Approved bool `json:"approved"`
		}
		if err := json.Unmarshal([]byte(earl(t, "doc", "approvals", "--json", doc.UID)), &got); err != nil {
			t.Fatalf("earl doc approvals --json: %v", err)
		}
		if got.Count != 1 || got.Required != 1 || !got.Met || !got.Approved {
			t.Errorf("approvals are %+v, want one approval meeting the one the process wants", got)
		}
	})

	t.Run("resolving the thread lets the move through", func(t *testing.T) {
		earl(t, "doc", "resolve", thread.UID)
		if got := earl(t, "doc", "do", doc.UID, "--to", "approved"); !strings.Contains(got, "moved to approved") {
			t.Fatalf("the move is still refused:\n%s", got)
		}
	})

	// Acceptance 3: a new version drops the count to zero, and the old
	// version's approvals stay exactly where they were.
	t.Run("a new version drops the count and keeps the history", func(t *testing.T) {
		// A checkout and a check-in, and no transition: the way back to draft
		// is Revoke, which declares clear_approvals, and this subtest is
		// about the version changing rather than about the effect discarding
		// them.
		earl(t, "doc", "checkout", doc.UID)
		earl(t, "doc", "checkin", doc.UID, "--note", "second cut")

		var now struct {
			Version int `json:"version"`
			Count   int `json:"count"`
		}
		if err := json.Unmarshal([]byte(earl(t, "doc", "approvals", "--json", doc.UID)), &now); err != nil {
			t.Fatal(err)
		}
		if now.Version != 2 {
			t.Fatalf("the current version is %d, want 2; this proves nothing otherwise", now.Version)
		}
		if now.Count != 0 {
			t.Errorf("version 2 carries %d approvals, want 0", now.Count)
		}

		var before struct {
			Version int `json:"version"`
			Count   int `json:"count"`
		}
		if err := json.Unmarshal([]byte(earl(t, "doc", "approvals", "--json", doc.UID, "--version", "1")), &before); err != nil {
			t.Fatal(err)
		}
		if before.Version != 1 || before.Count != 1 {
			t.Errorf("version 1's approvals are %+v, want the one that was recorded", before)
		}
	})

	t.Run("an approval can be withdrawn", func(t *testing.T) {
		// Back round to review, which is the state the default process counts
		// approvals in. Revoke declares note_required, so the move carries
		// one.
		earl(t, "doc", "do", doc.UID, "--to", "draft", "--note", "one more pass")
		earl(t, "doc", "do", doc.UID, "--to", "review")
		earl(t, "doc", "approve", doc.UID)

		var got struct {
			Count     int  `json:"count"`
			Withdrawn bool `json:"withdrawn"`
		}
		if err := json.Unmarshal([]byte(earl(t, "doc", "approve", "--json", "--withdraw", doc.UID)), &got); err != nil {
			t.Fatal(err)
		}
		if !got.Withdrawn || got.Count != 0 {
			t.Errorf("the withdrawal reported %+v, want it removed and the count back to zero", got)
		}
		// Idempotent: withdrawing one that is not there is not an error.
		if err := json.Unmarshal([]byte(earl(t, "doc", "approve", "--json", "--withdraw", doc.UID)), &got); err != nil {
			t.Fatal(err)
		}
		if got.Withdrawn {
			t.Error("the second withdrawal reported that it removed something")
		}
	})

	// Acceptance 6: every comment and approval is in the document's history.
	t.Run("every comment and approval is in the history", func(t *testing.T) {
		got := earl(t, "doc", "events", doc.UID)
		for _, want := range []string{"Commented", "Comment resolved", "Approved", "Approval withdrawn"} {
			if !strings.Contains(got, want) {
				t.Errorf("the history does not record %q:\n%s", want, got)
			}
		}
	})
}
