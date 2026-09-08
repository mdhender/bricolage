// Copyright (c) 2026 Michael D Henderson.

package domain

import (
	"encoding/json"
	"fmt"
	"time"
)

// Event is one row of the audit spine (DESIGN.md 10).
//
// Every state-changing operation writes one, and the payload carries enough to
// reconstruct what happened (invariant 7). A subject's history is then a query
// rather than a log grep, which is the whole reason this is a table and not a
// logger call.
type Event struct {
	ID   int64
	Type string

	// ActorID is the user who caused it, or 0 for the system. The column is
	// nullable rather than pointing at a sentinel row.
	ActorID int64

	// SubjectKind and SubjectID name the thing the event is about. The kind is
	// a short noun -- "user", "session", "document" -- and the id is that
	// table's integer key, because this table is internal and never
	// serialised as-is (invariant 10).
	SubjectKind string
	SubjectID   int64

	// Payload is JSON. It defaults to "{}" in the schema, and PayloadJSON
	// enforces the same here so nothing writes an empty string into a column
	// that other code will hand to a JSON parser.
	Payload map[string]any

	OccurredAt time.Time
}

// The subject kinds this milestone writes. They are constants because a typo
// in a subject kind produces a history query that silently returns nothing.
const (
	SubjectUser    = "user"
	SubjectSession = "session"
)

// PayloadJSON renders the payload for storage. A nil or empty payload is "{}",
// never "" and never "null".
func (e Event) PayloadJSON() (string, error) {
	if len(e.Payload) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(e.Payload)
	if err != nil {
		return "", fmt.Errorf("event %s: encoding the payload: %w", e.Type, err)
	}
	return string(b), nil
}

// Validate reports whether the event is one the store will accept. An event
// with no type or no subject is not an audit record; it is a row.
func (e Event) Validate() error {
	if e.Type == "" {
		return fmt.Errorf("event: no type: %w", ErrInvalid)
	}
	if e.SubjectKind == "" {
		return fmt.Errorf("event %s: no subject kind: %w", e.Type, ErrInvalid)
	}
	if e.OccurredAt.IsZero() {
		return fmt.Errorf("event %s: no timestamp: %w", e.Type, ErrInvalid)
	}
	return nil
}
