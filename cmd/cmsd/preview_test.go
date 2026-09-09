// Copyright (c) 2026 Michael D Henderson.

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// M8's end-to-end test: earl driven against a cmsd on a temporary database
// (AGENTS.md, "Testing"). It renders a story, watches the cascade choose a
// different template as the story is refiled, reads the preview back off the
// mount that serves it, and asks the two questions a broken template raises --
// does it compile, and what happened when it ran.

// earlPreview is a preview as earl --json prints it.
type earlPreview struct {
	UID      string   `json:"uid"`
	Mode     string   `json:"mode"`
	Channel  string   `json:"channel"`
	Name     string   `json:"channel_name"`
	Version  int      `json:"version"`
	Draft    bool     `json:"draft"`
	Category string   `json:"category"`
	URI      string   `json:"uri"`
	URL      string   `json:"url"`
	Template string   `json:"template"`
	Searched []string `json:"searched"`
	Path     string   `json:"path"`
	Checksum string   `json:"checksum"`
	Bytes    int      `json:"bytes"`
	Valid    bool     `json:"valid"`
}

// templateTree is the checked-in fixture cmsd is pointed at.
//
// It is a fixture rather than a tree this helper builds, because nothing in
// this repository creates a directory and that includes a test helper
// (invariant 19). See testdata/templates/README.md.
func templateTree(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("testdata", "templates"))
	if err != nil {
		t.Fatalf("locating the template tree: %v", err)
	}
	return dir
}

