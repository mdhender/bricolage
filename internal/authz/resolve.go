// Copyright (c) 2026 Michael D Henderson.

package authz

import (
	"strings"

	"github.com/mdhender/bricolage/internal/domain"
)

// Resolve returns the effective privilege the grants confer over subj
// (DESIGN.md 7.2).
//
// It is MAX over the matching grants, and deny wins because Deny is the
// numeric maximum of the scale. That is the entire algorithm: no second pass,
// no special case, no ordering to get wrong. The equivalent SQL is the
// definition of the rule; this is the hot path, and a user's grants are loaded
// once per request and evaluated here.
//
// It is pure. No I/O, no clock, no store.
func Resolve(grants []domain.Grant, subj domain.Subject) domain.Privilege {
	effective := domain.NoPrivilege
	for _, g := range grants {
		if !Matches(g.Scope, subj) {
			continue
		}
		if g.Privilege > effective {
			effective = g.Privilege
		}
	}
	return effective
}

// Allows reports whether the grants permit an operation needing want over
// subj. It is Resolve followed by the one comparison that knows about Deny.
func Allows(grants []domain.Grant, subj domain.Subject, want domain.Privilege) bool {
	return Resolve(grants, subj).Allows(want)
}

// Matches reports whether a scope applies to a subject.
//
// A nil field is a wildcard, so a scope matches when every constraint it
// states is satisfied and the scope that states none matches everything. The
// only dimension with any subtlety is the category, and the subtlety is the
// point: with CategoryDeep the grant covers the whole subtree below the
// category, by prefix on the materialised path.
func Matches(s domain.Scope, subj domain.Subject) bool {
	if s.SiteID != nil && *s.SiteID != subj.SiteID {
		return false
	}
	if s.DocKind != nil && *s.DocKind != subj.DocKind {
		return false
	}
	if s.WorkflowID != nil && *s.WorkflowID != subj.WorkflowID {
		return false
	}
	if s.State != nil && *s.State != subj.State {
		return false
	}
	if s.CollectionID != nil && *s.CollectionID != subj.CollectionID {
		return false
	}
	if s.DocumentID != nil && *s.DocumentID != subj.DocumentID {
		return false
	}
	if s.CategoryPath != nil && !categoryMatches(*s.CategoryPath, s.CategoryDeep, subj.CategoryPath) {
		return false
	}
	return true
}

// categoryMatches is the subtree rule. Paths are materialised and always end
// in '/', with the root being "/", so "is a descendant of" is a string prefix
// test and "/features/" does not match "/features-and-analysis/".
//
// Without CategoryDeep it is equality, which is what the system we learned
// from did everywhere because it had no ancestor walk at all: a grant on
// /features did not cover /features/film, and the documented workaround was to
// write the grant again for every child.
func categoryMatches(scopePath string, deep bool, subjectPath string) bool {
	if subjectPath == "" {
		return false
	}
	if scopePath == subjectPath {
		return true
	}
	return deep && strings.HasPrefix(subjectPath, scopePath)
}

// Covers reports whether every subject that inner could match is also matched
// by outer.
//
// This is scope containment rather than scope matching, and it is what the
// anti-escalation check needs: "do I hold this privilege over the whole of the
// scope I am about to grant" is a question about two scopes, not about one
// document. A wildcard covers any constraint on that dimension; an equal
// constraint covers itself; a deep category covers its own subtree.
func Covers(outer, inner domain.Scope) bool {
	if !coversInt(outer.SiteID, inner.SiteID) {
		return false
	}
	if !coversString(outer.DocKind, inner.DocKind) {
		return false
	}
	if !coversInt(outer.WorkflowID, inner.WorkflowID) {
		return false
	}
	if !coversString(outer.State, inner.State) {
		return false
	}
	if !coversInt(outer.CollectionID, inner.CollectionID) {
		return false
	}
	if !coversInt(outer.DocumentID, inner.DocumentID) {
		return false
	}
	if outer.CategoryPath != nil {
		if inner.CategoryPath == nil {
			// The inner scope is unconstrained by category, so it reaches
			// categories outside the outer one.
			return false
		}
		if !categoryMatches(*outer.CategoryPath, outer.CategoryDeep, *inner.CategoryPath) {
			return false
		}
		if inner.CategoryDeep && !outer.CategoryDeep {
			// The inner scope reaches below its category and the outer one
			// does not follow it there.
			return false
		}
	}
	return true
}

// Intersects reports whether any subject could match both scopes.
//
// Two constraints on the same dimension conflict only when they are both
// stated and differ; a wildcard never conflicts. Categories intersect when
// either path is inside the other's reach.
func Intersects(a, b domain.Scope) bool {
	if conflictsInt(a.SiteID, b.SiteID) {
		return false
	}
	if conflictsString(a.DocKind, b.DocKind) {
		return false
	}
	if conflictsInt(a.WorkflowID, b.WorkflowID) {
		return false
	}
	if conflictsString(a.State, b.State) {
		return false
	}
	if conflictsInt(a.CollectionID, b.CollectionID) {
		return false
	}
	if conflictsInt(a.DocumentID, b.DocumentID) {
		return false
	}
	if a.CategoryPath != nil && b.CategoryPath != nil {
		if !categoryMatches(*a.CategoryPath, a.CategoryDeep, *b.CategoryPath) &&
			!categoryMatches(*b.CategoryPath, b.CategoryDeep, *a.CategoryPath) {
			return false
		}
	}
	return true
}

func coversInt(outer, inner *int64) bool {
	if outer == nil {
		return true
	}
	return inner != nil && *outer == *inner
}

func coversString(outer, inner *string) bool {
	if outer == nil {
		return true
	}
	return inner != nil && *outer == *inner
}

func conflictsInt(a, b *int64) bool     { return a != nil && b != nil && *a != *b }
func conflictsString(a, b *string) bool { return a != nil && b != nil && *a != *b }
