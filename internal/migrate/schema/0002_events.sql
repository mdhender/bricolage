-- Copyright (c) 2026 Michael D Henderson.
--
-- 0002: events, the audit spine (DESIGN.md 10).
--
-- One table with a JSON payload replaces the four tables and the seeded
-- registry of 153 rows the system we learned from used. Event types are Go
-- constants with display names in a registry; nothing SELECTs them.
--
-- It is created here, in the first milestone that has a schema at all, because
-- DESIGN.md 10 says to wire it from the first milestone that changes state and
-- retrofitting an audit log is miserable. Nothing writes to it yet: the first
-- writer is "log-me-in" in M2 (session.dev_login).
--
-- actor_id is NULL for the system, which is why it is nullable rather than a
-- sentinel row.
CREATE TABLE events (
    id           INTEGER PRIMARY KEY,
    type         TEXT    NOT NULL,
    actor_id     INTEGER          REFERENCES users(id),
    subject_kind TEXT    NOT NULL,
    subject_id   INTEGER NOT NULL,
    payload      TEXT    NOT NULL DEFAULT '{}',
    occurred_at  TEXT    NOT NULL
) STRICT;

-- A document's history is a query, not a log grep.
CREATE INDEX events_subject ON events (subject_kind, subject_id, id DESC);
CREATE INDEX events_type ON events (type, occurred_at);