// TestEarlPreview is PLAN.md M8 acceptance 1, 2, 4, and 5 through the
// commands.
func TestEarlPreview(t *testing.T) {
	if testing.Short() {
		t.Skip("builds three binaries")
	}
	bin := buildCommands(t, t.TempDir())

	const (
		email    = "admin@example.com"
		password = "correct horse battery"
	)
	dir := bootstrapped(t, bin["cmsdb"], email, password)

	// t.TempDir already exists, which is the only reason the scratch tree
	// needs no mkdir: cmsd refuses to create one (invariant 19).
	scratch := t.TempDir()
	proc := start(t, bin["cmsd"], nil,
		"serve", "--db", dir, "--env", "development", "--addr", "127.0.0.1:0",
		"--templates", templateTree(t), "--preview", scratch, "--timeout", "120s")
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
	earlFails := func(t *testing.T, args ...string) string {
		t.Helper()
		stdout, stderr, code := run(t, bin["earl"], env, args...)
		if code == 0 {
			t.Fatalf("earl %s was expected to fail and did not\nstdout: %s",
				strings.Join(args, " "), stdout)
		}
		return stderr
	}

	// The category tree the fixture templates hang off. /features/film/ is
	// deeper than any template in the tree, which is what makes the cascade
	// visible: a document filed there has to walk up to find one
	// (PLAN.md M8 acceptance 1).
	for _, directory := range []string{"features", "broken", "explode"} {
		earl(t, "category", "create", "--parent", "/", "--directory", directory, "--name", directory)
	}
	earl(t, "category", "create", "--parent", "/features/", "--directory", "film", "--name", "film")

	// newStory creates a story with everything the seeded URI format needs --
	// %{categories}/%Y/%m/%d/%{slug} -- and files it.
	newStory := func(t *testing.T, title, slug, path, elementType string) string {
		t.Helper()
		out := earl(t, "doc", "create", "--json", "--title", title, "--slug", slug,
			"--element-type", elementType, "--cover-date", "2026-03-01",
			"--content", `{"body":"<p>Words with <em>markup</em>.</p>"}`)
		var doc struct {
			UID string `json:"uid"`
		}
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatalf("earl doc create --json: %v\n%s", err, out)
		}
		earl(t, "doc", "categories", doc.UID, "--set", path)
		return doc.UID
	}

	previewJSON := func(t *testing.T, uid string, extra ...string) earlPreview {
		t.Helper()
		args := append([]string{"doc", "preview", "--json", uid}, extra...)
		out := earl(t, args...)
		var got earlPreview
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("earl doc preview --json: %v\n%s", err, out)
		}
		return got
	}

	story := newStory(t, "A Feature", "a-feature", "/features/film/", "story")

	t.Run("a story in /features/film/ finds the template in /features/", func(t *testing.T) {
		got := previewJSON(t, story)
		if got.Template != "htmx-app.localhost/features/story.gohtml" {
			t.Errorf("template = %q, want the one in /features/", got.Template)
		}
		if got.URI != "/features/film/2026/03/01/a-feature" {
			t.Errorf("uri = %q, want the seeded format's answer", got.URI)
		}
		if got.Mode != "preview" || !got.Valid || got.Path == "" || got.Bytes == 0 {
			t.Errorf("preview = %+v, want a written preview", got)
		}
		// Searched carries what the chosen template beat, deepest first: the
		// one that is not there, then the one that is.
		want := []string{
			"htmx-app.localhost/features/film/story.gohtml",
			"htmx-app.localhost/features/story.gohtml",
		}
		if len(got.Searched) != len(want) || got.Searched[0] != want[0] || got.Searched[1] != want[1] {
			t.Errorf("searched = %v, want %v", got.Searched, want)
		}
	})

	t.Run("and the rendered page comes back", func(t *testing.T) {
		page := earl(t, "doc", "preview", story)
		for _, want := range []string{
			"<title>A Feature</title>",
			"the features template",
			"/features/film/",
			"<p>Words with <em>markup</em>.</p>", // raw, not escaped
		} {
			if !strings.Contains(page, want) {
				t.Errorf("the rendered page does not contain %q:\n%s", want, page)
			}
		}
	})

	t.Run("and --url names where it is served", func(t *testing.T) {
		got := previewJSON(t, story)
		url := strings.TrimSpace(earl(t, "doc", "preview", "--url", story))
		if !strings.HasSuffix(url, got.Path) {
			t.Errorf("--url printed %q, want it to end with %q", url, got.Path)
		}
		if !strings.HasPrefix(url, proc.url) {
			t.Errorf("--url printed %q, want it on the server earl is pointed at (%s)", url, proc.url)
		}
	})

	// Refiling the story at the root makes the cascade fall back to the
	// site's template, with no configuration changed anywhere
	// (PLAN.md M8 acceptance 1).
	t.Run("and refiling it falls back to the site root", func(t *testing.T) {
		earl(t, "doc", "categories", story, "--set", "/")
		got := previewJSON(t, story)
		if got.Template != "htmx-app.localhost/story.gohtml" {
			t.Errorf("template = %q, want the site's root template", got.Template)
		}
		page := earl(t, "doc", "preview", story)
		if !strings.Contains(page, "the site root template") {
			t.Errorf("the rendered page did not come from the root template:\n%s", page)
		}
		earl(t, "doc", "categories", story, "--set", "/features/film/")
	})

	// PLAN.md M8 acceptance 4: the draft while it is checked out, the
	// checked-in version after.
	t.Run("the draft, and then the version", func(t *testing.T) {
		earl(t, "doc", "categories", story, "--set", "/")

		draft := previewJSON(t, story)
		if !draft.Draft || draft.Version != 1 {
			t.Errorf("preview = version %d draft=%v, want the open working draft", draft.Version, draft.Draft)
		}
		if page := earl(t, "doc", "preview", story); !strings.Contains(page, "draft=true") {
			t.Errorf("the page does not say it is a draft:\n%s", page)
		}

		earl(t, "doc", "checkout", story)
		earl(t, "doc", "checkin", story, "--note", "first cut")
		checkedIn := previewJSON(t, story)
		if checkedIn.Draft || checkedIn.Version != 1 {
			t.Errorf("preview = version %d draft=%v, want the checked-in version 1",
				checkedIn.Version, checkedIn.Draft)
		}

		earl(t, "doc", "checkout", story)
		earl(t, "doc", "edit", story, "--content", `{"body":"the second draft"}`)
		editing := previewJSON(t, story)
		if !editing.Draft || editing.Version != 2 {
			t.Errorf("preview = version %d draft=%v, want the checked-out draft, version 2",
				editing.Version, editing.Draft)
		}
		if page := earl(t, "doc", "preview", story); !strings.Contains(page, "the second draft") {
			t.Errorf("the preview of a checked-out draft does not show the draft:\n%s", page)
		}
		earl(t, "doc", "categories", story, "--set", "/features/film/")
	})

	// PLAN.md M8 acceptance 2.
	t.Run("validate reports a template that will not parse", func(t *testing.T) {
		uid := newStory(t, "Broken", "broken", "/broken/", "story")

		stdout, stderr, code := run(t, bin["earl"], env, "doc", "preview", "--validate", uid)
		if code == 0 {
			t.Fatalf("earl doc preview --validate accepted a template that will not parse\n%s", stdout)
		}
		if !strings.Contains(stdout, "invalid") ||
			!strings.Contains(stdout, "htmx-app.localhost/broken/story.gohtml") {
			t.Errorf("the report does not name the broken template:\nstdout: %s\nstderr: %s", stdout, stderr)
		}
		if !strings.Contains(stdout, "parse") {
			t.Errorf("the report does not say it is a parse failure:\n%s", stdout)
		}

		// It wrote nothing: validate parses and stops.
		before := previewFiles(t, scratch)
		run(t, bin["earl"], env, "doc", "preview", "--validate", uid)
		if after := previewFiles(t, scratch); after != before {
			t.Errorf("validate wrote %d files, want none", after-before)
		}

		// And the template that does parse validates.
		if out := earl(t, "doc", "preview", "--validate", story); !strings.HasPrefix(out, "ok") {
			t.Errorf("validate of a good template printed %q, want it to say ok", out)
		}
	})

	// PLAN.md M8 acceptance 5.
	t.Run("an execution failure names the template and the line", func(t *testing.T) {
		uid := newStory(t, "Explode", "explode", "/explode/", "story")

		before := previewFiles(t, scratch)
		stderr := earlFails(t, "doc", "preview", uid)
		if !strings.Contains(stderr, "htmx-app.localhost/explode/story.gohtml") {
			t.Errorf("the refusal does not name the template: %s", stderr)
		}
		if !strings.Contains(stderr, "line 3") {
			t.Errorf("the refusal does not name the line: %s", stderr)
		}
		if after := previewFiles(t, scratch); after != before {
			t.Errorf("a failed render left %d files behind, want none", after-before)
		}
	})

	// PLAN.md M8 acceptance 1, last case: no template anywhere.
	t.Run("no template anywhere names the element type and the search", func(t *testing.T) {
		earl(t, "element-type", "create", "--key-name", "note", "--name", "Note",
			"--schema", `{"fields":[{"name":"body","type":"block"}]}`)
		uid := newStory(t, "A Note", "a-note", "/features/", "note")

		stderr := earlFails(t, "doc", "preview", uid)
		for _, want := range []string{
			"note",
			"htmx-app.localhost/features/note.gohtml",
			"htmx-app.localhost/note.gohtml",
		} {
			if !strings.Contains(stderr, want) {
				t.Errorf("the refusal does not name %q: %s", want, stderr)
			}
		}
	})
}

