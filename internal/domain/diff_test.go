// Copyright (c) 2026 Michael D Henderson.

package domain

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// update regenerates the golden file (AGENTS.md, "Testing"):
//
//	go test ./internal/domain/ -run TestRenderDiffGolden -update
var update = flag.Bool("update", false, "rewrite the golden files")

// TestDiffWordsIsLossless is the property that makes the output trustworthy:
// the equal and delete runs reproduce the older text's words, and the equal
// and insert runs reproduce the newer one exactly.
func TestDiffWordsIsLossless(t *testing.T) {
	cases := []struct{ from, to string }{
		{"", ""},
		{"", "hello world"},
		{"hello world", ""},
		{"the quick brown fox", "the quick brown fox"},
		{"the quick brown fox", "the slow brown fox"},
		{"the quick brown fox", "the quick brown fox jumps"},
		{"jumps over the lazy dog", "the lazy dog"},
		{"a b c d e", "e d c b a"},
		{"  leading space", "leading space"},
		{"one\ntwo\nthree", "one two three"},
		{"alpha beta gamma", "alpha GAMMA beta"},
	}

	for _, tc := range cases {
		t.Run(tc.from+" -> "+tc.to, func(t *testing.T) {
			ops := DiffWords(tc.from, tc.to)

			var newer, olderWords, newerWords strings.Builder
			for _, op := range ops {
				switch op.Kind {
				case DiffEqual:
					newer.WriteString(op.Text)
					olderWords.WriteString(words(op.Text))
					newerWords.WriteString(words(op.Text))
				case DiffInsert:
					newer.WriteString(op.Text)
					newerWords.WriteString(words(op.Text))
				case DiffDelete:
					olderWords.WriteString(words(op.Text))
				}
			}

			if got, want := newer.String(), tc.to; got != want {
				t.Errorf("equal+insert = %q, want the newer text %q", got, want)
			}
			if got, want := olderWords.String(), words(tc.from); got != want {
				t.Errorf("equal+delete words = %q, want %q", got, want)
			}
			if got, want := newerWords.String(), words(tc.to); got != want {
				t.Errorf("equal+insert words = %q, want %q", got, want)
			}
		})
	}
}

// words reduces a text to its words separated by single spaces, which is what
// a word diff preserves. Whitespace is a delimiter here, not content.
func words(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return ""
	}
	return strings.Join(f, " ") + " "
}

// TestDiffWordsIgnoresRewrapping is why the diff is word level rather than
// line level: re-flowing a paragraph changes no words, so it is not a change.
func TestDiffWordsIgnoresRewrapping(t *testing.T) {
	const before = "The quick brown fox\njumps over the lazy dog."
	const after = "The quick brown fox jumps over\nthe lazy dog."

	for _, op := range DiffWords(before, after) {
		if op.Kind != DiffEqual {
			t.Fatalf("rewrapping produced a %s run: %q", op.Kind, op.Text)
		}
	}
}

// TestDiffWordsIsMinimal checks that an edit in the middle of a long text
// reports only the edit, which is the whole reason for trimming the common
// head and tail before doing anything quadratic.
func TestDiffWordsIsMinimal(t *testing.T) {
	head := strings.Repeat("alpha ", 200)
	tail := strings.Repeat("omega ", 200)
	ops := DiffWords(head+"middle "+tail, head+"centre "+tail)

	var deleted, inserted []string
	for _, op := range ops {
		switch op.Kind {
		case DiffDelete:
			deleted = append(deleted, strings.Fields(op.Text)...)
		case DiffInsert:
			inserted = append(inserted, strings.Fields(op.Text)...)
		}
	}
	if len(deleted) != 1 || deleted[0] != "middle" {
		t.Errorf("deleted = %v, want [middle]", deleted)
	}
	if len(inserted) != 1 || inserted[0] != "centre" {
		t.Errorf("inserted = %v, want [centre]", inserted)
	}
}

// TestDiffVersionsReportsEveryField checks that an unchanged field is reported
// as compared-and-equal rather than omitted.
func TestDiffVersionsReportsEveryField(t *testing.T) {
	from := Version{Number: 1, Title: "One", Slug: "one", Content: `{"a":1}`}
	to := Version{Number: 2, Title: "Two", Slug: "one", Content: `{"a":1}`}

	fields := DiffVersions(from, to)
	if len(fields) != len(diffFields) {
		t.Fatalf("got %d fields, want %d", len(fields), len(diffFields))
	}
	changed := map[string]bool{}
	for _, f := range fields {
		changed[f.Field] = f.Changed
	}
	if !changed["title"] {
		t.Error("title is reported unchanged")
	}
	for _, name := range []string{"slug", "cover_date", "content"} {
		if changed[name] {
			t.Errorf("%s is reported changed", name)
		}
	}
}

// TestRenderDiffGolden is PLAN.md M3 acceptance 7 at the level the rendering
// lives: the same two versions always render identically. earl prints exactly
// what this produces, and the end-to-end test compares its output to a golden
// file of its own.
func TestRenderDiffGolden(t *testing.T) {
	from := Version{
		Number:    1,
		Title:     "The Quick Brown Fox",
		Slug:      "quick-brown-fox",
		CoverDate: "2026-03-01",
		Content:   `{"body":"The quick brown fox jumps over the lazy dog."}`,
	}
	to := Version{
		Number:    2,
		Title:     "The Slow Brown Fox",
		Slug:      "quick-brown-fox",
		CoverDate: "2026-03-01",
		Content:   `{"body":"The slow brown fox ambles past the lazy sleeping dog."}`,
	}

	got := RenderDiff(from.Number, to.Number, DiffVersions(from, to))
	compareGolden(t, "diff_render.golden", got)

	// Two identical versions say so rather than printing nothing, because an
	// empty answer and a broken command look the same from a terminal.
	same := RenderDiff(1, 1, DiffVersions(from, from))
	compareGolden(t, "diff_identical.golden", same)
}

func compareGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
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
