// Copyright (c) 2026 Michael D Henderson.

package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// M7's end-to-end test: earl driven against a cmsd on a temporary database
// (AGENTS.md, "Testing"). It builds a category tree, files a story in it, reads
// the address that follows, moves a section, and watches the address move with
// it -- because "if earl cannot do it, the API is incomplete".

// earlCategory is a category as earl --json prints it.
type earlCategory struct {
	UID       string `json:"uid"`
	Site      int64  `json:"site"`
	Path      string `json:"path"`
	Name      string `json:"name"`
	Directory string `json:"directory"`
	Parent    string `json:"parent"`
	Depth     int    `json:"depth"`
}

// TestEarlCategoriesAndURIs is PLAN.md M7 acceptance 1, 3, 4, and 5 through the
// commands.
func TestEarlCategoriesAndURIs(t *testing.T) {
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
	earlFails := func(t *testing.T, args ...string) string {
		t.Helper()
		stdout, stderr, code := run(t, bin["earl"], env, args...)
		if code == 0 {
			t.Fatalf("earl %s was expected to fail and did not\nstdout: %s",
				strings.Join(args, " "), stdout)
		}
		return stderr
	}

	// "cmsdb seed" creates the site, its root category, and one output
	// channel, so a freshly seeded database can answer "what is this
	// document's address" without anything else being configured.
	t.Run("a seeded database has a root category and a channel", func(t *testing.T) {
		if got := earl(t, "category", "list"); !strings.Contains(got, "/") {
			t.Errorf("category list = %q, want the site's root", got)
		}
		if got := earl(t, "output-channel", "list"); !strings.Contains(got, "%{categories}") {
			t.Errorf("output-channel list = %q, want the seeded channel's URI format", got)
		}
	})

	// The tree PLAN.md M7 acceptance 1 asks for: three levels and two
	// siblings.
	create := func(t *testing.T, parent, directory string) earlCategory {
		t.Helper()
		out := earl(t, "category", "create", "--json",
			"--parent", parent, "--directory", directory, "--name", directory)
		var c earlCategory
		if err := json.Unmarshal([]byte(out), &c); err != nil {
			t.Fatalf("earl category create --json: %v\n%s", err, out)
		}
		return c
	}
	create(t, "/", "features")
	create(t, "/", "culture")
	film := create(t, "/features/", "film")
	create(t, "/features/", "books")
	create(t, "/features/film/", "reviews")
	create(t, "/features/film/", "interviews")

	// A story filed in the deepest category, with a cover date, so the
	// seeded format -- %{categories}/%Y/%m/%d/%{slug} -- has everything it
	// needs.
	var doc struct {
		UID string `json:"uid"`
	}
	out := earl(t, "doc", "create", "--json", "--title", "A Film Piece",
		"--slug", "a-film-piece", "--cover-date", "2026-03-01", "--content", `{"body":"Words."}`)
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("earl doc create --json: %v\n%s", err, out)
	}
	earl(t, "doc", "categories", doc.UID, "--set", "/features/film/reviews/,/features/")

	// uriOf reads the document's address in the seeded channel.
	uriOf := func(t *testing.T, uid string) string {
		t.Helper()
		out := earl(t, "doc", "uris", "--json", uid)
		var got struct {
			URIs []struct {
				Name  string `json:"name"`
				URI   string `json:"uri"`
				File  string `json:"file"`
				URL   string `json:"url"`
				Error string `json:"error"`
			} `json:"uris"`
		}
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("earl doc uris --json: %v\n%s", err, out)
		}
		if len(got.URIs) != 1 {
			t.Fatalf("%d addresses, want the one seeded channel: %s", len(got.URIs), out)
		}
		if got.URIs[0].Error != "" {
			t.Fatalf("the address could not be built: %s", got.URIs[0].Error)
		}
		if strings.Contains(got.URIs[0].URI, "//") {
			t.Errorf("the URI %q contains a doubled slash", got.URIs[0].URI)
		}
		return got.URIs[0].URI
	}

	// Acceptance 3, from the outside: a nested category, a slug, and a format
	// containing %Y/%m/%d.
	t.Run("the address follows from the category, the date, and the slug", func(t *testing.T) {
		want := "/features/film/reviews/2026/03/01/a-film-piece"
		if got := uriOf(t, doc.UID); got != want {
			t.Errorf("uri = %q, want %q", got, want)
		}
	})

	// Acceptance 1, from the outside: moving a section rewrites every
	// descendant's path, and the document's address moves with it.
	t.Run("moving a category moves everything under it", func(t *testing.T) {
		earl(t, "category", "move", film.UID, "--parent", "/culture/")

		listed := earl(t, "category", "list")
		for _, want := range []string{
			"/culture/film/", "/culture/film/reviews/", "/culture/film/interviews/",
		} {
			if !strings.Contains(listed, want) {
				t.Errorf("after the move the listing has no %s:\n%s", want, listed)
			}
		}
		// The sibling that was not moved stayed where it was.
		if !strings.Contains(listed, "/features/books/") {
			t.Errorf("the move took a sibling with it:\n%s", listed)
		}
		if strings.Contains(listed, "/features/film/") {
			t.Errorf("the moved category is still at its old path:\n%s", listed)
		}

		want := "/culture/film/reviews/2026/03/01/a-film-piece"
		if got := uriOf(t, doc.UID); got != want {
			t.Errorf("uri = %q, want %q; the subtree rewrite did not reach the URI", got, want)
		}
	})

	// Acceptance 5, from the outside: a working draft may be invalid and a
	// check-in of the same content is refused, naming the offending field.
	t.Run("check-in refuses content the element type does not declare", func(t *testing.T) {
		var bad struct {
			UID string `json:"uid"`
		}
		out := earl(t, "doc", "create", "--json", "--title", "Typo",
			"--slug", "typo", "--content", `{"boyd":"typo"}`)
		if err := json.Unmarshal([]byte(out), &bad); err != nil {
			t.Fatalf("a working draft may be invalid, so creating one must succeed: %v\n%s", err, out)
		}

		stderr := earlFails(t, "doc", "checkin", bad.UID)
		if !strings.Contains(stderr, "boyd") {
			t.Errorf("the refusal does not name the offending field: %s", stderr)
		}

		// And fixing it lets the same document in.
		earl(t, "doc", "checkout", bad.UID)
		earl(t, "doc", "edit", bad.UID, "--content", `{"body":"Words."}`)
		earl(t, "doc", "checkin", bad.UID)
	})

	t.Run("a document filed nowhere has no address", func(t *testing.T) {
		var loose struct {
			UID string `json:"uid"`
		}
		out := earl(t, "doc", "create", "--json", "--title", "Unfiled", "--slug", "unfiled")
		if err := json.Unmarshal([]byte(out), &loose); err != nil {
			t.Fatal(err)
		}
		stderr := earlFails(t, "doc", "uris", loose.UID)
		if !strings.Contains(stderr, "category") {
			t.Errorf("the refusal does not say why: %s", stderr)
		}
	})

	// Acceptance 6, from the outside: a category-scoped grant covers its
	// subtree, and the documents outside it are not even visible.
	t.Run("a category grant covers its subtree and nothing else", func(t *testing.T) {
		earl(t, "admin", "grant", "--role", "viewer", "--privilege", "read",
			"--site", "1", "--category", "/culture/")

		// The description the server rendered back names the path and says the
		// grant is deep, which is what "**" means in a scope.
		got := earl(t, "admin", "grant", "--role", "writer", "--privilege", "edit",
			"--site", "1", "--category", "/features/")
		if !strings.Contains(got, "/features/**") {
			t.Errorf("the grant description = %q, want the path and the subtree marker", got)
		}

		shallow := earl(t, "admin", "grant", "--role", "editor", "--privilege", "edit",
			"--site", "1", "--category", "/features/books/", "--deep=false")
		if strings.Contains(shallow, "**") {
			t.Errorf("--deep=false produced a subtree grant: %q", shallow)
		}
	})

	// Element types and output channels are administered from here too.
	t.Run("element types and output channels", func(t *testing.T) {
		earl(t, "element-type", "create", "--key-name", "page", "--kind", "story",
			"--fixed-uri", "--schema", `{"fields":[{"name":"body","type":"block"}]}`)

		// A fixed-URI document uses the channel's fixed format, which carries
		// no date -- which is the whole point of a fixed URI.
		var page struct {
			UID string `json:"uid"`
		}
		out := earl(t, "doc", "create", "--json", "--title", "About Us",
			"--slug", "about-us", "--element-type", "page", "--content", `{"body":"Hello."}`)
		if err := json.Unmarshal([]byte(out), &page); err != nil {
			t.Fatal(err)
		}
		earl(t, "doc", "categories", page.UID, "--set", "/")
		if got := uriOf(t, page.UID); got != "/about-us" {
			t.Errorf("a fixed-uri document at the root = %q, want /about-us", got)
		}

		if got := earl(t, "element-type", "list"); !strings.Contains(got, "page") {
			t.Errorf("element-type list = %q, want the type just created", got)
		}
		earl(t, "output-channel", "create", "--site", "1", "--name", "Print",
			"--uri-format", "%{categories}/%Y-%m-%d/%{slug}", "--use-slug", "--file-ext", "txt")
		if got := earl(t, "output-channel", "list"); !strings.Contains(got, "Print") {
			t.Errorf("output-channel list = %q, want the channel just created", got)
		}
	})

	t.Run("deleting a category refuses what depends on it", func(t *testing.T) {
		listed := earl(t, "category", "list", "--json")
		var out struct {
			Categories []earlCategory `json:"categories"`
		}
		if err := json.Unmarshal([]byte(listed), &out); err != nil {
			t.Fatal(err)
		}
		byPath := map[string]string{}
		for _, c := range out.Categories {
			byPath[c.Path] = c.UID
		}

		if stderr := earlFails(t, "category", "delete", byPath["/culture/film/"]); !strings.Contains(stderr, "categories under it") {
			t.Errorf("deleting a category with children: %s", stderr)
		}
		if stderr := earlFails(t, "category", "delete", byPath["/culture/film/reviews/"]); !strings.Contains(stderr, "documents filed in it") {
			t.Errorf("deleting a category with documents in it: %s", stderr)
		}
		earl(t, "category", "delete", byPath["/culture/film/interviews/"])
	})
}
