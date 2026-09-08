// Copyright (c) 2026 Michael D Henderson.

package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mdhender/bricolage/internal/domain"
	"zombiezen.com/go/sqlite"
)

// RecordEvent appends one row to the audit spine (DESIGN.md 10, invariant 7).
//
// It is the only writer of the events table. internal/events owns the type
// vocabulary and the payload each type carries; this owns the INSERT, because
// all SQL lives in this package (invariant 2).
func (db *DB) RecordEvent(ctx context.Context, e domain.Event) (int64, error) {
	var id int64
	err := db.Write(ctx, func(conn *sqlite.Conn) error {
		var err error
		id, err = recordEvent(conn, e)
		return err
	})
	return id, err
}

// recordEvent is RecordEvent on a connection the caller already holds, so that
// an operation and its event are one transaction. A state change whose event
// can be rolled back separately is a state change with no audit record
// (invariant 7).
func recordEvent(conn *sqlite.Conn, e domain.Event) (int64, error) {
	if err := e.Validate(); err != nil {
		return 0, err
	}
	payload, err := e.PayloadJSON()
	if err != nil {
		return 0, err
	}
	err = run(conn, "recording event "+e.Type, `
		INSERT INTO events (type, actor_id, subject_kind, subject_id, payload, occurred_at)
		VALUES (:type, :actor_id, :subject_kind, :subject_id, :payload, :occurred_at)`,
		func(stmt *sqlite.Stmt) {
			stmt.SetText(":type", e.Type)
			if e.ActorID == 0 {
				// NULL is the system, which is why the column is nullable
				// rather than pointing at a sentinel row.
				stmt.SetNull(":actor_id")
			} else {
				stmt.SetInt64(":actor_id", e.ActorID)
			}
			stmt.SetText(":subject_kind", e.SubjectKind)
			stmt.SetInt64(":subject_id", e.SubjectID)
			stmt.SetText(":payload", payload)
			stmt.SetText(":occurred_at", formatTime(e.OccurredAt))
		}, nil)
	if err != nil {
		return 0, err
	}
	return conn.LastInsertRowID(), nil
}

// EventsForSubject returns a subject's history, newest first, at most limit
// rows. It is the query the events_subject index exists for.
func (db *DB) EventsForSubject(ctx context.Context, kind string, id int64, limit int) ([]domain.Event, error) {
	if limit <= 0 {
		limit = 100
	}
	var out []domain.Event
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return run(conn, fmt.Sprintf("history of %s %d", kind, id), `
			SELECT id, type, actor_id, subject_kind, subject_id, payload, occurred_at
			  FROM events
			 WHERE subject_kind = :kind AND subject_id = :id
			 ORDER BY id DESC
			 LIMIT :limit`,
			func(stmt *sqlite.Stmt) {
				stmt.SetText(":kind", kind)
				stmt.SetInt64(":id", id)
				stmt.SetInt64(":limit", int64(limit))
			},
			func(stmt *sqlite.Stmt) error {
				e, err := scanEvent(stmt)
				if err != nil {
					return err
				}
				out = append(out, e)
				return nil
			})
	})
	return out, err
}

// EventsOfType returns the most recent events of one type, newest first. It is
// what a test asserts on and what the admin screens list.
func (db *DB) EventsOfType(ctx context.Context, eventType string, limit int) ([]domain.Event, error) {
	if limit <= 0 {
		limit = 100
	}
	var out []domain.Event
	err := db.Read(ctx, func(conn *sqlite.Conn) error {
		return run(conn, "events of type "+eventType, `
			SELECT id, type, actor_id, subject_kind, subject_id, payload, occurred_at
			  FROM events
			 WHERE type = :type
			 ORDER BY id DESC
			 LIMIT :limit`,
			func(stmt *sqlite.Stmt) {
				stmt.SetText(":type", eventType)
				stmt.SetInt64(":limit", int64(limit))
			},
			func(stmt *sqlite.Stmt) error {
				e, err := scanEvent(stmt)
				if err != nil {
					return err
				}
				out = append(out, e)
				return nil
			})
	})
	return out, err
}

func scanEvent(stmt *sqlite.Stmt) (domain.Event, error) {
	id := stmt.GetInt64("id")
	at, err := parseTime(stmt.GetText("occurred_at"))
	if err != nil {
		return domain.Event{}, fmt.Errorf("event %d: occurred_at: %w", id, err)
	}
	var actor int64
	if a := nullInt64(stmt, "actor_id"); a != nil {
		actor = *a
	}
	e := domain.Event{
		ID:          id,
		Type:        stmt.GetText("type"),
		ActorID:     actor,
		SubjectKind: stmt.GetText("subject_kind"),
		SubjectID:   stmt.GetInt64("subject_id"),
		OccurredAt:  at,
	}
	if err := unmarshalPayload(stmt.GetText("payload"), &e.Payload); err != nil {
		return domain.Event{}, fmt.Errorf("event %d: payload: %w", id, err)
	}
	return e, nil
}

func unmarshalPayload(s string, into *map[string]any) error {
	if s == "" || s == "{}" {
		*into = nil
		return nil
	}
	return json.Unmarshal([]byte(s), into)
}
