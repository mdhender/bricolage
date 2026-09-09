// Copyright (c) 2026 Michael D Henderson.

package domain

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// The document kinds (DESIGN.md 5.1). A document's kind is fixed at creation:
// it decides which element types apply and it is a grant scope dimension, so
// changing it would silently change who may touch the row.
const (
	KindStory    = "story"
	KindMedia    = "media"
	KindTemplate = "template"
)

// DocKinds are the kinds a document may have, in a stable order for the CLI
// and the admin screens.
var DocKinds = []string{KindStory, KindMedia, KindTemplate}

// SubjectDocument is the events subject kind for a document. A version is not
// a subject of its own: a version is part of a document's history, and a
// history that is split across two subject kinds is a history nobody can read
// in one query.
const SubjectDocument = "document"

// ValidDocKind reports whether s is one of the three kinds.
func ValidDocKind(s string) bool {
	for _, k := range DocKinds {
		if k == s {
			return true
		}
	}
	return false
}

// ElementType declares the fields a document of one type carries
// (DESIGN.md 5.2).
//
// Eighteen entity-attribute-value tables in the system we learned from become
// one JSON column here. Schema declares the fields -- name, type, repeatable,
// required, allowed children -- and DESIGN.md 5.2 validates content against it
// on check-in. PLAN.md M3 stores the schema and does not yet validate; what is
// enforced today is that both this and a version's content are JSON objects,
// so the validator that arrives has something it can parse.
type ElementType struct {
	ID      int64
	UID     string
	KeyName string
	Name    string

	// Kind is which document kind this type applies to.
	Kind string

	TopLevel  bool
	FixedURI  bool
	Paginated bool

	// Schema is the JSON field definition document.
	Schema string

	CreatedAt time.Time
}

// Lock is the edit lease on a document (DESIGN.md 5.1).
//
// It lives on the document row, not on a version. In the system we learned
// from the flag lived on the version, so every non-current version reported
// itself as not checked out even while the document was locked, and the code
// had to consult a different column to compensate. One lock, one row, no
// lying.
//
// It is a lease rather than a flag: an expired lock is not a lock, which is
// what stops an editor who closed their laptop from blocking a document until
// an administrator intervenes.
type Lock struct {
	// UserID is who holds it, or 0 when the document is not checked out.
	UserID int64

	// ExpiresAt is when the lease runs out. It is extended by activity.
	ExpiresAt time.Time
}

// Held reports whether the document is checked out as of now.
//
// The instant is passed in rather than read, because time.Now belongs to main
// and internal/clock (invariant 3) and because a lease nobody can evaluate at
// an arbitrary instant is a lease nobody tests.
func (l Lock) Held(now time.Time) bool {
	return l.UserID != 0 && now.Before(l.ExpiresAt)
}

// IsHeldBy reports whether userID holds a live lease.
//
// A user with an expired lease does not hold the lock: this is the one
// question every editing operation asks, and answering it with "was it ever
// theirs" instead of "is it theirs now" is how a stale client overwrites
// somebody else's work.
func (l Lock) IsHeldBy(userID int64, now time.Time) bool {
	return userID != 0 && l.UserID == userID && now.Before(l.ExpiresAt)
}

