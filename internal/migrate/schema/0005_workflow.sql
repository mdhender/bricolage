-- Copyright (c) 2026 Michael D Henderson.
--
-- 0005: the workflow engine's schema (DESIGN.md 5.4, 5.5, 6, PLAN.md M4).
--
-- This migration does four things, in the order they depend on one another:
-- it creates the workflow tables, it seeds the default story workflow, it
-- rebuilds "documents" so that a document has a workflow and a state, and it
-- adds the two supporting tables the guards read.
--
-- migrate: disable-foreign-keys
--
-- The directive above is not a convenience. Adding a table-level composite
-- foreign key to an existing table is impossible with ALTER TABLE, so
-- "documents" is rebuilt through SQLite's documented twelve-step procedure,
-- whose first step is turning foreign key enforcement off. Two things go
-- wrong with it on: DROP TABLE performs an implicit DELETE FROM, which
-- cascades into document_versions and takes every version with it, and the
-- transfer INSERT is checked against a parent table that is about to be
-- replaced. internal/migrate reads the directive and hands
-- sqlitemigration a MigrationOptions asking for it; enforcement is restored
-- when this migration's transaction commits.
--
-- Why the default workflow is seeded here rather than by "cmsdb seed":
-- documents.workflow_id is NOT NULL with a composite foreign key to
-- workflow_states, so no document row may exist before a workflow does. A
-- database that already holds documents -- "cmsdb seed --demo" creates one --
-- has nowhere to put them otherwise. DESIGN.md 5.4 says the default story
-- workflow is seeded by "cmsdb", and "cmsdb init" is cmsdb. "cmsdb seed"
-- reports what it finds here rather than declaring it a second time, because
-- a state machine written down twice is a state machine that drifts.

-- Workflows (DESIGN.md 5.4).
--
-- site_id is NULL for a workflow that applies to every site, which is what
-- the default one is. kind is the document kind it governs: a story workflow
-- does not govern media, and the resolver that places a new document looks
-- for a workflow matching its kind.
CREATE TABLE workflows (
    id            INTEGER PRIMARY KEY,
    uid           TEXT    NOT NULL UNIQUE,
    site_id       INTEGER          REFERENCES sites(id),   -- NULL = all sites
    kind          TEXT    NOT NULL,
    name          TEXT    NOT NULL,
    initial_state TEXT    NOT NULL
) STRICT;

-- States (DESIGN.md 5.4).
--
-- "position" is the column the system we learned from never had. Its desk
-- ordering was derived by sorting start-first, publish-last and everything
-- else by primary key, so "the order of the desks" was three buckets. Storing
-- it costs one integer.
--
-- The primary key is (workflow_id, slug) and that is deliberate: it is the
-- unique index documents' composite foreign key resolves against, so a
-- document can only ever name a state its own workflow declares.
CREATE TABLE workflow_states (
    workflow_id        INTEGER NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
    slug               TEXT    NOT NULL,
    name               TEXT    NOT NULL,
    position           INTEGER NOT NULL,
    publishable        INTEGER NOT NULL DEFAULT 0,
    terminal           INTEGER NOT NULL DEFAULT 0,
    required_approvals INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (workflow_id, slug)
) STRICT;

-- Transitions (DESIGN.md 5.4, 6.1).
--
-- Transitions are data so that an editorial process can be configured;
-- "guards" and "effects" name members of a closed vocabulary so that every
-- guard which can be configured is one the engine actually enforces.
--
--   guards   JSON array  ["not_locked","has_slug"]
--   effects  JSON object {"set_due_in":"48h","clear_assignee":""}
--
-- internal/domain parses both and refuses a name that is not in the
-- vocabulary, which is the whole of invariant 6 made mechanical: the system we
-- learned from shipped pre_chk_rules and post_chk_rules with tables, foreign
-- keys, indexes, an accessor API, and no implementation anywhere.
CREATE TABLE workflow_transitions (
    id          INTEGER PRIMARY KEY,
    workflow_id INTEGER NOT NULL,
    from_state  TEXT    NOT NULL,
    to_state    TEXT    NOT NULL,
    name        TEXT    NOT NULL,
    privilege   INTEGER NOT NULL,
    guards      TEXT    NOT NULL DEFAULT '[]',
    effects     TEXT    NOT NULL DEFAULT '{}',
    position    INTEGER NOT NULL DEFAULT 0,
    FOREIGN KEY (workflow_id, from_state) REFERENCES workflow_states(workflow_id, slug),
    FOREIGN KEY (workflow_id, to_state)   REFERENCES workflow_states(workflow_id, slug),
    UNIQUE (workflow_id, from_state, to_state)
) STRICT;

