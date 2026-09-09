-- Copyright (c) 2026 Michael D Henderson.
--
-- 0004: element types, documents, and versions (DESIGN.md 5.1, 5.2, PLAN.md M3).
--
-- The one structural idea taken from the system we learned from unchanged:
-- identity and version are separate rows. A document is a stable identity with
-- a lock; a version is an immutable snapshot of content. Three defects in the
-- original die here by construction rather than by care -- the lock lives on
-- the document row, there is no version 0, and immutability is a trigger.
--
-- What is deliberately not here, and why:
--
--   * workflow_id and state. DESIGN.md 5.1 gives documents both, NOT NULL,
--     with a composite foreign key to workflow_states(workflow_id, slug).
--     Those tables arrive in M4, and SQLite can neither add a foreign key to
--     an existing column nor add a table-level composite one at all. Adding
--     the columns now without their constraints would leave them permanently
--     unconstrained, which is the "rule column nothing reads" that invariant 6
--     is about. M4 creates the workflow tables and rebuilds this table through
--     SQLite's documented twelve-step ALTER procedure, which is the only way
--     to attach the composite key, and adds the documents_queue index that
--     needs both columns. Workflow state is out of scope for M3 by PLAN.md.
--
--   * categories and the category_id scope column of grants. Categories are
--     M6; that migration adds the column, with its REFERENCES clause, exactly
--     as 0003 promised.
--
-- What *is* here from that promise: grants.document_id, the scope column whose
-- target table this migration creates (0003, header note).

-- Element types (DESIGN.md 5.2).
--
-- Eighteen entity-attribute-value tables in the original become one JSON
-- column here. "schema" declares the fields a document of this type carries;
-- M3 stores it and does not yet validate content against it, which PLAN.md M3
-- says in as many words. The store checks that both this column and
-- document_versions.content hold a JSON object, so the column has a meaning
-- the database enforces today rather than a meaning it promises later.
CREATE TABLE element_types (
    id         INTEGER PRIMARY KEY,
    uid        TEXT    NOT NULL UNIQUE,
    key_name   TEXT    NOT NULL UNIQUE,
    name       TEXT    NOT NULL,
    kind       TEXT    NOT NULL,      -- which document kind it applies to
    top_level  INTEGER NOT NULL DEFAULT 0,
    fixed_uri  INTEGER NOT NULL DEFAULT 0,
    paginated  INTEGER NOT NULL DEFAULT 0,
    schema     TEXT    NOT NULL,      -- JSON: field definitions
    created_at TEXT    NOT NULL
) STRICT;

-- Documents: a stable identity with a lock (DESIGN.md 5.1).
--
-- The lock is a lease. lock_expires_at is set on checkout and extended by
-- activity, and an expired lock is not a lock -- which is what stops an editor
-- who closed their laptop from blocking a document until an administrator
-- intervenes, as they had to in the original.
--
-- current_version_id and live_version_id point forward into document_versions,
-- which points back here. The cycle is fine: SQLite checks foreign keys per
-- statement, so a document is inserted with both NULL, its first version is
-- written, and the pointer is set. Deleting a document clears the pointer
-- first, then deletes the row, and the cascade takes the versions with it.
CREATE TABLE documents (
    id                 INTEGER PRIMARY KEY,
    uid                TEXT    NOT NULL UNIQUE,        -- external identifier
    site_id            INTEGER NOT NULL REFERENCES sites(id),
    kind               TEXT    NOT NULL,               -- 'story' | 'media' | 'template'
    element_type_id    INTEGER NOT NULL REFERENCES element_types(id),

    assigned_to        INTEGER          REFERENCES users(id),
    due_at             TEXT,

    locked_by          INTEGER          REFERENCES users(id),
    lock_expires_at    TEXT,

    current_version_id INTEGER          REFERENCES document_versions(id),
    live_version_id    INTEGER          REFERENCES document_versions(id),

    created_at         TEXT    NOT NULL,
    updated_at         TEXT    NOT NULL
) STRICT;

-- "What is on my desk" and "what is late" (DESIGN.md 5.1). Both are partial:
-- the overwhelming majority of rows are NULL in both columns, and an index
-- over those rows would be an index over nothing anybody asks about.
CREATE INDEX documents_mine    ON documents(assigned_to)      WHERE assigned_to IS NOT NULL;
CREATE INDEX documents_overdue ON documents(due_at)           WHERE due_at IS NOT NULL;
CREATE INDEX documents_live    ON documents(live_version_id)  WHERE live_version_id IS NOT NULL;

-- Versions: an immutable snapshot (DESIGN.md 5.1).
--
-- version is 1-based and monotonic per document. There is no version 0:
-- "never checked in" is checked_in_at IS NULL, so nothing needs to know that
-- reverting a never-saved document means deleting it -- the absence of a
-- checked-in row says it.
CREATE TABLE document_versions (
    id            INTEGER PRIMARY KEY,
    document_id   INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    version       INTEGER NOT NULL,               -- 1-based, monotonic per document
    title         TEXT    NOT NULL,
    slug          TEXT    NOT NULL DEFAULT '',
    cover_date    TEXT,
    content       TEXT    NOT NULL DEFAULT '{}',  -- JSON element tree
    note          TEXT,                           -- check-in message
    created_by    INTEGER NOT NULL REFERENCES users(id),
    created_at    TEXT    NOT NULL,
    checked_in_at TEXT,                           -- NULL = open working draft
    UNIQUE (document_id, version)
) STRICT;

-- One open working draft per document, enforced here rather than by care.
--
-- Everything above this line assumes it: checkout finds "the" draft, check-in
-- closes "the" draft, revert discards "the" draft. A second open draft would
-- make each of those queries return an arbitrary row, and the failure would
-- look like lost edits rather than like a bug. A partial unique index says it
-- once, in the place that cannot be bypassed.
CREATE UNIQUE INDEX document_versions_one_draft
    ON document_versions(document_id) WHERE checked_in_at IS NULL;

-- Immutability is enforced by the database (DESIGN.md 5.1).
--
-- A scheduled publish pins a version id and can trust it (invariant 8). The
-- trigger also blocks well-meaning administrative UPDATEs, and that is the
-- intended trade: the escape hatch is documented and reached with "sqlite3",
-- never with a code path.
CREATE TRIGGER document_versions_immutable
BEFORE UPDATE ON document_versions
WHEN OLD.checked_in_at IS NOT NULL
BEGIN
    SELECT RAISE(ABORT, 'checked-in versions are immutable');
END;

-- The scope column 0003 promised to add with the migration that creates its
-- target table. A grant may now name a single document -- one row, where the
-- system we learned from wanted a one-member user group and a one-member
-- object group. NULL is the wildcard, which is why ALTER TABLE accepts it.
ALTER TABLE grants ADD COLUMN document_id INTEGER REFERENCES documents(id);
