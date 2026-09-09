-- Copyright (c) 2026 Michael D Henderson.
--
-- 0009: sites get a hierarchy and an address (DESIGN.md 5.3, 5.5, PLAN.md M7).
--
-- Four of the five tables here were promised by 0003's header note: the scope
-- columns of "grants" arrive with the migrations that create the tables they
-- point at, because SQLite cannot add a foreign key to a column that already
-- exists and a scope column with no referential integrity is the rule column
-- nothing reads (invariant 6). This migration creates "categories" and
-- "collections", and adds category_id, category_deep and collection_id with
-- their REFERENCES clauses attached. After it, every one of DESIGN.md 7's nine
-- scope dimensions has a column, and domain.Scope stops being ahead of the
-- schema for the first time since M2.
--
-- What is new rather than promised is "output_channels", which is where a URI
-- comes from, and "document_categories", which is what gives a document a
-- place in the hierarchy to build one out of.

-- Categories: a per-site tree with a materialised path (DESIGN.md 5.3).
--
-- Two things depend on "path", and they are the reason it is a column rather
-- than a walk: URI construction, and permission scope matching by subtree
-- prefix (DESIGN.md 7.1). The system we learned from had no ancestor walk
-- anywhere in its authorization path, so a grant on /features did not cover
-- /features/film and the documented workaround was to write the grant again
-- for every child. A prefix test on this column is that walk, done once, in
-- an index.
--
-- The rules the rest of the system may assume, and which internal/domain
-- enforces as pure functions:
--
--   * path always begins and ends with '/'. The root is '/'.
--   * directory is one path segment, and it is empty for the root and only
--     for the root -- so path = parent.path || directory || '/' holds for
--     every row, root included, without a special case in the SQL.
--   * parent_id is NULL for the root of a site and for no other row.
--
-- UNIQUE (site_id, path) is what makes "one root per site" true: the root's
-- path is '/' on every site, so a second one cannot be inserted. It is also
-- the index the prefix match seeks on.
CREATE TABLE categories (
    id        INTEGER PRIMARY KEY,
    uid       TEXT    NOT NULL UNIQUE,
    site_id   INTEGER NOT NULL REFERENCES sites(id),
    parent_id INTEGER          REFERENCES categories(id),
    directory TEXT    NOT NULL,        -- one path segment; empty only for the root
    path      TEXT    NOT NULL,        -- materialised, always '/'-terminated
    name      TEXT    NOT NULL,
    UNIQUE (site_id, path)
) STRICT;

-- "What is directly under this one", which is what a category listing asks and
-- what a move has to refuse when it would orphan rows.
CREATE INDEX categories_parent ON categories(parent_id) WHERE parent_id IS NOT NULL;

-- Every site that already exists gets its root.
--
-- It is created here rather than left to whoever notices, because "a site has
-- a root category" is what makes path arithmetic total: a document filed
-- nowhere is filed at '/', and a URI built from a site with no root would have
-- no leading slash to start from. store.CreateSite writes one in the same
-- transaction as the site from here on, so the rule holds for sites created
-- later too.
--
-- The uid is derived from the site's rather than minted: a migration has no
-- clock and no entropy source. Prefixing with 'R' keeps it out of the ULID
-- space ids.New draws from, so nothing can collide with it, and it stays
-- stable across rebuilds, which is what lets a fixture name the row.
INSERT INTO categories (uid, site_id, parent_id, directory, path, name)
SELECT 'R' || s.uid, s.id, NULL, '', '/', s.name
  FROM sites s;

-- Where a document is filed (DESIGN.md 5.3).
--
-- A document may sit in several categories and exactly one of them is
-- primary: the primary category is the one its URI is built from, and the
-- others are places it also appears. The partial unique index makes "exactly
-- one" a truth of the schema rather than of the code that writes it -- the
-- same shape as document_versions_one_draft, and for the same reason: without
-- it, "the" primary category is an arbitrary row and the failure looks like a
-- URI that moves on its own.
CREATE TABLE document_categories (
    document_id INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    category_id INTEGER NOT NULL REFERENCES categories(id),
    primary_cat INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (document_id, category_id)
) STRICT;

CREATE UNIQUE INDEX document_categories_one_primary
    ON document_categories(document_id) WHERE primary_cat = 1;

-- "Everything filed here", which is the query a category page is.
CREATE INDEX document_categories_category ON document_categories(category_id);

-- Output channels carry the URI format (DESIGN.md 5.3).
--
-- uri_format and fixed_uri_format are strftime strings with the %{categories}
-- and %{slug} extensions, expanded against the document's cover date.
-- internal/domain holds the formatter; Go's reference-time layouts cannot
-- express them and translating one into the other is a lossy exercise nobody
-- would be able to debug.
--
-- fixed_uri_format is the format used for a document whose element type sets
-- fixed_uri: a page that lives at one address forever rather than at one
-- derived from the date it was published. element_types.fixed_uri has been in
-- the schema since 0004 and this is the column that gives it a meaning.
--
-- uri_case is 'mixed', 'lower', or 'upper', and the CHECK is here because
-- three spellings of a case rule is one spelling too many for something that
-- decides what a URL looks like.
CREATE TABLE output_channels (
    id               INTEGER PRIMARY KEY,
    uid              TEXT    NOT NULL UNIQUE,
    site_id          INTEGER NOT NULL REFERENCES sites(id),
    name             TEXT    NOT NULL,
    protocol         TEXT    NOT NULL DEFAULT 'https://',
    filename         TEXT    NOT NULL DEFAULT 'index',
    file_ext         TEXT    NOT NULL DEFAULT 'html',
    uri_format       TEXT    NOT NULL,   -- strftime + %{categories} %{slug}
    fixed_uri_format TEXT    NOT NULL,
    use_slug         INTEGER NOT NULL DEFAULT 0,
    uri_case         TEXT    NOT NULL DEFAULT 'mixed'
        CHECK (uri_case IN ('mixed', 'lower', 'upper')),
    UNIQUE (site_id, name)
) STRICT;

-- Collections (DESIGN.md 5.5).
--
-- A collection is a named set of documents that is not a place in the
-- hierarchy: "the Christmas package", "everything the legal team is looking
-- at". It is here because grants.collection_id needs a table to point at, and
-- because DESIGN.md 7 has resolved a collection-scoped grant since M2 --
-- domain.Scope and internal/authz carry all nine dimensions from the first
-- commit, so the resolver does not change when a column lands.
--
-- There is deliberately no API for writing one yet. That is not a rule column
-- nothing reads: internal/authz reads collection_id, a grant naming one is
-- refused by the foreign key unless the collection exists, and what is missing
-- is a way for a person to create one rather than an enforcement path.
CREATE TABLE collections (
    id   INTEGER PRIMARY KEY,
    uid  TEXT NOT NULL UNIQUE,
    slug TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL
) STRICT;

CREATE TABLE document_collections (
    document_id   INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    collection_id INTEGER NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
    PRIMARY KEY (document_id, collection_id)
) STRICT;

-- The last three scope columns 0003 promised (0003, header note).
--
-- category_deep defaults to 1, which is what a person means by a grant on a
-- category: /features covers /features/film. It is the behaviour the system we
-- learned from could not express at all, and the default makes the expressible
-- thing the ordinary one.
ALTER TABLE grants ADD COLUMN category_id   INTEGER REFERENCES categories(id);
ALTER TABLE grants ADD COLUMN category_deep INTEGER NOT NULL DEFAULT 1;
ALTER TABLE grants ADD COLUMN collection_id INTEGER REFERENCES collections(id);
