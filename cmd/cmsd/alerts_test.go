// Copyright (c) 2026 Michael D Henderson.

package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// M12's end-to-end test: earl driven against a cmsd on a temporary database
// (AGENTS.md, "Testing"). It is the milestone's claim made good at the command
// line -- "if earl cannot do it, the API is incomplete" -- and it is the only
// test that proves the dispatcher is actually wired into "cmsd serve": every
// other one calls Once by hand, and this one waits for the server's own loop
// to deliver.

// TestEarlAlertsAndNotifications covers PLAN.md M12 acceptances 1, 2, and 3
// through the commands.
func TestEarlAlertsAndNotifications(t *testing.T) {
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

	var me struct {
		User struct {
			UID string `json:"uid"`
		} `json:"user"`
	}
	if err := json.Unmarshal([]byte(earl(t, "whoami", "--json")), &me); err != nil {
		t.Fatalf("earl whoami --json: %v", err)
	}

	t.Run("the vocabulary is readable from the client", func(t *testing.T) {
		out := earl(t, "alert", "events")
		for _, want := range []string{"document.transitioned", "document.published"} {
			if !strings.Contains(out, want) {
				t.Errorf("earl alert events does not list %q:\n%s", want, out)
			}
		}
	})

	// PLAN.md M12 acceptance 3, at the command line: a pattern that does not
	// compile is refused when the rule is written, and the message names it.
	t.Run("a broken pattern is refused when the rule is written", func(t *testing.T) {
		stdout, stderr, code := run(t, bin["earl"], env, "alert", "create",
			"--name", "Broken", "--event", "document.published",
			"--channel", "in_app", "--target", "user:"+me.User.UID,
			"--condition", "title:matches:([unclosed")
		if code == 0 {
			t.Fatalf("earl alert create accepted an uncompilable pattern: %s", stdout)
		}
		if !strings.Contains(stderr, "([unclosed") {
			t.Errorf("the refusal does not name the pattern: %s", stderr)
		}
		if strings.Contains(earl(t, "alert", "list"), "Broken") {
			t.Error("the refused rule was written anyway")
		}
	})

	// The rule the test is about: a transition into "review" notifies one
	// person. The default workflow calls that state "review" where PLAN.md
	// M12's criterion writes "legal", and the payload key a transition writes
	// is "to".
	var rule struct {
		UID string `json:"uid"`
	}
	created := earl(t, "alert", "create", "--json",
		"--name", "Into review",
		"--event", "document.transitioned",
		"--channel", "in_app",
		"--target", "user:"+me.User.UID,
		"--condition", "to:eq:review")
	if err := json.Unmarshal([]byte(created), &rule); err != nil {
		t.Fatalf("earl alert create --json: %v\n%s", err, created)
	}

	// PLAN.md M12 acceptance 2: a second rule with two conditions, one of
	// which fails. It must fire nothing, so the inbox below holds one line
	// and not two.
	earl(t, "alert", "create",
		"--name", "Into review, but media",
		"--event", "document.transitioned",
		"--channel", "in_app",
		"--target", "user:"+me.User.UID,
		"--condition", "to:eq:review",
		"--condition", "kind:eq:media")

	var doc struct {
		UID string `json:"uid"`
	}
	out := earl(t, "doc", "create", "--json",
		"--title", "The Budget, Explained",
		"--slug", "the-budget-explained",
		"--cover-date", "2026-03-01",
		"--content", `{"body":"Where the money went."}`)
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("earl doc create --json: %v\n%s", err, out)
	}
	earl(t, "doc", "checkout", doc.UID)
	earl(t, "doc", "checkin", doc.UID, "--note", "first cut")
	earl(t, "doc", "do", doc.UID, "--to", "review")

	// The server's own dispatcher delivers this, on its poll, with nobody
	// calling anything. That is what this test is for.
	var inbox struct {
		Unread        int `json:"unread"`
		Notifications []struct {
			UID       string         `json:"uid"`
			Read      bool           `json:"read"`
			EventType string         `json:"event_type"`
			RuleName  string         `json:"rule_name"`
			Payload   map[string]any `json:"payload"`
		} `json:"notifications"`
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := json.Unmarshal([]byte(earl(t, "notification", "list", "--json")), &inbox); err != nil {
			t.Fatalf("earl notification list --json: %v", err)
		}
		if len(inbox.Notifications) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no notification arrived within 30s; the dispatcher is not running inside cmsd serve")
		}
		time.Sleep(200 * time.Millisecond)
	}

	// One notification and not two: the rule whose second condition fails
	// fired nothing (PLAN.md M12 acceptance 2). Give the dispatcher a moment
	// to prove it did not produce a second one late.
	time.Sleep(time.Second)
	if err := json.Unmarshal([]byte(earl(t, "notification", "list", "--json")), &inbox); err != nil {
		t.Fatalf("earl notification list --json: %v", err)
	}
	if len(inbox.Notifications) != 1 || inbox.Unread != 1 {
		t.Fatalf("inbox = %d notifications, %d unread; want exactly the one rule whose conditions all passed",
			len(inbox.Notifications), inbox.Unread)
	}
	got := inbox.Notifications[0]
	if got.EventType != "document.transitioned" || got.RuleName != "Into review" {
		t.Errorf("notified about %q by %q", got.EventType, got.RuleName)
	}
	if got.Payload["uid"] != doc.UID {
		t.Errorf("the notification is about %v, want %s", got.Payload["uid"], doc.UID)
	}

	t.Run("reading it", func(t *testing.T) {
		earl(t, "notification", "read", got.UID)
		var after struct {
			Unread        int        `json:"unread"`
			Notifications []struct{} `json:"notifications"`
		}
		if err := json.Unmarshal([]byte(earl(t, "notification", "list", "--json", "--unread")), &after); err != nil {
			t.Fatalf("earl notification list --unread --json: %v", err)
		}
		if after.Unread != 0 || len(after.Notifications) != 0 {
			t.Errorf("after reading it: %d unread, %d listed", after.Unread, len(after.Notifications))
		}
	})

	t.Run("switching a rule off keeps it", func(t *testing.T) {
		earl(t, "alert", "update", rule.UID, "--active=false")
		listing := earl(t, "alert", "list")
		if !strings.Contains(listing, "Into review") || !strings.Contains(listing, "off") {
			t.Errorf("the switched-off rule is not in the listing as off:\n%s", listing)
		}
	})

	t.Run("deleting one keeps what it already said", func(t *testing.T) {
		earl(t, "alert", "delete", rule.UID)
		if strings.Contains(earl(t, "alert", "list"), rule.UID) {
			t.Error("the deleted rule is still listed")
		}
		// The notification it produced survives it, carrying no rule:
		// notifications.rule_id is ON DELETE SET NULL.
		listing := earl(t, "notification", "list", "--json")
		if !strings.Contains(listing, got.UID) {
			t.Errorf("deleting the rule took its notification with it:\n%s", listing)
		}
	})
}
