-- Copyright (c) 2026 Michael D Henderson.
--
-- 0001: users.
--
-- Every foreign key in this schema eventually points here (DESIGN.md 7), so
-- users is the first table rather than a table belonging to any one feature.
-- M2 extends it with credentials and adds roles, user_roles, grants, and
-- sessions; this migration carries only the identity columns that M2's shape
-- cannot change: the surrogate key, the external uid, and the two fields
-- "cmsdb bootstrap admin" is given on the command line.
--
-- STRICT (DESIGN.md 13.5): no affinity surprises.
-- uid is a lowercase ULID; the API speaks it and never the integer id
-- (invariant 10).
-- created_at is ISO-8601 UTC text, sortable and comparable in SQL.
CREATE TABLE users (
    id         INTEGER PRIMARY KEY,
    uid        TEXT    NOT NULL UNIQUE,
    email      TEXT    NOT NULL UNIQUE,
    name       TEXT    NOT NULL,
    active     INTEGER NOT NULL DEFAULT 1,
    created_at TEXT    NOT NULL
) STRICT;
