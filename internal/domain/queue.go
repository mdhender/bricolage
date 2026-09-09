// Copyright (c) 2026 Michael D Henderson.

package domain

import (
	"fmt"
	"strings"
	"time"
)

// Assignment, due dates, and the queue filter (DESIGN.md 5.1, PLAN.md M5).
//
// A queue is a question about the document table -- what is in review, what is
// mine, what is nobody's, what is late -- and this file is the vocabulary for
// asking it. The question is a value: internal/store turns one into SQL,
// internal/service resolves the names in it, and internal/config names a few
// of them so that a person can ask by slug.
//
// The system we learned from could not express "in review and unassigned" at
// all, because "unassigned" was not a state it could name. That is the query
// PLAN.md M5 acceptance 1 makes a named test.

// DocumentFilter is a queue query: which documents, and how many.
//
// Every field is a constraint and the zero value constrains nothing, which is
// what makes the unfiltered list and a queue the same code path rather than
// two that drift. A filter is resolved -- AssignedTo is an internal user id,
// not a uid -- because resolving a name is I/O and this package does none.
type DocumentFilter struct {
	// State is the workflow state slug, or "" for any. It is not validated
	// against a workflow here: a filter naming a state no process declares is
	// a query that matches nothing, which is a truthful answer to a question
	// nobody should have asked, and refusing it would mean loading every
	// workflow in the system to answer a list.
	State string

	// SiteID constrains the list to one site, or 0 for any.
	SiteID int64

	// AssignedTo is whose work to list, or 0 for anybody's.
	AssignedTo int64

	// Unassigned lists the documents nobody has been given. It is a separate
	// field rather than AssignedTo == 0 because "anybody's" and "nobody's" are
	// different questions and a sentinel that meant both would answer neither.
	Unassigned bool

	// Overdue lists the documents whose due date has passed. The instant it
	// has passed by is supplied by the caller from the injected clock
	// (invariant 3, PLAN.md M5 acceptance 5); this struct records only that
	// the question was asked.
	Overdue bool

	// Limit is how many rows to return. Zero means DefaultListLimit.
	Limit int
}

// DefaultListLimit is how many documents a list returns when nobody says.
const DefaultListLimit = 100

// MaxListLimit bounds what a caller may ask for. A queue is something a person
// reads, and a request for a million rows is a request to hold a million rows
// in memory on the way to somebody who will read thirty.
const MaxListLimit = 1000

// Validate reports whether the filter is one the system will accept.
func (f DocumentFilter) Validate() error {
	if f.Unassigned && f.AssignedTo != 0 {
		return fmt.Errorf("a document is assigned to somebody or to nobody, not both: %w", ErrInvalid)
	}
	if f.Limit < 0 {
		return fmt.Errorf("limit %d: not a count: %w", f.Limit, ErrInvalid)
	}
	if f.Limit > MaxListLimit {
		return fmt.Errorf("limit %d: at most %d: %w", f.Limit, MaxListLimit, ErrInvalid)
	}
	return nil
}

// Normalize returns the filter with its defaults filled in. It is pure: the
// caller writes the result.
func (f DocumentFilter) Normalize() DocumentFilter {
	f.State = strings.TrimSpace(f.State)
	if f.Limit <= 0 {
		f.Limit = DefaultListLimit
	}
	return f
}

// IsQueue reports whether the filter constrains anything at all.
//
// It is what tells a plain list from a queue query, which matters in exactly
// one place: an unconstrained list is a bounded read of the newest rows and
// has no index to seek on, while every constrained one must
// (PLAN.md M5 acceptance 4).
func (f DocumentFilter) IsQueue() bool {
	return f.State != "" || f.SiteID != 0 || f.AssignedTo != 0 || f.Unassigned || f.Overdue
}

// Describe renders the filter as the sentence a CLI prints above an empty
// list, so that "no documents" says which question was asked.
func (f DocumentFilter) Describe() string {
	var parts []string
	if f.State != "" {
		parts = append(parts, "in "+f.State)
	}
	if f.Unassigned {
		parts = append(parts, "unassigned")
	}
	if f.AssignedTo != 0 {
		parts = append(parts, "assigned")
	}
	if f.Overdue {
		parts = append(parts, "overdue")
	}
	if f.SiteID != 0 {
		parts = append(parts, fmt.Sprintf("on site %d", f.SiteID))
	}
	if len(parts) == 0 {
		return "documents"
	}
	return "documents " + strings.Join(parts, ", ")
}

// dueFormats are the spellings ParseDue accepts for an instant, in the order
// it tries them.
//
// A due date is typed by a person, and a person types "2026-03-01". Accepting
// the date alone and reading it as midnight UTC is the difference between a
// command that works and one that needs a timestamp nobody can produce from
// memory.
var dueFormats = []string{
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02T15:04",
	"2006-01-02 15:04:05",
	"2006-01-02 15:04",
	"2006-01-02",
}

// ParseDue reads a due date as an instant.
//
// It accepts a duration -- "48h", "3h30m" -- relative to now, because "due in
// two days" is what an editor means and computing the date by hand is how a
// deadline lands on the wrong day. It accepts the RFC 3339 form a machine
// produces and the date form a person types, both read as UTC when they carry
// no zone.
//
// now is a parameter because time.Now belongs to main and internal/clock
// (invariant 3), and because a relative deadline nobody can evaluate at an
// arbitrary instant is a deadline nobody tests.
func ParseDue(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, fmt.Errorf("due date: empty: %w", ErrInvalid)
	}

	// A leading sign or digit followed by a unit is a duration. time.ParseDuration
	// accepts "48" as invalid and "48h" as valid, so trying it first costs one
	// failed parse and never claims a date: no date format begins with a unit
	// suffix, and "2026-03-01" fails ParseDuration on the "-" between digits.
	if d, err := time.ParseDuration(s); err == nil {
		if d < 0 {
			return time.Time{}, fmt.Errorf("due in %s: a deadline in the past: %w", s, ErrInvalid)
		}
		return now.Add(d).UTC(), nil
	}

	for _, layout := range dueFormats {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf(
		"due date %q: want a date (2026-03-01), a timestamp (2026-03-01T17:00:00Z), or a duration (48h): %w",
		s, ErrInvalid)
}

// IsOverdue reports whether a document's due date has passed as of now.
//
// A document with no due date is never overdue: the zero time is the absence
// of a deadline rather than a deadline in 1 CE. The instant is passed in for
// the same reason ParseDue takes one.
func (d Document) IsOverdue(now time.Time) bool {
	return !d.DueAt.IsZero() && !now.Before(d.DueAt)
}

// IsAssigned reports whether the work has been given to somebody.
func (d Document) IsAssigned() bool { return d.AssignedTo != 0 }