// TestPreviewIsUnavailableWithoutTheFlags is the other half of the wiring: a
// server started without --templates serves everything else and says which
// flag it was not given.
func TestPreviewIsUnavailableWithoutTheFlags(t *testing.T) {
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
		t.Fatalf("decoding: %v\n%s", err, out)
	}

	_, stderr, code := run(t, bin["earl"], env, "doc", "preview", doc.UID)
	if code == 0 {
		t.Fatal("earl doc preview succeeded on a server with no template tree")
	}
	if !strings.Contains(stderr, "--templates") {
		t.Errorf("the refusal does not name the flag that was not given: %s", stderr)
	}
}

// TestCmsdRefusesAMissingTemplateTree is invariant 19 at the command
// boundary: --templates names a directory that must already exist, and cmsd
// says so while starting rather than on the first preview.
func TestCmsdRefusesAMissingTemplateTree(t *testing.T) {
	if testing.Short() {
		t.Skip("builds three binaries")
	}
	bin := buildCommands(t, t.TempDir())
	dir := bootstrapped(t, bin["cmsdb"], "admin@example.com", "correct horse battery")

	missing := filepath.Join(t.TempDir(), "no-such-tree")
	for _, flag := range []string{"--templates", "--preview"} {
		stdout, stderr, code := run(t, bin["cmsd"], nil,
			"serve", "--db", dir, "--addr", "127.0.0.1:0", "--timeout", "5s", flag, missing)
		if code == 0 {
			t.Errorf("cmsd serve %s %s started; the directory does not exist\n%s", flag, missing, stdout)
		}
		if !strings.Contains(stderr, missing) {
			t.Errorf("the failure does not name the directory: %s", stderr)
		}
	}
}

// previewFiles counts what is in the scratch tree.
//
// It is a count rather than a listing because what the assertions care about
// is whether a render wrote anything, and a preview is content-addressed: two
// renders of one version are one file, so a name-by-name comparison would
// report "nothing new" for a case that did write.
func previewFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading the preview tree: %v", err)
	}
	return len(entries)
}