-- Approvals attach to a version, not a document (DESIGN.md 5.5).
--
-- Edit the document, a new version exists, and the old approvals no longer
-- satisfy the guard. Getting "changes invalidate sign-off" right costs one
-- foreign key. The UNIQUE constraint makes "two distinct people must approve"
-- a plain COUNT(*).
--
-- The table and the guard that reads it land together; the API that writes one
-- is M11 (PLAN.md M4, "Schema"). A guard whose data nothing can produce is a
-- guard nobody has tested, which is why internal/store can write one today.
CREATE TABLE approvals (
    id          INTEGER PRIMARY KEY,
    document_id INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    version_id  INTEGER NOT NULL REFERENCES document_versions(id) ON DELETE CASCADE,
    state       TEXT    NOT NULL,
    user_id     INTEGER NOT NULL REFERENCES users(id),
    created_at  TEXT    NOT NULL,
    UNIQUE (version_id, state, user_id)
) STRICT;

-- The guard counts approvals of one version in one state.
CREATE INDEX approvals_version ON approvals(version_id, state);

-- Comments are a thread (DESIGN.md 5.5).
--
-- They are here, one milestone before their API, for the same reason approvals
-- are: the comments_resolved guard is one of the seven, and PLAN.md M4 says it
-- must exist and be enforced rather than merely named. A guard stubbed to "no
-- comments exist" cannot refuse, and a guard that cannot refuse has not been
-- tested. document_versions.note stays as well -- the check-in message is a
-- different thing from a discussion.
CREATE TABLE comments (
    id          INTEGER PRIMARY KEY,
    uid         TEXT    NOT NULL UNIQUE,
    document_id INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    version_id  INTEGER          REFERENCES document_versions(id),
    in_reply_to INTEGER          REFERENCES comments(id),
    author_id   INTEGER NOT NULL REFERENCES users(id),
    body        TEXT    NOT NULL,
    resolved_at TEXT,
    resolved_by INTEGER          REFERENCES users(id),
    created_at  TEXT    NOT NULL
) STRICT;

-- "What is still open on this document" is the guard's question, and it is
-- the only one this index has to answer.
CREATE INDEX comments_open ON comments(document_id) WHERE resolved_at IS NULL;

-- The default story workflow (DESIGN.md 5.4).
--
--   draft ──submit──▶ review ──approve──▶ approved ──publish──▶ published
--     ▲                  │                    │                     │
--     └────reject────────┘                    │                     │
--     ◀────────────revoke─────────────────────┘                     │
--     ◀────────────revise───────────────────────────────────────────┘
--
-- plus archive from draft, review and published, and restore from archived.
--
-- "published" is a state, not an exit. The system we learned from removed a
-- published document from workflow entirely, so a live document was nowhere.
-- Here it stays, and documents.live_version_id independently records what is
-- serving: a document can sit in draft being revised while the previously
-- published version continues to serve, which is what actually happens.
--
-- The uid is a fixed ULID-shaped constant rather than a minted one. A
-- migration has no clock and no entropy source, and this is the one workflow
-- every database has: giving it a stable identifier means a test, a fixture,
-- and an operator can all name the same row.
INSERT INTO workflows (uid, site_id, kind, name, initial_state)
VALUES ('00000000000000000000000001', NULL, 'story', 'Story', 'draft');

-- required_approvals is 0 on every state, including review.
--
-- The approvals_met guard is enforced -- internal/workflow reads this column
-- and refuses when the count falls short -- but nothing can record an approval
-- through the API until M11, and a default process that no editor can move a
-- document through is a default process nobody would ship. Raising it is one
-- UPDATE, and M11 raises it with the API that satisfies it.
INSERT INTO workflow_states (workflow_id, slug, name, position, publishable, terminal, required_approvals)
SELECT w.id, s.slug, s.name, s.position, s.publishable, s.terminal, s.required_approvals
  FROM workflows w
  JOIN (SELECT 'draft'     AS slug, 'Draft'     AS name, 1 AS position, 0 AS publishable, 0 AS terminal, 0 AS required_approvals
        UNION ALL SELECT 'review',    'In review', 2, 0, 0, 0
        UNION ALL SELECT 'approved',  'Approved',  3, 1, 0, 0
        UNION ALL SELECT 'published', 'Published', 4, 1, 0, 0
        UNION ALL SELECT 'archived',  'Archived',  5, 0, 1, 0) s
 WHERE w.uid = '00000000000000000000000001';

