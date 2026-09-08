-- Copyright (c) 2026 Michael D Henderson.
--
-- 0003: identity, roles, grants, and sessions (DESIGN.md 7, PLAN.md M2).
--
-- Four ideas, in the order they depend on one another: a user may hold a
-- password; a user holds roles; a role holds scoped grants; and a login is a
-- session row keyed by the hash of a token this database never sees in the
-- clear.
--
-- What is deliberately not here: the scope columns of "grants" whose target
-- table does not exist yet. DESIGN.md 7 gives the grant a nine-column scope,
-- four of which point at tables later milestones create -- categories,
-- workflows, collections, documents. SQLite cannot add a foreign key to a
-- column that already exists, so a column added now without its REFERENCES
-- clause would never get one, and a scope column with no referential integrity
-- is the "rule column nothing reads" that invariant 6 is about. Each is added
-- by the migration that creates its table, with the constraint attached:
--
--     ALTER TABLE grants ADD COLUMN category_id INTEGER REFERENCES categories(id);
--
-- which SQLite permits because the default is NULL, and NULL is the wildcard.
-- internal/domain carries the whole scope and internal/authz resolves all of
-- it today (PLAN.md M2 acceptance 6); this file stores the part that has
-- somewhere to point.

-- Credentials on users. The identity columns are 0001's; this is the half M2
-- adds (PLAN.md M2, "Schema").
--
-- The empty string means "no password set", which is a real state and not a
-- missing value: "cmsdb bootstrap admin" is the only thing that sets one, and
-- a user who has never been given a password must not be able to log in with
-- the empty one. Verification treats it as a definite failure rather than
-- comparing it.
--
-- bcrypt output is self-describing -- algorithm, cost, and salt travel in the
-- string -- so raising the cost later needs no migration and no second column.
ALTER TABLE users ADD COLUMN password_hash TEXT NOT NULL DEFAULT '';

-- Roles are flat (DESIGN.md 7.1). Nested roles were considered and rejected:
-- use multiple assignment, and add parent_role_id only when a concrete need
-- appears. There is no uid here on purpose -- the slug is the external
-- identifier, and invariant 10 is about integer primary keys, not about every
-- table carrying a ULID.
CREATE TABLE roles (
    id   INTEGER PRIMARY KEY,
    slug TEXT    NOT NULL UNIQUE,
    name TEXT    NOT NULL
) STRICT;

CREATE TABLE user_roles (
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role_id INTEGER NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
    PRIMARY KEY (user_id, role_id)
) STRICT;

-- The reverse direction of the primary key: "who holds this role" is the
-- question the role administration screens ask.
CREATE INDEX user_roles_role ON user_roles(role_id);

-- Sites arrive here rather than in M3 because "cmsdb seed" creates one in this
-- milestone and because grants.site_id points at it. DESIGN.md 5.3 unchanged.
CREATE TABLE sites (
    id     INTEGER PRIMARY KEY,
    uid    TEXT    NOT NULL UNIQUE,
    name   TEXT    NOT NULL,
    domain TEXT    NOT NULL,
    active INTEGER NOT NULL DEFAULT 1
) STRICT;

-- Grants: a privilege, held by a role, over a scope (DESIGN.md 7).
--
-- NULL is a wildcard in every scope column, and a grant matches when every
-- non-NULL column matches. The privilege scale is ordered and cumulative so
-- that "may they do this?" is one integer comparison, and DENY is 255, the
-- numeric maximum, so that effective privilege is MAX(privilege) over matching
-- grants and deny-overrides falls out with no second pass.
--
-- created_by is nullable because "cmsdb seed" writes the first grants before
-- any user exists to have written them; NULL means the system.
CREATE TABLE grants (
    id         INTEGER PRIMARY KEY,
    role_id    INTEGER NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
    privilege  INTEGER NOT NULL,          -- 1 read .. 5 publish, 255 deny

    -- Scope. See the note at the top of this file for the four columns that
    -- are not here yet and which migration adds each of them.
    site_id    INTEGER          REFERENCES sites(id),
    doc_kind   TEXT,
    state      TEXT,

    created_at TEXT    NOT NULL,
    created_by INTEGER          REFERENCES users(id)
) STRICT;

-- Grants are loaded per role, once per request (DESIGN.md 7.2).
CREATE INDEX grants_role ON grants(role_id);

-- Sessions (PLAN.md M2, "Schema").
--
-- The database stores only the SHA-256 of the token, lowercase hex
-- (DESIGN.md 14). A token is 32 random bytes and is shown to its owner once;
-- a stolen copy of this table is not a set of usable credentials. SHA-256 and
-- not bcrypt because the input is 256 bits of entropy rather than something a
-- person chose, so there is nothing to slow an attacker down about.
--
-- The lookup is by token_sha256, which is why it carries the UNIQUE index that
-- is also the collision guard.
CREATE TABLE sessions (
    id           INTEGER PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_sha256 TEXT    NOT NULL UNIQUE,
    created_at   TEXT    NOT NULL,
    expires_at   TEXT    NOT NULL,
    last_seen_at TEXT    NOT NULL
) STRICT;

CREATE INDEX sessions_user   ON sessions(user_id);

-- Expiry is swept by date, and an expired session is not a session
-- (PLAN.md M2 acceptance 8).
CREATE INDEX sessions_expiry ON sessions(expires_at);
