// Copyright (c) 2026 Michael D Henderson.

package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// The related-asset cascade's vocabulary (DESIGN.md 8.2, PLAN.md M10).
//
// Publishing a document publishes the documents it references. Two things
// live here and nothing else does: how a version's content says what it
// references, and how a document that will not be published says why.
//
// The traversal itself is in internal/publish, as a pure function over a
// loaded graph, because it needs the authorization resolver and this package
// imports nothing local (invariant 1). What it produces is a Refusal, and a
// Refusal is here for the reason Guard is here: the transport edge has to
// render one, the CLI has to print one, and a type each of them defined for
// itself would be three vocabularies for one refusal.
//
// Every bullet in DESIGN.md 8.2 is a lesson somebody learned in production.
// The one that decides the shape of this file is "refused by name": a cascade
// that reports "3 related documents could not be published" tells an editor
// nothing they can act on, and the system we learned from reported exactly
// that.

// References returns the uids of the documents content names, in the order the
// schema declares the fields and the content lists the values, with duplicates
// removed.
//
// The order is deterministic and that is load-bearing rather than tidy. The
// gathered set and the refusals are compared between a dry run and a real
// publish (PLAN.md M10 acceptance 6), and two runs over one map's iteration
// order would disagree about the order of a list nobody could then compare.
//
// An empty value is not a reference. A repeatable document field with a blank
// entry in it is somebody's half-filled form, and refusing the publish over it
// would be refusing a document that names nothing.
func References(et *ElementType, content string) ([]string, error) {
	if et == nil {
		return nil, fmt.Errorf("references: no element type to read the schema of: %w", ErrInvalid)
	}
	schema, err := ParseElementSchema(et.Schema)
	if err != nil {
		return nil, err
	}

	// Nothing to look for. The common case, and worth answering before
	// parsing the content: most element types declare no document field at
	// all, and a graph loader that unmarshalled every version's content to
	// discover that would be doing it once per node.
	var fields []FieldDef
	for _, f := range schema.Fields {
		if f.Type == FieldDocument {
			fields = append(fields, f)
		}
	}
	if len(fields) == 0 {
		return nil, nil
	}

	var tree map[string]json.RawMessage
	if strings.TrimSpace(content) != "" {
		if err := json.Unmarshal([]byte(content), &tree); err != nil {
			return nil, fmt.Errorf("references: content is not a JSON object: %v: %w", err, ErrInvalid)
		}
	}

	var out []string
	seen := map[string]bool{}
	add := func(uid string) {
		uid = strings.TrimSpace(uid)
		if uid == "" || seen[uid] {
			return
		}
		seen[uid] = true
		out = append(out, uid)
	}

	for _, f := range fields {
		raw, present := tree[f.Name]
		if !present {
			continue
		}
		if f.Repeatable {
			var values []string
			if err := json.Unmarshal(raw, &values); err != nil {
				return nil, fmt.Errorf("references: field %q is repeatable, so it must be an array of uids: %w",
					f.Name, ErrInvalid)
			}
			for _, v := range values {
				add(v)
			}
			continue
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, fmt.Errorf("references: field %q must be a uid written as a string: %w",
				f.Name, ErrInvalid)
		}
		add(value)
	}
	return out, nil
}

// RefusalReason is why one document in a cascade will not be published.
//
// It is a closed vocabulary for the reason the guards are (DESIGN.md 6.1): a
// client switching on a reason it does not recognise is a client that cannot
// tell "you may not publish this" from "somebody is editing it", and those
// two ask different things of the person reading them.
type RefusalReason string

const (
	// RefusedMissing is a uid in a document's content that names no
	// document. A related story deleted after the referring version was
	// checked in is the ordinary way this happens.
	RefusedMissing RefusalReason = "missing"

	// RefusedPermission is DESIGN.md 8.2's "permission-checked per node":
	// Publish is required on each related document and not only on the root.
	RefusedPermission RefusalReason = "permission"

	// RefusedState is "state-gated": a related document whose workflow does
	// not call its current state publishable.
	RefusedState RefusalReason = "state"

	// RefusedCheckedOut is "lock-gated": somebody holds the edit lease on
	// the related document.
	RefusedCheckedOut RefusalReason = "checked_out"

	// RefusedNoVersion is a related document with nothing to pin. A publish
	// names a version and never a document (invariant 8), so a document
	// whose only version is its first open draft cannot be published at all.
	RefusedNoVersion RefusalReason = "no_version"
)

// RefusalReasons are the reasons, in a stable order for the CLI and the tests.
var RefusalReasons = []RefusalReason{
	RefusedMissing, RefusedPermission, RefusedState, RefusedCheckedOut, RefusedNoVersion,
}

// Refusal is one document a cascade will not publish, and why.
//
// It names the document by uid because that is what a person can act on
// (invariant 10) and because PLAN.md M10 acceptance 2, 4, and 5 all ask for
// the refusal to name it. Title is carried beside the uid when the graph knows
// one: a uid is what a client addresses and a title is what an editor
// recognises, and a report carrying only the first sends somebody looking one
// document at a time.
//
// Referrer is the uid of the document whose content named this one. A chain of
// five documents that refuses at the fourth is a report nobody can read
// without it.
type Refusal struct {
	UID      string
	Title    string
	Referrer string

	Reason RefusalReason

	// Detail is the sentence a person reads. It names the state, the
	// privilege, or the lock holder, depending on the reason.
	Detail string
}

// String renders a refusal for a log line or a CLI table.
func (r Refusal) String() string {
	name := r.UID
	if r.Title != "" {
		name = fmt.Sprintf("%s (%s)", r.Title, r.UID)
	}
	if r.Referrer != "" {
		return fmt.Sprintf("%s, referenced by %s: %s", name, r.Referrer, r.Detail)
	}
	return fmt.Sprintf("%s: %s", name, r.Detail)
}

// RelatedError is a cascade refused under publish.related_failure = fail
// (PLAN.md M10 acceptance 2).
//
// It carries every refusal rather than the first, for the reason ContentError
// carries every field error: the person fixing this wants the list and not a
// sequence of round trips, and a cascade of five documents that refuses at two
// of them should say so once.
//
// It answers to ErrConflict, so the transport edge maps it to 409 through the
// one mapping function it has (DESIGN.md 14). A conflict rather than a
// forbidden, even when the reason is a privilege: the statement being made is
// about the set of documents this publish would have to touch, not about
// whether the caller may publish the one they asked for -- they may, which is
// why the request got this far.
type RelatedError struct {
	// UID is the root: the document somebody asked to publish.
	UID string

	Refusals []Refusal
}

func (e *RelatedError) Error() string {
	parts := make([]string, 0, len(e.Refusals))
	for _, r := range e.Refusals {
		parts = append(parts, r.String())
	}
	return fmt.Sprintf(
		"publishing %s would publish documents it references and %d of them cannot be published: %s",
		e.UID, len(e.Refusals), strings.Join(parts, "; "))
}

// Is makes a refused cascade answer to ErrConflict.
func (e *RelatedError) Is(target error) bool { return target == ErrConflict }

// RefusalsOf returns the refusals carried by err, and whether err was a
// refused cascade at all.
//
// It is how the transport edge fills in the problem document's "refusals"
// member without knowing anything else about the error, and it is the same
// shape as GuardOf and FieldErrorsOf (DESIGN.md 12).
func RefusalsOf(err error) ([]Refusal, bool) {
	var re *RelatedError
	if errors.As(err, &re) {
		return re.Refusals, true
	}
	return nil, false
}
