-- Copyright (c) 2026 Michael D Henderson.
--
-- 0010: publishing remembers every file it wrote (DESIGN.md 8.3, PLAN.md M9).
--
-- One table and one UPDATE. The table is the memory that makes stale expiry
-- possible; the UPDATE is what makes the default workflow's "Publish"
-- transition actually publish.
--
-- Without published_resources, changing a cover date, a slug, or a category
-- leaves the file at the old URI serving forever, and the only way back is a
-- reconciliation pass over an already-dirty output tree. DESIGN.md 8.3 says to
-- build it with publishing rather than after, and this is that.

-- Every file the publisher has written, and what it was written from.
--
-- version_id is the whole point of the row and it is not derivable: a document
-- may be in draft, being revised, while the version that is actually serving is
-- three versions old (invariant 8). documents.live_version_id records what is
-- live for the document; this records what is live at one address in one
-- channel, which is the granularity expiry works at.
--
-- checksum and bytes are what "cmsdb check" compares the file on disk against,
-- so that "the row says we wrote this and the file is not there" and "there is
-- a file here nothing claims" are both answerable without re-rendering
-- anything (PLAN.md M9 acceptance 7).
--
-- UNIQUE (output_channel_id, uri) is two things at once. It is the index the
-- expiry diff seeks on, and it is URI-collision detection for free: two
-- documents that resolve to one address cannot both be published, and the
-- second one fails with SQLITE_CONSTRAINT_UNIQUE. That is detected by result
-- code and never by matching message text (invariant 11) -- the system we
-- learned from regexed PostgreSQL 7.1 error strings for this, which is why its
-- friendly "URI is not unique" message has not fired on any server built this
-- century.
--
-- ON DELETE CASCADE on document_id and not on the other two is deliberate.
-- Deleting a document should take its resource rows with it; deleting an
-- output channel or a version out from under a published resource is a thing
-- the schema refuses, because the file is still on disk and a row that named
-- what wrote it is the only way to find it again.
CREATE TABLE published_resources (
    id                INTEGER PRIMARY KEY,
    document_id       INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    output_channel_id INTEGER NOT NULL REFERENCES output_channels(id),
    version_id        INTEGER NOT NULL REFERENCES document_versions(id),
    uri               TEXT    NOT NULL,
    path              TEXT    NOT NULL,
    checksum          TEXT    NOT NULL,
    bytes             INTEGER NOT NULL,
    published_at      TEXT    NOT NULL,
    UNIQUE (output_channel_id, uri)
) STRICT;

-- "What did we write for this document in this channel", which is the question
-- the expiry diff asks on every republish and the question
-- GET /documents/{uid}/resources answers.
CREATE INDEX published_resources_document
    ON published_resources(document_id, output_channel_id);

-- The default workflow's "Publish" transition now publishes.
--
-- The vocabulary gains two members in this milestone and this is the row they
-- exist for: the effect "publish", which enqueues a publish job pinned to the
-- document's newest checked-in version, and the guard "has_checked_in_version",
-- which refuses a document that has none. Adding either name without its
-- enforcement would be invariant 6's failure exactly; internal/workflow
-- enforces the guard and applies the effect, and internal/publish runs the job,
-- all in the commit that adds this line.
--
-- The two arrive together and domain.Workflow.Validate refuses one without the
-- other. A publish pins a checked-in version, so a transition that could
-- publish must be able to refuse a document that has never been checked in --
-- and it must refuse in "check", where Available and Do share one answer,
-- rather than while applying the effect, which is after Available has already
-- offered the move (invariant 5).
--
-- The row is matched by the states it joins rather than by id, because an
-- installation that has edited its copy of the default workflow should keep its
-- edits and still get the effect the state machine already implied.
UPDATE workflow_transitions
   SET guards  = '["not_locked","has_cover_date","has_checked_in_version"]',
       effects = '{"publish":""}'
 WHERE workflow_id = (SELECT id FROM workflows WHERE uid = '00000000000000000000000001')
   AND from_state = 'approved'
   AND to_state = 'published';
