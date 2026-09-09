-- Copyright (c) 2026 Michael D Henderson.
--
-- 0007: the indexes the queue queries seek on (DESIGN.md 5.1, PLAN.md M5).
--
-- M5 adds no columns. assigned_to and due_at have been on "documents" since
-- 0004 and were rebuilt into it by 0005; what M5 adds is the ability to ask
-- questions about them, and PLAN.md M5 acceptance 4 says every one of those
-- questions must be answered by an index rather than by reading the table.
--
-- The three indexes 0005 left behind cannot answer them:
--
--   documents_queue   (workflow_id, state, due_at)  -- leads on the workflow
--   documents_mine    (assigned_to) WHERE assigned_to IS NOT NULL
--   documents_overdue (due_at)      WHERE due_at IS NOT NULL
--
-- documents_queue leads on workflow_id, so "everything in review" -- which
-- names no workflow -- cannot seek on it. documents_overdue is right as it
-- stands and is kept. documents_mine is the one that has to go, and the
-- reason is the query this milestone exists for.

-- "Unassigned" is the query the system we learned from could not express, and
-- a partial index over the rows that DO have an assignee is precisely the
-- index that cannot answer it: SQLite may only use a partial index when the
-- query's WHERE clause implies the index's, and "assigned_to IS NULL" implies
-- the opposite of "assigned_to IS NOT NULL".
--
-- 0004's comment argued that most rows are NULL in this column and that an
-- index over them would be an index over nothing anybody asks about. M5 is
-- the milestone that makes them exactly what everybody asks about -- an
-- editor's first question is "what is nobody working on" -- so the partial
-- index is replaced by a full one. SQLite treats IS NULL as an equality
-- constraint, so one index serves "assigned to this person" and "assigned to
-- nobody" alike, and the trailing columns let it serve both combined with a
-- state and ordered by a due date.
DROP INDEX documents_mine;
CREATE INDEX documents_assignee ON documents(assigned_to, state, due_at);

-- "Everything in review", with or without a due date bound. This is
-- documents_queue without the leading workflow_id: a queue is usually named
-- by its state alone, because most installations run one process per kind.
CREATE INDEX documents_state ON documents(state, due_at);

-- "Everything on this site", and the combined query a site-scoped editor
-- actually asks, which constrains all three.
CREATE INDEX documents_site ON documents(site_id, state, due_at);
