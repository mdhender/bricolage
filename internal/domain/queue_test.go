// Copyright (c) 2026 Michael D Henderson.

package domain

import (
	"errors"
	"testing"
	"time"
)

// domain is pure, so it is tested exhaustively and table-driven
// (AGENTS.md, "Testing").

var noon = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func TestParseDue(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want time.Time
		bad  bool
	}{
		{name: "a date is midnight UTC", in: "2026-03-01", want: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)},
		{name: "RFC 3339", in: "2026-03-01T17:00:00Z", want: time.Date(2026, 3, 1, 17, 0, 0, 0, time.UTC)},
		{
			name: "RFC 3339 with an offset is normalised to UTC",
			in:   "2026-03-01T17:00:00+02:00",
			want: time.Date(2026, 3, 1, 15, 0, 0, 0, time.UTC),
		},
		{name: "a bare timestamp", in: "2026-03-01T17:00", want: time.Date(2026, 3, 1, 17, 0, 0, 0, time.UTC)},
		{name: "a space instead of a T", in: "2026-03-01 17:00", want: time.Date(2026, 3, 1, 17, 0, 0, 0, time.UTC)},
		{name: "a duration is relative to now", in: "48h", want: noon.Add(48 * time.Hour)},
		{name: "a compound duration", in: "3h30m", want: noon.Add(3*time.Hour + 30*time.Minute)},
		{name: "surrounding space", in: "  2026-03-01  ", want: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)},

		{name: "empty", in: "", bad: true},
		{name: "a negative duration is a deadline in the past", in: "-4h", bad: true},
		{name: "a number with no unit", in: "48", bad: true},
		{name: "prose", in: "next tuesday", bad: true},
		{name: "a date that does not exist", in: "2026-02-31", bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseDue(tc.in, noon)
			if tc.bad {
				if err == nil {
					t.Fatalf("ParseDue(%q) = %v, want an error", tc.in, got)
				}
				if !errors.Is(err, ErrInvalid) {
					t.Errorf("ParseDue(%q) returned %v, want it to answer to ErrInvalid", tc.in, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseDue(%q): %v", tc.in, err)
			}
			if !got.Equal(tc.want) {
				t.Errorf("ParseDue(%q) = %v, want %v", tc.in, got, tc.want)
			}
			if got.Location() != time.UTC {
				t.Errorf("ParseDue(%q) is in %v, want UTC", tc.in, got.Location())
			}
		})
	}
}

// TestIsOverdueUsesTheInstantItIsGiven is PLAN.md M5 acceptance 5 at the level
// where it is decidable: the answer is a function of the instant passed in and
// of nothing else, so a fake clock moves it.
func TestIsOverdue(t *testing.T) {
	due := noon
	for _, tc := range []struct {
		name string
		doc  Document
		now  time.Time
		want bool
	}{
		{name: "no due date is never overdue", doc: Document{}, now: noon.Add(time.Hour)},
		{name: "before", doc: Document{DueAt: due}, now: noon.Add(-time.Second)},
		{name: "exactly at the deadline is overdue", doc: Document{DueAt: due}, now: noon, want: true},
		{name: "after", doc: Document{DueAt: due}, now: noon.Add(time.Second), want: true},
		{
			name: "the zero time is the absence of a deadline, not the year 1",
			doc:  Document{DueAt: time.Time{}},
			now:  noon,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.doc.IsOverdue(tc.now); got != tc.want {
				t.Errorf("IsOverdue(%v) = %v, want %v", tc.now, got, tc.want)
			}
		})
	}
}

func TestDocumentFilterValidate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		filter DocumentFilter
		bad    bool
	}{
		{name: "the zero filter constrains nothing", filter: DocumentFilter{}},
		{name: "a state", filter: DocumentFilter{State: "review"}},
		{name: "unassigned", filter: DocumentFilter{Unassigned: true}},
		{name: "an assignee", filter: DocumentFilter{AssignedTo: 7}},
		{
			name:   "assigned to somebody and to nobody at once",
			filter: DocumentFilter{AssignedTo: 7, Unassigned: true},
			bad:    true,
		},
		{name: "a negative limit", filter: DocumentFilter{Limit: -1}, bad: true},
		{name: "a limit past the maximum", filter: DocumentFilter{Limit: MaxListLimit + 1}, bad: true},
		{name: "the maximum limit", filter: DocumentFilter{Limit: MaxListLimit}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.filter.Validate()
			if tc.bad {
				if err == nil {
					t.Fatal("Validate() = nil, want an error")
				}
				if !errors.Is(err, ErrInvalid) {
					t.Errorf("Validate() returned %v, want it to answer to ErrInvalid", err)
				}
				return
			}
			if err != nil {
				t.Errorf("Validate(): %v", err)
			}
		})
	}
}

func TestDocumentFilterNormalize(t *testing.T) {
	got := DocumentFilter{State: "  review  "}.Normalize()
	if got.State != "review" {
		t.Errorf("State = %q, want the trimmed slug", got.State)
	}
	if got.Limit != DefaultListLimit {
		t.Errorf("Limit = %d, want the default %d", got.Limit, DefaultListLimit)
	}
	if got := (DocumentFilter{Limit: 5}).Normalize(); got.Limit != 5 {
		t.Errorf("Limit = %d, want the 5 that was asked for", got.Limit)
	}

	// Normalize is pure: the value it was called on is unchanged.
	before := DocumentFilter{State: " review "}
	_ = before.Normalize()
	if before.State != " review " {
		t.Errorf("Normalize mutated its receiver: State = %q", before.State)
	}
}

func TestDocumentFilterIsQueue(t *testing.T) {
	if (DocumentFilter{Limit: 10}).IsQueue() {
		t.Error("a filter with only a limit is not a queue query")
	}
	for name, f := range map[string]DocumentFilter{
		"state":      {State: "review"},
		"site":       {SiteID: 1},
		"assignee":   {AssignedTo: 2},
		"unassigned": {Unassigned: true},
		"overdue":    {Overdue: true},
	} {
		if !f.IsQueue() {
			t.Errorf("a filter constraining %s is a queue query", name)
		}
	}
}
