-- Copyright (c) 2026 Michael D Henderson.
--
-- 0012: alert rules, notifications, and the cursor that drives them
-- (DESIGN.md 10, PLAN.md M12).
--
-- Alerts are a rule engine over the event stream, and the whole of the engine
-- is here: a rule names an event type and a list of conditions, and a
-- notification is what a match produced. There is deliberately no scripting
-- language. The system we learned from evaluated alert rules inside a "Safe"
-- sandbox, which is a security surface for something a switch statement
-- handles, and every operator this schema can carry is one internal/domain
-- enforces (invariant 6).
--
-- Two departures from the tables as DESIGN.md 10 wrote them, both deliberate.
--
--   * uid, on both tables. DESIGN.md 12 gives the read route as
--     POST /api/v1/notifications/{id}/read, and invariant 10 says the API
--     speaks uid only: internal integer primary keys never appear in a URL, a
--     JSON body, or user-facing output. An invariant outranks a path spelled
--     in an example, which is the same correction migration 0008 made for
--     jobs, so both tables carry a uid and the route is
--     /notifications/{uid}/read.
--
--   * alert_cursor. DESIGN.md 6.4 says alert evaluation happens after commit,
--     driven off the event row, so that a failing notification cannot roll
--     back an editorial action. "Driven off the event row" needs somewhere to
--     record which rows have been driven off, and the alternative -- calling
--     the dispatcher from each of the thirty-odd places that write an event --
--     is thirty places to forget one.
CREATE TABLE alert_rules (
    id         INTEGER PRIMARY KEY,
    uid        TEXT    NOT NULL UNIQUE,          -- external identifier

    name       TEXT    NOT NULL,                 -- what a person calls it
    event_type TEXT    NOT NULL,                 -- one of internal/events' constants

    -- JSON: [{"field": "to", "op": "eq", "value": "legal"}]
    --
    -- All of them must pass. A rule is an AND of its conditions, and there is
    -- no OR: two rules are how you say "or", and they cost one row each.
    --
    -- A condition whose op is "matches" carries a Go regexp, and the rule is
    -- refused at save time if it does not compile (PLAN.md M12 acceptance 3).
    -- That check lives in internal/domain because it is a pure function over
    -- the value, and it is what makes "the pattern is broken" a 422 the person
    -- writing the rule reads rather than a log line at three in the morning
    -- naming an event nobody was watching.
    conditions TEXT    NOT NULL DEFAULT '[]',

    -- Where it goes and to whom. The channel is "in_app" or "email"; the
    -- target is "user:<uid>", "role:<slug>", or -- on the email channel only
    -- -- "email:<address>". Both are validated by internal/domain before they
    -- are written: a channel this binary does not deliver and a target it
    -- cannot resolve are the same mistake as a guard name nothing enforces.
    channel    TEXT    NOT NULL,
    target     TEXT    NOT NULL,

    -- A rule that is off is kept rather than deleted. Turning an alert off
    -- for a week is what an operator does during a bulk import, and deleting
    -- it means retyping the conditions afterwards.
    active     INTEGER NOT NULL DEFAULT 1 CHECK (active IN (0, 1)),

    created_by INTEGER          REFERENCES users(id),  -- NULL = system
    created_at TEXT    NOT NULL,
    updated_at TEXT    NOT NULL
) STRICT;

-- "Which rules care about this event type", which is the only question the
-- dispatcher asks of this table. Partial on active, because a rule that is off
-- is never an answer to it.
CREATE INDEX alert_rules_event ON alert_rules (event_type) WHERE active = 1;

CREATE TABLE notifications (
    id         INTEGER PRIMARY KEY,
    uid        TEXT    NOT NULL UNIQUE,

    user_id    INTEGER NOT NULL REFERENCES users(id)  ON DELETE CASCADE,
    event_id   INTEGER NOT NULL REFERENCES events(id) ON DELETE CASCADE,

    -- ON DELETE SET NULL, from DESIGN.md 10, and it is the interesting one:
    -- deleting a rule must not delete what it already told somebody. The
    -- notification survives with no rule behind it, which is exactly what
    -- happened.
    rule_id    INTEGER          REFERENCES alert_rules(id) ON DELETE SET NULL,

    read_at    TEXT,
    created_at TEXT    NOT NULL
) STRICT;

-- One notification per person per event per rule.
--
-- The dispatcher advances alert_cursor in the same transaction as the rows it
-- writes, so a second delivery of the same event cannot happen through it;
-- this is what says so to a second process, and to a future dispatcher that
-- replays. NULL rule_ids compare distinct in SQLite, which is right here: a
-- row whose rule has been deleted is history and constrains nothing.
CREATE UNIQUE INDEX notifications_once ON notifications (user_id, event_id, rule_id);

-- The inbox, newest first, and the unread count beside it. Two indexes for two
-- queries, which is 0007's rule: an index arrives with the query that seeks on
-- it. The partial one holds only what somebody still has to look at.
CREATE INDEX notifications_inbox  ON notifications (user_id, id DESC);
CREATE INDEX notifications_unread ON notifications (user_id, id DESC) WHERE read_at IS NULL;

-- How far the dispatcher has read.
--
-- One row, forever, held to it by the CHECK. The batch that evaluates events
-- writes its notifications and moves this in one transaction, so the pair is
-- atomic: every event is evaluated exactly once, and a crash between the two
-- is not a state this schema can hold. The UPDATE names the value it read, so
-- a second dispatcher racing it loses the compare-and-swap rather than
-- delivering the same batch again.
CREATE TABLE alert_cursor (
    id            INTEGER PRIMARY KEY CHECK (id = 1),
    last_event_id INTEGER NOT NULL,
    updated_at    TEXT    NOT NULL
) STRICT;

-- Seeded at the newest event that exists, not at zero.
--
-- A database that has been running for a month has a month of events in it and
-- nobody wants an inbox of them the first time an alert rule is written. The
-- dispatcher starts from now; the history stays a query, which is what the
-- events table was always for.
INSERT INTO alert_cursor (id, last_event_id, updated_at)
VALUES (1, (SELECT COALESCE(MAX(id), 0) FROM events), strftime('%Y-%m-%dT%H:%M:%f', 'now') || 'Z');
