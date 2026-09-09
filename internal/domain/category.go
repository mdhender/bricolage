// Copyright (c) 2026 Michael D Henderson.

package domain

import (
	"fmt"
	"strings"
)

// Categories and the arithmetic on their materialised paths (DESIGN.md 5.3).
//
// Everything here is pure and everything here is about one invariant:
// categories.path always begins and ends with '/', and the root is '/'. Two
// things depend on it -- URI construction and permission scope matching by
// subtree prefix -- and both are string operations, so a path that broke the
// rule would not fail loudly. It would build a URI with a doubled slash, or
// match a grant against a sibling whose name happens to start the same way.
//
// The rule is enforced here, in one place, and asserted by a property test.

// RootPath is the path of a site's root category. Every site has exactly one,
// created in the same transaction as the site itself.
const RootPath = "/"

// SubjectCategory is the events subject kind for a category.
const SubjectCategory = "category"

// MaxDirectoryLen bounds one path segment. It is generous for a directory
// name and small enough that a path built from a deep tree stays inside what a
// web server will accept.
const MaxDirectoryLen = 64

// Category is one node of a site's hierarchy (DESIGN.md 5.3).
type Category struct {
	ID     int64
	UID    string
	SiteID int64

	// ParentID is 0 for the root of a site and for no other row.
	ParentID int64

	// Directory is one path segment. It is empty for the root and only for
	// the root, which is what makes
	//
	//	path = parent.path + directory + "/"
	//
	// hold for every row without a special case.
	Directory string

	// Path is materialised and always '/'-terminated.
	Path string

	Name string
}

// IsRoot reports whether this is a site's root category.
func (c Category) IsRoot() bool { return c.ParentID == 0 }

// Depth is how many segments the path carries. The root is 0.
func (c Category) Depth() int { return PathDepth(c.Path) }

// PathDepth counts the segments in a '/'-terminated path. The root is 0.
func PathDepth(path string) int {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return 0
	}
	return strings.Count(trimmed, "/") + 1
}

// ValidatePath reports whether p is a well-formed materialised category path.
//
// This is the property the whole design rests on, written down once: it begins
// with '/', it ends with '/', and it contains no empty segment -- which is the
// same thing as saying it contains no "//".
func ValidatePath(p string) error {
	switch {
	case p == "":
		return fmt.Errorf("category path: empty: %w", ErrInvalid)
	case !strings.HasPrefix(p, "/"):
		return fmt.Errorf("category path %q: must begin with %q: %w", p, "/", ErrInvalid)
	case !strings.HasSuffix(p, "/"):
		return fmt.Errorf("category path %q: must end with %q: %w", p, "/", ErrInvalid)
	case strings.Contains(p, "//") && p != RootPath:
		return fmt.Errorf("category path %q: has an empty segment: %w", p, ErrInvalid)
	}
	return nil
}

// ValidateDirectory reports whether s is usable as one path segment.
//
// The empty directory is refused here even though the root carries one: the
// root is created by the same transaction that creates its site and never
// through this function, and accepting "" anywhere else is how a second row
// ends up claiming the root's path.
func ValidateDirectory(s string) error {
	switch {
	case s == "":
		return fmt.Errorf("directory: empty; only a site's root category has no directory: %w", ErrInvalid)
	case len(s) > MaxDirectoryLen:
		return fmt.Errorf("directory %q: longer than %d characters: %w", s, MaxDirectoryLen, ErrInvalid)
	case strings.Contains(s, "/"):
		return fmt.Errorf("directory %q: one path segment, so no %q: %w", s, "/", ErrInvalid)
	case s == "." || s == "..":
		return fmt.Errorf("directory %q: not a name: %w", s, ErrInvalid)
	}
	for _, r := range s {
		if !validDirectoryRune(r) {
			return fmt.Errorf("directory %q: %q is not allowed; use letters, digits, %q, %q, %q or %q: %w",
				s, r, "-", "_", ".", "~", ErrInvalid)
		}
	}
	return nil
}

// validDirectoryRune is the unreserved set of RFC 3986 and nothing else.
//
// A directory becomes a URI segment verbatim, so anything needing
// percent-encoding would make the stored path and the served path two
// different strings -- and the stored one is what a grant prefix-matches
// against.
func validDirectoryRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	case r == '-', r == '_', r == '.', r == '~':
		return true
	}
	return false
}

