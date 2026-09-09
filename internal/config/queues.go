// Copyright (c) 2026 Michael D Henderson.

package config

import (
	"fmt"
	"sort"
	"strings"
)

// Saved queue definitions (PLAN.md M5, "Saved queue definitions in config (not
// schema)").
//
// A queue is a named question about the document table -- "what is in review
// and nobody's", "what is mine", "what is late". Naming one is configuration
// and not data: a queue has no identity anybody refers to, nothing points at
// it, and an installation that wants a different set wants a different config
// file rather than a migration and an admin screen. The system we learned from
// made every such list a row somewhere, and the result was that changing what
// an editor saw meant a database write nobody could review.
//
// These types are deliberately spelled in this package's own vocabulary rather
// than in internal/domain's: config imports the standard library and nothing
// else (doc.go), so it names a state as a string and an assignee as one of
// three words. internal/service turns a Queue into a domain.DocumentFilter,
// which is where the words become an internal user id.

// QueueAssignee is a queue's constraint on who the work is assigned to.
//
// There are three answers and not two. "Anybody" is the absence of a
// constraint; "the person asking" and "nobody" are constraints, and they are
// different questions -- one is a person's own list, the other is the pile
// nobody has picked up. A single nullable user id could express neither.
type QueueAssignee string

const (
	// QueueAssigneeAny does not constrain the assignee.
	QueueAssigneeAny QueueAssignee = ""

	// QueueAssigneeActor is whoever is asking. It is resolved per request,
	// which is what makes one saved definition serve every editor.
	QueueAssigneeActor QueueAssignee = "me"

	// QueueAssigneeNobody is the unassigned pile.
	QueueAssigneeNobody QueueAssignee = "nobody"
)

// ParseQueueAssignee maps a configured word to a constraint. An unrecognised
// word is refused rather than read as "anybody", because a queue silently
// widened to everything is a queue that shows an editor somebody else's work.
func ParseQueueAssignee(s string) (QueueAssignee, error) {
	switch QueueAssignee(strings.TrimSpace(s)) {
	case QueueAssigneeAny:
		return QueueAssigneeAny, nil
	case QueueAssigneeActor:
		return QueueAssigneeActor, nil
	case QueueAssigneeNobody:
		return QueueAssigneeNobody, nil
	default:
		return QueueAssigneeAny, fmt.Errorf("assignee %q: want \"\", \"me\", or \"nobody\"", s)
	}
}

// Queue is one saved definition.
type Queue struct {
	// Slug is what GET /api/v1/queues/{slug} and "earl queue SLUG" name.
	Slug string

	// Name is the heading a person reads.
	Name string

	// Description says what question the queue asks, so that a UI can explain
	// an empty list.
	Description string

	// State is the workflow state slug, or "" for any.
	State string

	// Assignee constrains who the work is on.
	Assignee QueueAssignee

	// Overdue restricts the queue to documents whose due date has passed.
	Overdue bool
}

// Validate reports whether a definition is usable. It is exported because the
// file loader that arrives with a config file will need it; the built-in set
// below is validated by a test.
func (q Queue) Validate() error {
	if strings.TrimSpace(q.Slug) == "" {
		return fmt.Errorf("queue: no slug")
	}
	if strings.TrimSpace(q.Name) == "" {
		return fmt.Errorf("queue %q: no name", q.Slug)
	}
	if _, err := ParseQueueAssignee(string(q.Assignee)); err != nil {
		return fmt.Errorf("queue %q: %w", q.Slug, err)
	}
	return nil
}

// QueueSet is the queues one server serves, in the order it lists them.
type QueueSet struct {
	queues []Queue
	bySlug map[string]Queue
}

// NewQueueSet builds a set, refusing a duplicate slug and an unusable
// definition. The order given is the order List returns.
func NewQueueSet(queues []Queue) (QueueSet, error) {
	set := QueueSet{
		queues: make([]Queue, 0, len(queues)),
		bySlug: make(map[string]Queue, len(queues)),
	}
	for _, q := range queues {
		if err := q.Validate(); err != nil {
			return QueueSet{}, err
		}
		if _, dup := set.bySlug[q.Slug]; dup {
			return QueueSet{}, fmt.Errorf("queue %q: defined twice", q.Slug)
		}
		set.queues = append(set.queues, q)
		set.bySlug[q.Slug] = q
	}
	return set, nil
}

// List returns every queue in the set, in order. The slice is a copy: a caller
// that sorted it would reorder every later caller's menu.
func (s QueueSet) List() []Queue {
	out := make([]Queue, len(s.queues))
	copy(out, s.queues)
	return out
}

// Lookup finds one queue by slug.
func (s QueueSet) Lookup(slug string) (Queue, bool) {
	q, ok := s.bySlug[slug]
	return q, ok
}

// Slugs returns every slug, sorted, for an error message that says what is
// available.
func (s QueueSet) Slugs() []string {
	out := make([]string, 0, len(s.queues))
	for _, q := range s.queues {
		out = append(out, q.Slug)
	}
	sort.Strings(out)
	return out
}

// IsEmpty reports whether the set names no queues.
func (s QueueSet) IsEmpty() bool { return len(s.queues) == 0 }

// DefaultQueues is the built-in set, and it is what every server serves today:
// nothing reads a config file yet, so this list is the configuration rather
// than a fallback for one.
//
// It is short on purpose. Each of these is a question an editor asks out loud
// on an ordinary day, and a queue nobody opens is a menu entry everybody
// scrolls past. The state slugs are the default story workflow's
// (DESIGN.md 5.4); a queue naming a state no workflow declares is not an
// error, it is a list that comes back empty, which is the truthful answer.
func DefaultQueues() QueueSet {
	set, err := NewQueueSet([]Queue{
		{
			Slug:        "mine",
			Name:        "My work",
			Description: "Documents assigned to you, in any state.",
			Assignee:    QueueAssigneeActor,
		},
		{
			Slug:        "unassigned",
			Name:        "Unassigned",
			Description: "Documents nobody has been given, in any state.",
			Assignee:    QueueAssigneeNobody,
		},
		{
			Slug:        "review",
			Name:        "In review",
			Description: "Everything waiting on an editor.",
			State:       "review",
		},
		{
			Slug:        "needs-editor",
			Name:        "Needs an editor",
			Description: "In review and assigned to nobody. This is the pile that stalls.",
			State:       "review",
			Assignee:    QueueAssigneeNobody,
		},
		{
			Slug:        "ready",
			Name:        "Ready to publish",
			Description: "Approved and waiting to go out.",
			State:       "approved",
		},
		{
			Slug:        "overdue",
			Name:        "Overdue",
			Description: "Past its due date, whoever has it.",
			Overdue:     true,
		},
	})
	if err != nil {
		// Unreachable: the list above is a constant. It panics rather than
		// returning an error because a built-in set that does not validate is
		// a programming mistake, not a configuration one, and every caller
		// would have to handle an error that cannot happen.
		panic("config: the built-in queues do not validate: " + err.Error())
	}
	return set
}
