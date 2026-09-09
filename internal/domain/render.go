// Copyright (c) 2026 Michael D Henderson.

package domain

import (
	"errors"
	"fmt"
	"strings"
)

// Rendering: the vocabulary, the failures, and where a media binary lives
// (DESIGN.md 8.4, PLAN.md M8).
//
// internal/render does the work. What is here is what the rest of the system
// has to agree about: the three modes, which the API and earl both speak; the
// failure a broken template is, which the transport edge has to turn into a
// response naming the template and the line; and the path a content-addressed
// blob has, which is arithmetic on a checksum.

// The rendering modes (DESIGN.md 8.4). Three, as in the system we learned
// from, and the difference between the first two is not only what the caller
// does with the bytes: a template can ask which mode it is in, which is how a
// preview draws the banner that stops somebody mistaking it for the live page.
const (
	// ModePublish renders for the output tree. internal/publish writes the
	// result (PLAN.md M9).
	ModePublish = "publish"

	// ModePreview renders to the scratch tree cmsd serves back under
	// /preview/.
	ModePreview = "preview"

	// ModeValidate parses and type-checks, and writes nothing. It is the mode
	// that answers "does this template compile" without a document having to
	// be published to find out.
	ModeValidate = "validate"
)

// RenderModes are the modes, in a stable order for the CLI and the admin
// screens.
var RenderModes = []string{ModePublish, ModePreview, ModeValidate}

// ValidRenderMode reports whether s is one of the three.
func ValidRenderMode(s string) bool {
	for _, m := range RenderModes {
		if m == s {
			return true
		}
	}
	return false
}

// The phases a template can fail in. They are separate because they are
// different problems for the person reading them: a template that will not
// parse is broken for every document, and one that fails while executing is
// broken for this one.
const (
	PhaseParse   = "parse"
	PhaseExecute = "execute"
)

// TemplateError is a template that would not parse or would not run
// (PLAN.md M8 acceptance 2 and 5).
//
// It carries the template's name and the line, because that is what the person
// who wrote the template needs and because an error saying only "executing
// template" sends them reading the whole file. The transport edge lifts both
// onto the problem document as extension members, the way it lifts a guard
// name, so they survive production's rule that a 500 discloses no detail: the
// name of a template on this server is not a secret, and the alternative is an
// editor who can see that preview is broken and cannot see why.
//
// It deliberately answers to no sentinel. A broken template is not the
// caller's malformed request and not a refusal about the document -- it is
// this installation's configuration failing, which is a 500 by the table in
// DESIGN.md 12 and by PLAN.md M8 acceptance 5.
type TemplateError struct {
	// Template is the path of the template within the template tree.
	Template string

	// Line is the line the error was reported at, or 0 when the underlying
	// error did not name one.
	Line int

	// Phase is PhaseParse or PhaseExecute.
	Phase string

	// Err is what html/template said.
	Err error
}

func (e *TemplateError) Error() string {
	var b strings.Builder
	b.WriteString("template ")
	b.WriteString(e.Template)
	if e.Line > 0 {
		fmt.Fprintf(&b, " line %d", e.Line)
	}
	b.WriteString(": ")
	b.WriteString(e.Phase)
	b.WriteString(": ")
	if e.Err != nil {
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

func (e *TemplateError) Unwrap() error { return e.Err }

// TemplateErrorOf returns the template failure carried by err, and whether err
// was one at all.
//
// It is the same shape as GuardOf and FieldErrorsOf, and it is here for the
// same reason: internal/api turns an error into a response in exactly one
// function, and that function needs to ask this question without importing the
// package that produced it.
func TemplateErrorOf(err error) (*TemplateError, bool) {
	var te *TemplateError
	if errors.As(err, &te) {
		return te, true
	}
	return nil, false
}

// BlobDir is the directory content-addressed media binaries live under
// (DESIGN.md 8.4). The database stores no blobs; it stores this path.
const BlobDir = "blobs"

// BlobPath is where the binary with checksum sha lives: blobs/<sha[:2]>/<sha>.
//
// The two-character shard is the whole reason the function exists. A flat
// directory of a hundred thousand files is a directory listing nobody can read
// and a filesystem that slows down looking one up; the shard is the fix every
// content-addressed store uses, and getting it slightly wrong -- three
// characters here, one there -- means two halves of the system disagreeing
// about where a file is.
//
// Nothing writes a blob yet: there is no media ingest route in DESIGN.md 12,
// so the milestone that adds one adds the writing. What M8 owes is the
// addressing rule, written once, so that the writer and the reader cannot
// invent two of them. When something does write one it will need the shard
// directory to exist already, because nothing in this system creates a
// directory (invariant 19).
func BlobPath(sha string) (string, error) {
	if err := ValidateChecksum(sha); err != nil {
		return "", err
	}
	return BlobDir + "/" + sha[:2] + "/" + sha, nil
}

// ChecksumLen is the length of the hex SHA-256 this system addresses content
// by.
const ChecksumLen = 64

// ValidateChecksum reports whether sha is a lowercase hex SHA-256.
//
// Lowercase only, and not "case-insensitively accepted then normalised": a
// checksum is a filename here, and two spellings of one filename is two files
// on a case-sensitive filesystem and one race on a case-insensitive one.
func ValidateChecksum(sha string) error {
	if len(sha) != ChecksumLen {
		return fmt.Errorf("checksum %q: want %d hex characters, got %d: %w",
			sha, ChecksumLen, len(sha), ErrInvalid)
	}
	for _, r := range sha {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
		default:
			return fmt.Errorf("checksum %q: %q is not lowercase hex: %w", sha, r, ErrInvalid)
		}
	}
	return nil
}