// JoinPath returns the path of the child of parent named directory.
//
// It is the one place the arithmetic is written. parent must be a valid path;
// the result is parent + directory + "/", which begins and ends with '/'
// because parent does and because directory carries no slash of its own.
func JoinPath(parent, directory string) (string, error) {
	if err := ValidatePath(parent); err != nil {
		return "", err
	}
	if err := ValidateDirectory(directory); err != nil {
		return "", err
	}
	return parent + directory + "/", nil
}

// IsDescendantPath reports whether child is at or below parent.
//
// Both are '/'-terminated, so "is inside" is a plain string prefix and
// "/features/" does not match "/features-and-analysis/". That is the whole
// reason the trailing slash is an invariant rather than a convention.
func IsDescendantPath(parent, child string) bool {
	return strings.HasPrefix(child, parent)
}

// NewCategory is a category somebody is asking to create.
type NewCategory struct {
	SiteID int64

	// ParentPath is where it goes. The root is "/".
	ParentPath string

	Directory string
	Name      string
}

// Validate reports whether the request is one the system will accept.
func (n NewCategory) Validate() error {
	if n.SiteID == 0 {
		return fmt.Errorf("category: no site: %w", ErrInvalid)
	}
	if err := ValidatePath(n.ParentPath); err != nil {
		return err
	}
	if err := ValidateDirectory(n.Directory); err != nil {
		return err
	}
	if strings.TrimSpace(n.Name) == "" {
		return fmt.Errorf("category: a name is required: %w", ErrInvalid)
	}
	return nil
}

// CategoryMove is a request to move or rename a category.
//
// Both fields are pointers because "not mentioned" and "set to this" are
// different requests: a rename leaves the parent alone, a move leaves the
// directory alone, and the two together are one operation rather than two.
type CategoryMove struct {
	// ParentPath is the new parent's path, or nil to keep the current one.
	ParentPath *string

	// Directory is the new segment, or nil to keep the current one.
	Directory *string
}

// IsEmpty reports whether the move asks for nothing.
func (m CategoryMove) IsEmpty() bool { return m.ParentPath == nil && m.Directory == nil }

// Resolve computes the path a category would have after the move, and refuses
// the two moves that cannot be made.
//
// A root cannot be moved: it is the anchor every other path is measured from,
// and a site with no '/' has no address arithmetic at all. A category cannot
// be moved into its own subtree: the tree would leave the site's root
// unreachable from it, and the single-statement path rewrite would then read
// rows it had already written.
func (m CategoryMove) Resolve(c Category, newParent Category) (string, error) {
	if c.IsRoot() {
		return "", fmt.Errorf("category %q is a site's root and cannot be moved: %w", c.Path, ErrConflict)
	}
	if m.IsEmpty() {
		return "", fmt.Errorf("move: nothing to change: %w", ErrInvalid)
	}
	directory := c.Directory
	if m.Directory != nil {
		directory = *m.Directory
	}
	if newParent.SiteID != c.SiteID {
		return "", fmt.Errorf("category %q is on site %d and %q is on site %d; a category does not move between sites: %w",
			c.Path, c.SiteID, newParent.Path, newParent.SiteID, ErrInvalid)
	}
	if IsDescendantPath(c.Path, newParent.Path) {
		return "", fmt.Errorf("category %q cannot be moved inside itself (%q): %w",
			c.Path, newParent.Path, ErrConflict)
	}
	return JoinPath(newParent.Path, directory)
}

// RewritePath returns what path becomes when the subtree rooted at oldPrefix
// moves to newPrefix.
//
// It is the Go statement of what the store's single UPDATE does, and it exists
// so that a test can check the two against each other: the SQL rewrites every
// descendant in one statement, and this says what each row should hold
// afterwards.
func RewritePath(path, oldPrefix, newPrefix string) string {
	if !IsDescendantPath(oldPrefix, path) {
		return path
	}
	return newPrefix + path[len(oldPrefix):]
}

// Filing is one place a document sits, and whether it is the primary one.
//
// A document may appear in several categories and exactly one of them is
// primary: the primary is what its URI is built from, and the others are
// places it also appears. The partial unique index on document_categories
// makes "exactly one" a truth of the schema rather than of the code that
// writes it.
type Filing struct {
	Category Category
	Primary  bool
}

// PrimaryOf returns the primary filing, and whether the document has one.
func PrimaryOf(filings []Filing) (Category, bool) {
	for _, f := range filings {
		if f.Primary {
			return f.Category, true
		}
	}
	return Category{}, false
}
