// Copyright (c) 2026 Michael D Henderson.

// Package render looks up and executes templates for the publishing pipeline
// (DESIGN.md 8.4, PLAN.md M8).
//
// Two things live here and nothing else does. Lookup walks up the category
// tree -- a document in "/features/film/" looks for its element type's
// template in "/features/film/", then "/features/", then "/", first match wins
// -- and execution runs the match against a context assembled from one
// document, one version, one category, one site, and one output channel.
//
// The template tree is a directory of files, laid out to mirror the category
// tree it is searched by:
//
//	<root>/<site domain>/<category path>/<element type key>.gohtml
//	<root>/assemblage.localhost/features/film/story.gohtml
//	<root>/assemblage.localhost/story.gohtml
//
// The site directory is the one part DESIGN.md 8.4 does not spell out, and it
// is there because category paths are unique per site and not across sites:
// two sites both have "/features/", and a tree without the site level would
// silently hand one site's templates to the other. It is named by the site's
// domain because that is the key a person already types -- "cmsdb seed" looks
// a site up by it -- and because a directory named by a uid is a directory
// nobody can navigate.
//
// Nothing here writes to the output tree. Publish hands the bytes to
// internal/publish (PLAN.md M9) and preview hands them to Scratch, which is
// also here because the scratch tree is a rendering artefact rather than a
// use case. Neither creates a directory: both roots must already exist, the
// same rule --db lives under (invariant 19).
//
// Permitted imports: internal/domain, the standard library.
package render
