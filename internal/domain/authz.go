// Copyright (c) 2026 Michael D Henderson.

package domain

import (
	"fmt"
	"strings"
	"time"
)

// Role is a named set of grants that users are assigned to. Roles are flat: a
// user may hold several, and there is no nesting (DESIGN.md 7.1).
type Role struct {
	ID   int64
	Slug string
	Name string
}

// Scope is the set of things a grant applies to.
//
// Every field is a constraint, and a nil field is a wildcard: the scope with
// no constraints at all is every document in the system. A Scope matches a
// Subject when every constraint it does state is satisfied.
//
// This is what replaces the group-of-users-times-group-of-objects model of the
// system we learned from (DESIGN.md 7.1). Its useful property was that
// permissions followed the document -- file a story under /features and the
// features grants apply, with no access control list written anywhere -- and
// the reason it worked was that the "groups" were really attributes of the
// document: kind, category, workflow, state. Scope keeps the attributes and
// drops the disguise.
//
// Four of these fields have no column in the schema yet. They are resolved
// here in full from the first commit, because a scope dimension that the
// resolver ignores is a hole that opens the moment its column lands; the
// migration that creates each target table adds the column
// (internal/migrate/schema/0003_identity.sql).
type Scope struct {
	// SiteID constrains the grant to one site.
	SiteID *int64

	// DocKind constrains it to 'story', 'media', or 'template'.
	DocKind *string

	// CategoryID constrains it to one category, and CategoryPath is that
	// category's materialised path, always '/'-terminated. The path is
	// carried alongside the id because subtree matching is a prefix test on
	// it and the resolver performs no I/O: whoever loads the grant joins the
	// path in.
	CategoryID   *int64
	CategoryPath *string

	// CategoryDeep extends a category constraint to the whole subtree below
	// it. It defaults to true in the schema, which is the behaviour the
	// system we learned from could not express at all: there is no ancestor
	// walk anywhere in its authorization path, so a grant on /features did
	// not cover /features/film.
	CategoryDeep bool

	// WorkflowID and State constrain the grant to a workflow, or to one state
	// within one.
	WorkflowID *int64
	State      *string

	// CollectionID constrains it to a collection.
	CollectionID *int64

	// DocumentID constrains it to a single document. One row, where the
	// system we learned from wanted a one-member user group and a one-member
	// object group.
	DocumentID *int64
}

// Subject is the thing a privilege is being resolved against: the attributes
// of one document, or of a document about to be created.
//
// It carries paths and identifier sets rather than a document, because the
// resolver is pure and because "may this person create a story in /features"
// is asked before any document exists.
type Subject struct {
	SiteID       int64
	DocKind      string
	CategoryPath string
	WorkflowID   int64
	State        string
	CollectionID int64
	DocumentID   int64
}

// Grant is one row of the grants table: a privilege, held by a role, over a
// scope.
type Grant struct {
	ID        int64
	RoleID    int64
	Privilege Privilege
	Scope     Scope
	CreatedAt time.Time

	// CreatedBy is the user who wrote the grant, or 0 for the system. "cmsdb
	// seed" writes the first grants before there is anybody to have written
	// them.
	CreatedBy int64
}

// Validate reports whether a grant is one the system will store.
//
// The privilege must be on the scale, and a category constraint must carry
// its path: a subtree match with no path to prefix-test is a constraint that
// silently matches nothing, which is the worst of the three possible answers.
func (g Grant) Validate() error {
	if !g.Privilege.Valid() {
		return fmt.Errorf("grant privilege %d: not on the scale: %w", uint8(g.Privilege), ErrInvalid)
	}
	if g.RoleID == 0 {
		return fmt.Errorf("grant: no role: %w", ErrInvalid)
	}
	return g.Scope.Validate()
}

// Validate reports whether a scope is well formed.
func (s Scope) Validate() error {
	if s.CategoryID != nil && (s.CategoryPath == nil || !strings.HasSuffix(*s.CategoryPath, "/")) {
		return fmt.Errorf("scope: a category constraint needs the category's path, '/'-terminated: %w", ErrInvalid)
	}
	if s.CategoryPath != nil && s.CategoryID == nil {
		return fmt.Errorf("scope: a category path without a category: %w", ErrInvalid)
	}
	if s.DocKind != nil && *s.DocKind == "" {
		return fmt.Errorf("scope: an empty document kind is not a wildcard; omit it: %w", ErrInvalid)
	}
	if s.State != nil && *s.State == "" {
		return fmt.Errorf("scope: an empty state is not a wildcard; omit it: %w", ErrInvalid)
	}
	return nil
}

// IsGlobal reports whether the scope constrains nothing, and therefore covers
// every document in the system.
func (s Scope) IsGlobal() bool {
	return s.SiteID == nil && s.DocKind == nil && s.CategoryID == nil &&
		s.WorkflowID == nil && s.State == nil && s.CollectionID == nil &&
		s.DocumentID == nil
}

// String renders the scope the way the CLI and the audit log show it: the
// constraints it states, or "everything" when it states none.
func (s Scope) String() string {
	if s.IsGlobal() {
		return "everything"
	}
	var parts []string
	if s.SiteID != nil {
		parts = append(parts, fmt.Sprintf("site=%d", *s.SiteID))
	}
	if s.DocKind != nil {
		parts = append(parts, "kind="+*s.DocKind)
	}
	if s.CategoryPath != nil {
		suffix := ""
		if s.CategoryDeep {
			suffix = "**"
		}
		parts = append(parts, "category="+*s.CategoryPath+suffix)
	}
	if s.WorkflowID != nil {
		parts = append(parts, fmt.Sprintf("workflow=%d", *s.WorkflowID))
	}
	if s.State != nil {
		parts = append(parts, "state="+*s.State)
	}
	if s.CollectionID != nil {
		parts = append(parts, fmt.Sprintf("collection=%d", *s.CollectionID))
	}
	if s.DocumentID != nil {
		parts = append(parts, fmt.Sprintf("document=%d", *s.DocumentID))
	}
	return strings.Join(parts, " ")
}

// Ref returns a pointer to v.
//
// A Scope constraint is a pointer because NULL is the wildcard, and Go has no
// literal syntax for the address of one. It lives here, beside the only type
// in this repository that is built that way, rather than in a utility package
// that would then attract everything else.
func Ref[T any](v T) *T { return &v }
