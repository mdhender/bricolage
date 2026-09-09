// Copyright (c) 2026 Michael D Henderson.

package render

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"mime"
	"os"
	"path/filepath"
	"strings"

	"github.com/mdhender/bricolage/internal/domain"
)

// The preview scratch tree (PLAN.md M8).
//
// A preview is written to a scratch tree and served back by cmsd under
// /preview/ (DESIGN.md 8.4). Two decisions shape what that tree looks like,
// and both come from constraints this system already has.
//
// It is flat, and files in it are named by the SHA-256 of their own bytes.
// Nothing in this system creates a directory (invariant 19), so a scratch tree
// that mirrored the output tree's shape -- /features/film/2026/03/01/ -- could
// not be written at all: the first preview of the first story would need six
// directories nobody made. Content addressing is the shape that needs none,
// and it pays for itself twice: two previews of the same version against the
// same template are one file, and a stale preview is never served under a name
// that now means something else.
//
// It is written through a temporary file and renamed. A reader either sees the
// whole preview or no file at all, which is the same promise PLAN.md M8
// acceptance 5 asks of a failed render, made at the layer that actually holds
// a file descriptor.

// PreviewPrefix is the path previews are served under. It is a constant so
// that the route table, the handler, and the path in an API response are one
// string.
const PreviewPrefix = "/preview/"

// Scratch is the preview tree.
type Scratch struct {
	dir string
}

// NewScratch opens a scratch tree.
//
// The directory must already exist. That is invariant 19 again and it is worth
// saying why it applies to something as disposable as a preview: a tool that
// creates what it cannot find turns "--preview /tmp/preivew" into a working
// system nobody can find the files of.
func NewScratch(dir string) (*Scratch, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("render: no preview directory: %w", domain.ErrUnavailable)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("preview directory %s: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("preview directory %s: not a directory", dir)
	}
	return &Scratch{dir: dir}, nil
}

// Dir is the directory previews are written to.
func (s *Scratch) Dir() string { return s.dir }

// Entry is one written preview.
type Entry struct {
	// Name is the file's name within the tree, which is also the last segment
	// of the URL it is served at.
	Name string

	// Checksum is the SHA-256 of the bytes, in hex. Name is this plus the
	// extension.
	Checksum string

	// Bytes is how many were written.
	Bytes int
}

// Path is where the preview is served: PreviewPrefix plus the name.
func (e Entry) Path() string { return PreviewPrefix + e.Name }

// Put writes body into the tree and returns where it went.
//
// ext is the output channel's file extension, without the dot; an empty one
// means the file has none. It is carried into the name so that the browser is
// told what the preview is by the same rule that decides what the published
// file is called.
func (s *Scratch) Put(body []byte, ext string) (Entry, error) {
	sum := sha256.Sum256(body)
	checksum := hex.EncodeToString(sum[:])
	name, err := entryName(checksum, ext)
	if err != nil {
		return Entry{}, err
	}

	// A temporary file in the same directory, then a rename: the rename is
	// atomic on every filesystem this runs on, so a reader sees the whole
	// preview or no file. os.CreateTemp creates a file, not a directory
	// (invariant 19).
	tmp, err := os.CreateTemp(s.dir, ".preview-*")
	if err != nil {
		return Entry{}, fmt.Errorf("preview: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // a no-op once the rename has happened

	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return Entry{}, fmt.Errorf("preview %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return Entry{}, fmt.Errorf("preview %s: %w", name, err)
	}
	if err := os.Rename(tmpName, filepath.Join(s.dir, name)); err != nil {
		return Entry{}, fmt.Errorf("preview %s: %w", name, err)
	}
	return Entry{Name: name, Checksum: checksum, Bytes: len(body)}, nil
}

// Open reads a preview back.
//
// The name is validated before it is joined to anything. It arrives from a URL
// path, and a path segment from a URL joined to a directory without a check is
// the oldest hole there is.
func (s *Scratch) Open(name string) (fs.File, error) {
	if err := ValidEntryName(name); err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(s.dir, name))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("preview %s: %w", name, domain.ErrNotFound)
		}
		return nil, err
	}
	return f, nil
}

// ContentType is what a preview with this name is served as.
//
// It is derived from the extension and defaults to HTML, because an output
// channel with no extension is producing a page rather than a download and
// because "application/octet-stream" would make a browser save the preview
// instead of showing it.
func ContentType(name string) string {
	ext := filepath.Ext(name)
	if ext == "" {
		return "text/html; charset=utf-8"
	}
	if t := mime.TypeByExtension(ext); t != "" {
		return t
	}
	return "application/octet-stream"
}

// entryName is the filename for a checksum and an extension.
func entryName(checksum, ext string) (string, error) {
	if err := domain.ValidateChecksum(checksum); err != nil {
		return "", err
	}
	ext = strings.TrimPrefix(strings.TrimSpace(ext), ".")
	if ext == "" {
		return checksum, nil
	}
	if err := validExt(ext); err != nil {
		return "", err
	}
	return checksum + "." + ext, nil
}

// MaxExtLen bounds a file extension. It is generous for "html" and small
// enough that a channel misconfigured with a sentence in the column produces a
// refusal rather than a filename nothing can open.
const MaxExtLen = 16

// validExt reports whether ext is usable as a file extension.
func validExt(ext string) error {
	if len(ext) > MaxExtLen {
		return fmt.Errorf("file extension %q: longer than %d characters: %w", ext, MaxExtLen, domain.ErrInvalid)
	}
	for _, r := range ext {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		default:
			return fmt.Errorf("file extension %q: %q is not a letter or a digit: %w", ext, r, domain.ErrInvalid)
		}
	}
	return nil
}

// ValidEntryName reports whether name is one Put could have written.
//
// It is the whole of the path safety for /preview/{name}: a checksum, and
// optionally an extension. Nothing else is a preview, so nothing else is
// opened -- there is no separator to traverse with and no "." or ".." that
// could survive the checksum test.
func ValidEntryName(name string) error {
	checksum, ext, hasExt := strings.Cut(name, ".")
	if err := domain.ValidateChecksum(checksum); err != nil {
		return fmt.Errorf("preview name %q: %w", name, err)
	}
	if hasExt {
		if err := validExt(ext); err != nil {
			return fmt.Errorf("preview name %q: %w", name, err)
		}
	}
	return nil
}
