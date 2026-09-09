// Copyright (c) 2026 Michael D Henderson.

// Package publish renders documents to the output tree, records what it wrote,
// and expires what it no longer writes (DESIGN.md 8, PLAN.md M9).
//
// Three ideas carry the milestone, and all three are corrections of systems
// that got them wrong.
//
// A publish job pins a version id, never a document id (invariant 8). An
// editor approves version 5 for midnight and keeps working; at midnight
// version 5 publishes, not whatever the draft has become. The immutability
// trigger on document_versions is what makes that promise real, and a design
// that models this as "publish the current draft at time T" cannot express it
// at all. PLAN.md M9 acceptance 1 is the test, and it is the single most
// important test in the project.
//
// Every file written is remembered, and a republish diffs the addresses it now
// produces against the addresses it produced before. Without that, changing a
// cover date, a slug, or a category leaves the file at the old URI serving
// forever -- almost no CMS gets this right, and retrofitting it means a
// reconciliation pass over an already-dirty output tree (DESIGN.md 8.3).
//
// A URI collision is detected by SQLITE_CONSTRAINT_UNIQUE on
// UNIQUE (output_channel_id, uri) and never by matching message text
// (invariant 11). The system we learned from regexed PostgreSQL 7.1 error
// strings for this, which is why its friendly "URI is not unique" message has
// not fired on any server built this century.
//
// The output tree is the one place in this system that creates a directory,
// and tree.go's header note says exactly why the exception is narrow and what
// of invariant 19 it keeps: the root must already exist, and only the
// computed interior beneath it is made.
//
// M10 adds the related-asset cascade (DESIGN.md 8.2). Publishing a document
// publishes the documents it references, and the traversal that decides which
// ones is a pure function over a loaded graph -- gather.go -- so that cycle
// safety, the per-node permission check, the state gate and the lock gate can
// all be tested without a database. graph.go is the half that reads. What to
// do about a refusal is not decided here: Gather reports, and
// config.RelatedFailure, resolved by internal/service, decides.
//
// Permitted imports: internal/domain, internal/authz, internal/store,
// internal/render, internal/jobs, internal/clock, internal/events,
// internal/ids.
//
// internal/authz arrived with the cascade and is the reason to say so: the
// per-node permission check is authz.Allows over each related document's
// subject, which is the same pure resolver internal/service asks about the
// root. A predicate passed in by the caller would have hidden the one rule
// DESIGN.md 8.2 is most emphatic about behind a function pointer.
package publish
