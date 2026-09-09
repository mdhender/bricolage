// Copyright (c) 2026 Michael D Henderson.

package render

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mdhender/bricolage/internal/domain"
)

func newScratch(t *testing.T) *Scratch {
	t.Helper()
	s, err := NewScratch(t.TempDir())
	if err != nil {
		t.Fatalf("NewScratch: %v", err)
	}
	return s
}

// TestScratchIsContentAddressed is the property the flat tree rests on: the
// same bytes are the same file, so two previews of one version are one write
// and no directory is ever needed.
func TestScratchIsContentAddressed(t *testing.T) {
	s := newScratch(t)
	body := []byte("<p>hello</p>")
	sum := sha256.Sum256(body)
	want := hex.EncodeToString(sum[:])

	first, err := s.Put(body, "html")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if first.Checksum != want {
		t.Errorf("checksum = %q, want %q", first.Checksum, want)
	}
	if first.Name != want+".html" {
		t.Errorf("name = %q, want %q", first.Name, want+".html")
	}
	if first.Bytes != len(body) {
		t.Errorf("bytes = %d, want %d", first.Bytes, len(body))
	}
	if got, want := first.Path(), PreviewPrefix+first.Name; got != want {
		t.Errorf("Path() = %q, want %q", got, want)
	}

	again, err := s.Put(body, "html")
	if err != nil {
		t.Fatalf("Put again: %v", err)
	}
	if again.Name != first.Name {
		t.Errorf("the same bytes went to %q and %q; a content-addressed tree has one name per body", first.Name, again.Name)
	}

	// Exactly one file, and it is the preview: the temporary file the write
	// went through is gone.
	entries, err := os.ReadDir(s.Dir())
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != first.Name {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the tree holds %v, want just %q", names, first.Name)
	}

	f, err := s.Open(first.Name)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = f.Close() }()
	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("reading it back: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("read back %q, want %q", got, body)
	}
}

// TestScratchWithNoExtension covers an output channel that produces a file
// without one.
func TestScratchWithNoExtension(t *testing.T) {
	s := newScratch(t)
	entry, err := s.Put([]byte("x"), "")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if strings.Contains(entry.Name, ".") {
		t.Errorf("name = %q, want no extension", entry.Name)
	}
	if _, err := s.Open(entry.Name); err != nil {
		t.Errorf("Open(%q): %v", entry.Name, err)
	}
}

// TestScratchRefusesAnEscapingName is the whole of the path safety for
// GET /preview/{name}: the name arrives from a URL, and anything that is not a
// checksum is not a preview.
func TestScratchRefusesAnEscapingName(t *testing.T) {
	s := newScratch(t)
	outside := filepath.Join(filepath.Dir(s.Dir()), "secret")
	if err := os.WriteFile(outside, []byte("not yours"), 0o600); err != nil {
		t.Fatalf("writing the file outside the tree: %v", err)
	}

	for _, name := range []string{
		"", ".", "..", "../secret", "../../etc/passwd",
		"secret", strings.Repeat("a", 63), strings.Repeat("a", 65),
		strings.Repeat("A", 64), // one filename with two spellings
		strings.Repeat("a", 64) + ".ht ml",
		strings.Repeat("a", 64) + "/x",
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidEntryName(name); !errors.Is(err, domain.ErrInvalid) {
				t.Errorf("ValidEntryName(%q) = %v, want a refusal", name, err)
			}
			if _, err := s.Open(name); !errors.Is(err, domain.ErrInvalid) {
				t.Errorf("Open(%q) = %v, want a refusal", name, err)
			}
		})
	}
}

// TestScratchOpenMissing distinguishes "not a preview name" from "no such
// preview": the first is the caller's mistake and the second is an expired
// scratch tree.
func TestScratchOpenMissing(t *testing.T) {
	s := newScratch(t)
	if _, err := s.Open(strings.Repeat("ab", 32) + ".html"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Open of a well-formed name that is not there = %v, want not found", err)
	}
}

// TestNewScratchRefusesAMissingDirectory is invariant 19: a tool that creates
// what it cannot find turns a typo into a working system nobody can find the
// files of.
func TestNewScratchRefusesAMissingDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "preivew")
	if _, err := NewScratch(missing); err == nil {
		t.Error("NewScratch accepted a directory that does not exist")
	}
	if _, err := os.Stat(missing); err == nil {
		t.Error("NewScratch created the directory it was given; nothing in this system creates a directory")
	}
	if _, err := NewScratch(""); !errors.Is(err, domain.ErrUnavailable) {
		t.Error("NewScratch with no directory did not say the server has no preview tree")
	}
}

// TestContentType is what the browser is told a preview is.
func TestContentType(t *testing.T) {
	sha := strings.Repeat("ab", 32)
	for _, tc := range []struct{ name, want string }{
		{sha, "text/html; charset=utf-8"},
		{sha + ".html", "text/html; charset=utf-8"},
		{sha + ".txt", "text/plain; charset=utf-8"},
	} {
		got := ContentType(tc.name)
		if !strings.HasPrefix(got, strings.Split(tc.want, ";")[0]) {
			t.Errorf("ContentType(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}
