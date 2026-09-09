// Copyright (c) 2026 Michael D Henderson.

package config

import "testing"

// The saved queue definitions are configuration rather than schema
// (PLAN.md M5), so what these tests assert is that the built-in set is
// well formed and that a set nobody wrote by hand cannot be.

// TestDefaultQueuesValidate is what stops DefaultQueues from panicking in
// production: it panics on a set that does not validate, and this is where
// that is found out.
func TestDefaultQueuesValidate(t *testing.T) {
	set := DefaultQueues()
	if set.IsEmpty() {
		t.Fatal("the built-in set is empty; every server would serve no queues")
	}
	for _, q := range set.List() {
		if err := q.Validate(); err != nil {
			t.Errorf("queue %q: %v", q.Slug, err)
		}
		if got, ok := set.Lookup(q.Slug); !ok || got.Slug != q.Slug {
			t.Errorf("Lookup(%q) did not find the queue List returned", q.Slug)
		}
	}

	// PLAN.md M5 acceptance 1's query gets a name, so that the thing the
	// system we learned from could not express is one word here.
	q, ok := set.Lookup("needs-editor")
	if !ok {
		t.Fatal(`there is no "needs-editor" queue`)
	}
	if q.State != "review" || q.Assignee != QueueAssigneeNobody {
		t.Errorf("needs-editor asks %+v, want review and unassigned", q)
	}
}

// TestQueueSetRefusesADuplicate keeps the file loader honest before it exists:
// two definitions with one slug is a menu whose second entry can never be
// reached.
func TestQueueSetRefusesADuplicate(t *testing.T) {
	_, err := NewQueueSet([]Queue{
		{Slug: "mine", Name: "Mine"},
		{Slug: "mine", Name: "Also mine"},
	})
	if err == nil {
		t.Fatal("NewQueueSet accepted two queues with one slug")
	}
}

func TestQueueValidate(t *testing.T) {
	for _, tc := range []struct {
		name  string
		queue Queue
		bad   bool
	}{
		{name: "the least a queue needs", queue: Queue{Slug: "all", Name: "Everything"}},
		{name: "no slug", queue: Queue{Name: "Everything"}, bad: true},
		{name: "no name", queue: Queue{Slug: "all"}, bad: true},
		{
			name:  "an assignee word this binary does not know",
			queue: Queue{Slug: "all", Name: "Everything", Assignee: "somebody"},
			bad:   true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.queue.Validate()
			if tc.bad && err == nil {
				t.Error("Validate() = nil, want an error")
			}
			if !tc.bad && err != nil {
				t.Errorf("Validate(): %v", err)
			}
		})
	}
}

// TestParseQueueAssigneeRefusesAnUnknownWord is invariant 6 in miniature: a
// configured constraint this binary does not understand must refuse, never
// widen to "anybody". A queue silently widened to everything shows an editor
// somebody else's work.
func TestParseQueueAssigneeRefusesAnUnknownWord(t *testing.T) {
	if _, err := ParseQueueAssignee("everyone"); err == nil {
		t.Error(`ParseQueueAssignee("everyone") = nil, want a refusal rather than "anybody"`)
	}
	for _, in := range []string{"", "me", "nobody"} {
		if _, err := ParseQueueAssignee(in); err != nil {
			t.Errorf("ParseQueueAssignee(%q): %v", in, err)
		}
	}
}

// TestQueueSetListIsACopy: a caller that sorted the menu would reorder every
// later caller's.
func TestQueueSetListIsACopy(t *testing.T) {
	set := DefaultQueues()
	first := set.List()[0].Slug
	got := set.List()
	got[0].Slug = "mutated"
	if set.List()[0].Slug != first {
		t.Error("List returns the set's own slice; a caller can rewrite the menu")
	}
}
