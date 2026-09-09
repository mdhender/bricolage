// Copyright (c) 2026 Michael D Henderson.

package render

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"

	"github.com/mdhender/bricolage/internal/domain"
)

// Ext is the extension every template file carries.
//
// It names the engine rather than the output: these are html/template sources
// whatever the output channel's file extension says, and a tree of ".html"
// files that are not HTML is a tree somebody's editor lies about.
const Ext = ".gohtml"

// Locator finds the template for an element type in a category.
//
// The cascade is DESIGN.md 8.4's and it is fifteen lines, which is the whole
// reason it is worth keeping: the deepest category that has a template for
// this element type wins, and the site's root is the backstop.
type Locator struct {
	// FS is the template tree.
	FS fs.FS

	// Base is the directory within FS the category tree hangs off, which is
	// the site's. An empty Base searches from the root of FS, which is what a
	// test with one site does.
	Base string
}

// Match is a template that was found, and where it was looked for.
type Match struct {
	// Path is the template's path within the tree.
	Path string

	// Searched is every path that was tried, deepest first, ending with the
	// one that matched. It is carried on success as well as on failure
	// because "which template did this use, and what did it beat" is the
	// question asked when the wrong one is used.
	Searched []string
}

// NotFoundError is "there is no template for this document anywhere"
// (PLAN.md M8 acceptance 1).
//
// It names the element type and every path that was searched, because the two
// mistakes that produce it -- a template in the wrong directory, and a
// template with the wrong name -- are both invisible from a message that says
// only "not found".
type NotFoundError struct {
	KeyName  string
	Searched []string
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("no template for element type %q; searched %s",
		e.KeyName, strings.Join(e.Searched, ", "))
}

// Is makes a missing template answer to ErrNotFound, which the transport edge
// maps to 404 through the one mapping function it has.
func (e *NotFoundError) Is(target error) bool { return target == domain.ErrNotFound }

// Lookup returns the template for keyName in categoryPath, walking up.
func (l Locator) Lookup(categoryPath, keyName string) (Match, error) {
	candidates, err := l.Candidates(categoryPath, keyName)
	if err != nil {
		return Match{}, err
	}
	for i, candidate := range candidates {
		info, err := fs.Stat(l.FS, candidate)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			return Match{}, fmt.Errorf("template %s: %w", candidate, err)
		case info.IsDir():
			// A directory named "story.gohtml" is somebody's mistake and not
			// a template. Walking past it rather than failing is deliberate:
			// the next category up may have a real one, and the alternative
			// is one bad directory breaking every document below it.
			continue
		}
		return Match{Path: candidate, Searched: candidates[:i+1]}, nil
	}
	return Match{}, &NotFoundError{KeyName: keyName, Searched: candidates}
}

// Candidates is every path Lookup would try, deepest first.
//
// It is exported because it is what the "no template anywhere" error carries
// and what a test asserts on, and because a caller that wants to say "put one
// here" needs the list without a failed lookup to get it.
func (l Locator) Candidates(categoryPath, keyName string) ([]string, error) {
	if err := domain.ValidatePath(categoryPath); err != nil {
		return nil, err
	}
	if err := ValidSegment("element type key", keyName); err != nil {
		return nil, err
	}
	if l.Base != "" {
		if err := ValidSegment("template directory", l.Base); err != nil {
			return nil, err
		}
	}

	ancestors := domain.AncestorPaths(categoryPath)
	out := make([]string, 0, len(ancestors))
	for _, ancestor := range ancestors {
		p := path.Join(l.Base, strings.Trim(ancestor, "/"), keyName+Ext)
		if !fs.ValidPath(p) {
			return nil, fmt.Errorf("template path %q is not one this system can read: %w", p, domain.ErrInvalid)
		}
		out = append(out, p)
	}
	return out, nil
}

// ValidSegment refuses a path segment that would leave the template tree.
//
// Both segments this applies to come from the database -- a site's domain and
// an element type's key name -- and neither column constrains its characters.
// A key name of "../../etc" would otherwise be a path traversal with a
// migration in front of it, and the fix is to refuse it here rather than to
// hope every caller remembers.
func ValidSegment(what, s string) error {
	switch {
	case s == "":
		return fmt.Errorf("%s: empty: %w", what, domain.ErrInvalid)
	case s == "." || s == "..":
		return fmt.Errorf("%s %q: not a name: %w", what, s, domain.ErrInvalid)
	case strings.ContainsAny(s, `/\`):
		return fmt.Errorf("%s %q: one path segment, so no separator: %w", what, s, domain.ErrInvalid)
	case strings.ContainsRune(s, 0):
		return fmt.Errorf("%s %q: contains a NUL: %w", what, s, domain.ErrInvalid)
	}
	return nil
}
