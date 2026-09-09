-- Copyright (c) 2026 Michael D Henderson.
--
-- 0006: one workflow governs one kind on one site (DESIGN.md 5.4).
--
-- 0005 created "workflows" without saying how many may govern the same
-- documents, and internal/store.WorkflowFor resolves the question with
--
--     WHERE kind = :kind AND (site_id = :site_id OR site_id IS NULL)
--     ORDER BY site_id IS NULL, id LIMIT 1
--
-- so a site-specific workflow wins over the general one. That ordering is the
-- rule; without these indexes it is only a tie-break. Two workflows configured
-- for the same kind and the same site are accepted, the lower id silently
-- wins, and every document created afterwards enters one of them with no error
-- and nothing to notice -- which is the same shape of failure as a rule column
-- nothing reads (invariant 6), a configuration that says one thing while the
-- system does another.
--
-- This is a new migration rather than an edit to 0005. Migrations are
-- append-only after beta, and the beta exception is a deliberate, separately
-- announced squash rather than a licence to edit one during ordinary work
-- (invariant 9).
--
-- Two partial indexes and not one UNIQUE (kind, site_id), because SQLite
-- treats NULLs as distinct in a unique index: UNIQUE (kind, site_id) would
-- permit any number of rows with site_id NULL, which is exactly the row this
-- is about -- the default workflow every site falls back to.

-- At most one workflow per kind applies to every site.
CREATE UNIQUE INDEX workflows_default_per_kind
    ON workflows(kind) WHERE site_id IS NULL;

-- At most one workflow per kind applies to a given site.
CREATE UNIQUE INDEX workflows_site_per_kind
    ON workflows(kind, site_id) WHERE site_id IS NOT NULL;