-- The privilege each transition needs, on the scale of DESIGN.md 7:
-- 2 edit, 3 recall, 4 create, 5 publish. A writer holds edit and may submit;
-- an editor holds create and may approve, reject, revoke and archive; only an
-- administrator holds publish. Recall is the one genuinely workflow-shaped
-- privilege -- "pull back out of a terminal state" -- and it is what revise
-- and restore ask for.
--
-- Every one of the seven guards and all four effects appear here, so the
-- default process exercises the whole vocabulary rather than a corner of it.
INSERT INTO workflow_transitions (workflow_id, from_state, to_state, name, privilege, guards, effects, position)
SELECT w.id, t.from_state, t.to_state, t.name, t.privilege, t.guards, t.effects, t.position
  FROM workflows w
  JOIN (SELECT 'draft' AS from_state, 'review' AS to_state, 'Submit' AS name, 2 AS privilege,
               '["not_locked","assignee_only","has_slug"]' AS guards,
               '{"clear_assignee":"","set_due_in":"48h"}'  AS effects,
               1 AS position
        UNION ALL SELECT 'review',    'approved',  'Approve', 4,
               '["approvals_met","comments_resolved"]', '{"clear_assignee":""}', 2
        UNION ALL SELECT 'approved',  'published', 'Publish', 5,
               '["not_locked","has_cover_date"]', '{}', 3
        UNION ALL SELECT 'review',    'draft',     'Reject',  4,
               '["note_required"]', '{"clear_approvals":""}', 4
        UNION ALL SELECT 'approved',  'draft',     'Revoke',  4,
               '["note_required"]', '{"clear_approvals":""}', 5
        UNION ALL SELECT 'published', 'draft',     'Revise',  3,
               '[]', '{"assign_to_actor":"","clear_approvals":""}', 6
        UNION ALL SELECT 'draft',     'archived',  'Archive', 4,
               '["not_locked"]', '{"clear_assignee":""}', 7
        UNION ALL SELECT 'review',    'archived',  'Archive', 4,
               '["not_locked"]', '{"clear_assignee":""}', 8
        UNION ALL SELECT 'published', 'archived',  'Archive', 4,
               '["not_locked"]', '{"clear_assignee":""}', 9
        UNION ALL SELECT 'archived',  'draft',     'Restore', 3,
               '[]', '{}', 10) t
 WHERE w.uid = '00000000000000000000000001';

-- The scope column 0003 promised to add with the migration that creates its
-- target table (0003, header note). A grant may now be constrained to one
-- workflow; documents.state makes the "state" column it has carried since
-- 0003 resolve against something real for the first time.
ALTER TABLE grants ADD COLUMN workflow_id INTEGER REFERENCES workflows(id);

-- The twelve-step rebuild of "documents" (DESIGN.md 5.1).
--
-- 0004 created this table without workflow_id and state and said why: SQLite
-- can neither add a foreign key to an existing column nor add a table-level
-- composite one at all, and a column added without its
-- REFERENCES workflow_states(workflow_id, slug) would never get one. This is
-- the rebuild that promise named. The definition below is DESIGN.md 5.1's,
-- complete for the first time.
CREATE TABLE documents_rebuilt (
    id                 INTEGER PRIMARY KEY,
    uid                TEXT    NOT NULL UNIQUE,        -- external identifier
    site_id            INTEGER NOT NULL REFERENCES sites(id),
    kind               TEXT    NOT NULL,               -- 'story' | 'media' | 'template'
    element_type_id    INTEGER NOT NULL REFERENCES element_types(id),
    workflow_id        INTEGER NOT NULL REFERENCES workflows(id),
    state              TEXT    NOT NULL,

    assigned_to        INTEGER          REFERENCES users(id),
    due_at             TEXT,

    locked_by          INTEGER          REFERENCES users(id),
    lock_expires_at    TEXT,

    current_version_id INTEGER          REFERENCES document_versions(id),
    live_version_id    INTEGER          REFERENCES document_versions(id),

    created_at         TEXT    NOT NULL,
    updated_at         TEXT    NOT NULL,

    FOREIGN KEY (workflow_id, state)
        REFERENCES workflow_states(workflow_id, slug)
) STRICT;

-- Every existing document enters the default workflow in its initial state.
--
-- The CROSS JOIN names one row, so the workflow and the state come from the
-- same workflow by construction: taking the id from one subquery and the
-- initial state from another is how a document ends up naming a state its
-- workflow does not declare, which the composite foreign key would then
-- refuse in a message about a constraint rather than about the mistake.
INSERT INTO documents_rebuilt
       (id, uid, site_id, kind, element_type_id, workflow_id, state,
        assigned_to, due_at, locked_by, lock_expires_at,
        current_version_id, live_version_id, created_at, updated_at)
SELECT d.id, d.uid, d.site_id, d.kind, d.element_type_id, w.id, w.initial_state,
       d.assigned_to, d.due_at, d.locked_by, d.lock_expires_at,
       d.current_version_id, d.live_version_id, d.created_at, d.updated_at
  FROM documents d
 CROSS JOIN workflows w
 WHERE w.uid = '00000000000000000000000001';

DROP TABLE documents;

ALTER TABLE documents_rebuilt RENAME TO documents;

-- Step eight: the indexes the dropped table carried, plus the one that needed
-- both new columns.
--
-- documents_queue is DESIGN.md 5.1's, and it is the index M5's queue queries
-- are asserted against with EXPLAIN QUERY PLAN. The other three are partial:
-- the overwhelming majority of rows are NULL in those columns, and an index
-- over them would be an index over nothing anybody asks about.
CREATE INDEX documents_queue   ON documents(workflow_id, state, due_at);
CREATE INDEX documents_mine    ON documents(assigned_to)      WHERE assigned_to IS NOT NULL;
CREATE INDEX documents_overdue ON documents(due_at)           WHERE due_at IS NOT NULL;
CREATE INDEX documents_live    ON documents(live_version_id)  WHERE live_version_id IS NOT NULL;
