// Copyright (c) 2026 Michael D Henderson.

package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// M10's end-to-end test: earl driven against a cmsd on a temporary database
// (AGENTS.md, "Testing").
//
// It publishes a story that references two others, watches all three files
// appear, and then checks one of the relatives out and asks for a dry run --
// which reports the refusal by name and schedules nothing. The workers are
// running in this cmsd, so the cascade happens the way it will in production:
// three jobs are scheduled and something else picks them up.

// earlRelatedPublication is a scheduled publish as earl --json prints it,
// including M10's two additions.
type earlRelatedPublication struct {
	UID     string `json:"uid"`
	Version int    `json:"version"`
	Job     string `json:"job"`
	Related []struct {
		UID     string `json:"uid"`
		Title   string `json:"title"`
		Version int    `json:"version"`
		Job     string `json:"job"`
	} `json:"related"`
	Refusals []struct {
		UID          string `json:"uid"`
		Title        string `json:"title"`
		ReferencedBy string `json:"referenced_by"`
		Reason       string `json:"reason"`
		Detail       string `json:"detail"`
	} `json:"refusals"`
	DryRun      bool `json:"dry_run"`
	WouldRefuse bool `json:"would_refuse"`
}

// TestEarlPublishCascade is PLAN.md M10 acceptance 5, 6, and 7 through the
// commands, with acceptance 2's "by name" visible in what earl prints.
func TestEarlPublishCascade(t *testing.T) {
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

	// story creates a filed, checked-in, approved document whose content
	// names the given relatives -- the shape a publish needs, three times.
	story := func(t *testing.T, title, slug string, refs ...string) string {
		t.Helper()
		content := map[string]any{"body": "<p>" + title + "</p>"}
		if len(refs) > 0 {
			content["related"] = refs
		}
		body, err := json.Marshal(content)
		if err != nil {
			t.Fatalf("encoding the content: %v", err)
		}
		out := earl(t, "doc", "create", "--json", "--title", title, "--slug", slug,
			"--cover-date", "2026-03-01", "--content", string(body))
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
		earl(t, "doc", "do", doc.UID, "--to", "approved")
		return doc.UID
	}

	footnote := story(t, "The Footnote", "the-footnote")
	sidebar := story(t, "The Sidebar", "the-sidebar", footnote)
	feature := story(t, "The Feature", "the-feature", sidebar)

	t.Run("a dry run reports the chain and schedules nothing", func(t *testing.T) {
		var plan earlRelatedPublication
		out := earl(t, "doc", "publish", "--json", "--dry-run", feature)
		if err := json.Unmarshal([]byte(out), &plan); err != nil {
			t.Fatalf("earl doc publish --dry-run --json: %v\n%s", err, out)
		}
		if !plan.DryRun {
			t.Error("the plan does not say it was a dry run")
		}
		if plan.Job != "" {
			t.Errorf("a dry run reported job %q; it schedules none", plan.Job)
		}
		if len(plan.Related) != 2 {
			t.Fatalf("the plan gathered %d related documents, want the sidebar and the footnote", len(plan.Related))
		}
		if plan.Related[0].UID != sidebar || plan.Related[1].UID != footnote {
			t.Errorf("the plan gathered %s then %s, want %s then %s",
				plan.Related[0].UID, plan.Related[1].UID, sidebar, footnote)
		}
		if plan.Related[0].Title != "The Sidebar" {
			t.Errorf("the plan names the sidebar %q, want its title", plan.Related[0].Title)
		}
		if len(plan.Refusals) != 0 {
			t.Errorf("the plan refused %v", plan.Refusals)
		}

		// Nothing was queued: the job list is what a person would look at,
		// and it is empty.
		jobs := earl(t, "job", "list")
		if strings.Contains(jobs, "publish") {
			t.Errorf("a dry run left a job on the queue:\n%s", jobs)
		}
	})

	t.Run("the publish cascades and every file appears", func(t *testing.T) {
		var scheduled earlRelatedPublication
		out := earl(t, "doc", "publish", "--json", feature)
		if err := json.Unmarshal([]byte(out), &scheduled); err != nil {
			t.Fatalf("earl doc publish --json: %v\n%s", err, out)
		}
		if scheduled.Job == "" {
			t.Error("the publish scheduled no job for the root")
		}
		if len(scheduled.Related) != 2 {
			t.Fatalf("the publish gathered %d related documents, want 2", len(scheduled.Related))
		}
		for _, rel := range scheduled.Related {
			if rel.Job == "" {
				t.Errorf("%s was gathered with no job of its own", rel.UID)
			}
			if rel.Version != 1 {
				t.Errorf("%s pinned version %d, want 1", rel.UID, rel.Version)
			}
		}

		for _, slug := range []string{"the-feature", "the-sidebar", "the-footnote"} {
			waitForFile(t, output, "features/2026/03/01/"+slug+"/index.html", true)
		}
	})

	t.Run("a checked-out relative is refused by name", func(t *testing.T) {
		earl(t, "doc", "checkout", sidebar)
		t.Cleanup(func() { run(t, bin["earl"], env, "doc", "cancel", sidebar) })

		// The dry run says what a real publish would do, and says it without
		// erroring -- which is the only way a report gets read.
		var plan earlRelatedPublication
		out := earl(t, "doc", "publish", "--json", "--dry-run", feature)
		if err := json.Unmarshal([]byte(out), &plan); err != nil {
			t.Fatalf("earl doc publish --dry-run --json: %v\n%s", err, out)
		}
		if !plan.WouldRefuse {
			t.Error("the plan does not say this server's policy would refuse")
		}
		if len(plan.Refusals) != 1 {
			t.Fatalf("the plan refused %v, want the sidebar alone", plan.Refusals)
		}
		if plan.Refusals[0].UID != sidebar {
			t.Errorf("the refusal names %q, want %s", plan.Refusals[0].UID, sidebar)
		}
		if plan.Refusals[0].Reason != "checked_out" {
			t.Errorf("the reason is %q, want checked_out", plan.Refusals[0].Reason)
		}
		if plan.Refusals[0].ReferencedBy != feature {
			t.Errorf("the refusal was referenced by %q, want %s", plan.Refusals[0].ReferencedBy, feature)
		}
		if len(plan.Related) != 0 {
			t.Errorf("the plan gathered %v; under fail nothing at all is published", plan.Related)
		}

		// And the real publish refuses, naming the document in what a person
		// reads rather than in a code they have to look up.
		stdout, stderr, code := run(t, bin["earl"], env, "doc", "publish", feature)
		if code == 0 {
			t.Fatalf("earl doc publish went ahead:\n%s", stdout)
		}
		if !strings.Contains(stderr, sidebar) {
			t.Errorf("the refusal is %q; it must name %s", stderr, sidebar)
		}
		if !strings.Contains(stderr+stdout, "The Sidebar") {
			t.Errorf("the refusal does not name the document's title:\nstdout: %s\nstderr: %s", stdout, stderr)
		}
	})
}
