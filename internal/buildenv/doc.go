// Copyright (c) 2026 Michael D Henderson.

// Package buildenv is the build/environment interlock (DESIGN.md 14). It is
// the only build-tagged code in this repository.
//
// One tag exists, "production", and it does exactly one thing: it asserts that
// the binary is running on the kind of machine it was built for. Release
// binaries are built with it; everything else is built without it.
//
// Three things about it are easy to get wrong, so they are stated here as
// well as in the design:
//
//   - Verify reads the exported CMS_ENV only, never the resolved environment
//     from the precedence chain in internal/config. It answers "is this binary
//     on the machine it was built for", and it answers before flags, the config
//     file, or defaults have been consulted.
//   - Verify never gates a route or a feature. The /__development/* routes are
//     gated on the resolved environment and nothing else (DESIGN.md 11). Do not
//     reach for this tag to hide code.
//   - Each command's main calls Verify explicitly. It is deliberately not an
//     init: init fires before main gets to do anything, and main may want to
//     handle version or --help first (invariant 18). There is no init function
//     in this package, and a test asserts it.
//
// Permitted imports: the standard library. This package must never grow a
// second responsibility.
package buildenv