// Document is a stable identity: what a thing is, who has it, and which
// version is current (DESIGN.md 5.1).
//
// The content is not here. A document has no title and no body; its versions
// do, and asking the document for them would be asking which version's.
type Document struct {
	ID  int64
	UID string

	SiteID        int64
	Kind          string
	ElementTypeID int64

	// ElementTypeKey is the element type's external identifier, joined in by
	// the store so that the API can speak it without a second query
	// (invariant 10).
	ElementTypeKey string

	// ElementTypeFixedURI is the element type's fixed_uri flag, joined in for
	// the same reason: BuildURI needs to know which of the output channel's
	// two formats applies, and a pure function cannot go and look
	// (DESIGN.md 5.3).
	ElementTypeFixedURI bool

	// WorkflowID is the editorial process this document is in, and State is
	// where it has got to. The pair is a composite foreign key to
	// workflow_states, so a document can only ever name a state its own
	// workflow declares (DESIGN.md 5.1).
	//
	// internal/workflow is the only writer of State (invariant 4). Nothing
	// else assigns to it -- not a service method, not a migration fix-up --
	// and internal/store exposes no way to set it that is not a whole
	// transition.
	WorkflowID int64
	State      string

	// AssignedTo is who the work is on, or 0. Due is when it is due, or the
	// zero time. Both are written by M5; they are read here because the
	// columns exist and a struct that omits half a row is a struct somebody
	// overwrites with zeroes.
	AssignedTo int64
	DueAt      time.Time

	Lock Lock

	// CategoryID is the primary category the document is filed in, or 0 when
	// it is filed nowhere, and CategoryPath is that category's materialised
	// path (PLAN.md M7). The path is joined in by the store because the
	// authorization resolver is pure and matches a category-scoped grant by
	// prefix on it (DESIGN.md 7.1), and because BuildURI expands
	// %{categories} from it.
	CategoryID   int64
	CategoryPath string

	// CurrentVersionID is the newest version row, checked in or not.
	// LiveVersionID is the version that is published, set by M8.
	CurrentVersionID int64
	LiveVersionID    int64

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Version is an immutable snapshot of content (DESIGN.md 5.1).
//
// "Immutable" is enforced by a trigger on the table, not by convention here: a
// scheduled publish pins a version id and can trust it (invariant 8).
type Version struct {
	ID         int64
	DocumentID int64

	// Number is the 1-based, monotonic version number within the document.
	// There is no version 0.
	Number int

	Title     string
	Slug      string
	CoverDate string

	// Content is the JSON element tree. It is a string rather than a
	// json.RawMessage because it is stored and returned verbatim, and because
	// a []byte in a domain type is a []byte somebody mutates.
	Content string

	// Note is the check-in message, which is a different thing from a
	// comment thread (DESIGN.md 5.5).
	Note string

	CreatedBy int64
	CreatedAt time.Time

	// CheckedInAt is the zero time while this is the open working draft.
	CheckedInAt time.Time
}

// IsDraft reports whether this is the open working draft. "Never checked in"
// is the absence of a timestamp, which is why there is no version 0 and no
// status column.
func (v Version) IsDraft() bool { return v.CheckedInAt.IsZero() }

// Subject renders the document as the thing a privilege is resolved against
// (DESIGN.md 7.2).
//
// The workflow and the state arrived with M4, and with them the grants.state
// column that had been resolvable since M2 finally resolved against something
// real. M7 completes it: the category path is the last dimension the resolver
// carried without a column behind it, and a category_deep grant on /features
// now covers /features/film because this is where the path it prefix-matches
// comes from. A document filed nowhere has an empty path, and a grant that
// names a category does not match it -- which is the right answer rather than
// a permissive one.
func (d Document) Subject() Subject {
	return Subject{
		SiteID:       d.SiteID,
		DocKind:      d.Kind,
		CategoryPath: d.CategoryPath,
		WorkflowID:   d.WorkflowID,
		State:        d.State,
		DocumentID:   d.ID,
	}
}

// NewDocument is a document somebody is asking to create: everything that
// decides what it is, plus the first draft's content.
type NewDocument struct {
	SiteID         int64
	Kind           string
	ElementTypeKey string

	Title     string
	Slug      string
	CoverDate string
	Content   string
}

// Validate reports whether the request is one the system will accept.
//
// The content check is a shape check and not a schema check: PLAN.md M3 stores
// element_types.schema and does not yet validate against it. What is enforced
// today is that the column holds a JSON object, so that everything downstream
// -- the API, the diff, the renderer -- can parse it without asking whether it
// is JSON at all.
func (n NewDocument) Validate() error {
	if n.SiteID == 0 {
		return fmt.Errorf("document: no site: %w", ErrInvalid)
	}
	if !ValidDocKind(n.Kind) {
		return fmt.Errorf("document kind %q: not one of story, media, template: %w", n.Kind, ErrInvalid)
	}
	if n.ElementTypeKey == "" {
		return fmt.Errorf("document: no element type: %w", ErrInvalid)
	}
	if strings.TrimSpace(n.Title) == "" {
		return fmt.Errorf("document: a title is required: %w", ErrInvalid)
	}
	return ValidateContentShape(n.Content)
}

// DraftUpdate is a change to the open working draft. Every field is a pointer
// because "not mentioned" and "set to empty" are different requests, and the
// difference between them is the difference between leaving a title alone and
// clearing it.
type DraftUpdate struct {
	Title     *string
	Slug      *string
	CoverDate *string
	Content   *string
}

// IsEmpty reports whether the update asks for nothing.
func (u DraftUpdate) IsEmpty() bool {
	return u.Title == nil && u.Slug == nil && u.CoverDate == nil && u.Content == nil
}

// Apply returns v with the update applied. It is pure: the caller writes the
// result, and the version it was given is unchanged.
func (u DraftUpdate) Apply(v Version) (Version, error) {
	if u.Title != nil {
		if strings.TrimSpace(*u.Title) == "" {
			return v, fmt.Errorf("document: a title is required: %w", ErrInvalid)
		}
		v.Title = *u.Title
	}
	if u.Slug != nil {
		v.Slug = *u.Slug
	}
	if u.CoverDate != nil {
		v.CoverDate = *u.CoverDate
	}
	if u.Content != nil {
		if err := ValidateContentShape(*u.Content); err != nil {
			return v, err
		}
		v.Content = *u.Content
	}
	return v, nil
}

// ValidateContentShape reports whether s is storable in
// document_versions.content: a JSON object, or empty, which the schema's
// default renders as "{}".
//
// It is the shape check, and it is separate from ValidateContent, which is the
// element type's field definitions (DESIGN.md 5.2). The two run at different
// moments and that is the point: a working draft may be invalid against its
// schema and is written anyway, because refusing a half-finished paragraph is
// how a writer loses a sentence. What this refuses is content no validator
// could even parse.
func ValidateContentShape(s string) error {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var into map[string]any
	if err := json.Unmarshal([]byte(s), &into); err != nil {
		return fmt.Errorf("content: not a JSON object: %v: %w", err, ErrInvalid)
	}
	return nil
}

// NormalizeContent renders content for storage: the empty string becomes the
// schema's default rather than a NOT NULL column holding "".
func NormalizeContent(s string) string {
	if strings.TrimSpace(s) == "" {
		return "{}"
	}
	return s
}
