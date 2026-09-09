-- Copyright (c) 2026 Michael D Henderson.
--
-- 0008: the job queue (DESIGN.md 9, PLAN.md M6).
--
-- One table, claimed by a compare-and-swap, with a lease that expires on its
-- own. That last clause is the whole point. The system we learned from marked
-- a row "executing" and had no lease and no heartbeat, so a worker that died
-- mid-job left the row marked executing forever and recovery was a manual
-- UPDATE by whoever noticed. Here nobody has to notice: an expired lease is
-- not a lease, exactly as an expired document lock is not a lock (0004).
--
-- Two departures from the table as DESIGN.md 9 wrote it, both deliberate and
-- both recorded there as well.
--
--   * uid. DESIGN.md 12 gives the retry route as /api/v1/jobs/{id}/retry, and
--     invariant 10 says the API speaks uid only: internal integer primary keys
--     never appear in a URL, a JSON body, or user-facing output. An invariant
--     outranks a path spelled in an example, so jobs carry a uid like every
--     other externally addressable row, and the route is /jobs/{uid}/retry.
--
--   * the claimable index leads on priority, not on scheduled_for. See below.
CREATE TABLE jobs (
    id               INTEGER PRIMARY KEY,
    uid              TEXT    NOT NULL UNIQUE,        -- external identifier

    kind             TEXT    NOT NULL,               -- which handler runs it
    priority         INTEGER NOT NULL DEFAULT 3 CHECK (priority BETWEEN 1 AND 5),
    scheduled_for    TEXT    NOT NULL,               -- not claimable before this
    payload          TEXT    NOT NULL,               -- JSON: the handler's argument

    -- The lease. lease_owner names a worker rather than a user: a worker is
    -- not a row in "users" and never will be, so this is text and not a
    -- foreign key.
    lease_owner      TEXT,
    lease_expires_at TEXT,

    -- attempts is incremented by the claim itself, in the same statement, so
    -- a worker that dies between claiming and recording anything has still
    -- spent an attempt. Counting attempts anywhere else would let a job that
    -- kills its worker be retried forever.
    attempts         INTEGER NOT NULL DEFAULT 0,
    max_attempts     INTEGER NOT NULL DEFAULT 5,
    last_error       TEXT,

    -- The two terminal columns. A job is finished when one of them is set,
    -- and the claimable index is partial on both being NULL.
    completed_at     TEXT,
    failed_at        TEXT,

    created_by       INTEGER          REFERENCES users(id),  -- NULL = system
    created_at       TEXT    NOT NULL
) STRICT;

-- The index the claim seeks on.
--
-- DESIGN.md 9 wrote this as (scheduled_for, priority), which does not serve
-- the query it wrote three lines below it:
--
--   WHERE  completed_at IS NULL AND failed_at IS NULL
--     AND  scheduled_for <= :now
--     AND  (lease_expires_at IS NULL OR lease_expires_at < :now)
--   ORDER BY priority ASC, scheduled_for ASC, id ASC
--   LIMIT 1
--
-- scheduled_for carries a range constraint, and an index cannot both satisfy
-- a range on its leading column and order by a later one: SQLite would seek
-- the range, read every row in it, and sort. Sorting the whole ready backlog
-- to take one row off it is the cost that shows up as a slow queue on the day
-- the queue is long, which is the only day it matters.
--
-- Led by priority the same index answers the query without a sort at all:
-- SQLite walks priority 1 in scheduled_for order, then priority 2, and the
-- first row that also satisfies the two time filters is the answer. That is
-- exactly the ORDER BY, so the plan is a search with no temp b-tree, and
-- LIMIT 1 stops it. TestClaimQueryUsesTheIndex asserts both halves against
-- the statement store.ClaimJob actually runs.
--
-- Priority is load-bearing rather than decoration (DESIGN.md 9): it is what
-- lets a fifty-thousand document republish run at priority 5 without blocking
-- an editor pressing Publish. An index that made the queue sort before
-- honouring it would be an index that quietly taxed the thing priority exists
-- to protect.
--
-- The trade this makes, stated rather than discovered later: with nothing
-- constraining the leading column, a claim against a queue whose pending jobs
-- are ALL scheduled for the future walks the whole partial index before
-- concluding that nothing is ready, where an index led by scheduled_for would
-- have seeked an empty range and stopped. That is the cheaper half of the
-- trade. A queue with work ready -- the case that matters, and the case a
-- bulk republish creates -- stops this index at the first row and costs the
-- other index a sort of every ready row, once per claim.
CREATE INDEX jobs_claimable ON jobs (priority, scheduled_for, id)
    WHERE completed_at IS NULL AND failed_at IS NULL;

-- The leases currently held. It is what "cmsdb check" counts to report leases
-- held past expiry (PLAN.md M6), and it is small by construction: only a job
-- some worker is holding right now has a row in it.
CREATE INDEX jobs_leased ON jobs (lease_expires_at)
    WHERE lease_expires_at IS NOT NULL AND completed_at IS NULL AND failed_at IS NULL;

-- The failures, newest first, which is "earl job list --failed". Partial on
-- the column it orders by, so it holds only the jobs somebody has to look at.
CREATE INDEX jobs_failed ON jobs (failed_at)
    WHERE failed_at IS NOT NULL;
