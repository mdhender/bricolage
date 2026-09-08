// Copyright (c) 2026 Michael D Henderson.

// Package config resolves configuration from flags, the environment, and a
// file, and holds the defaults.
//
// The setting that carries the most weight is the environment (DESIGN.md 14).
// Because no build tag gates the /__development/* routes, the resolved
// environment is the only thing standing between a deployment and an
// authentication bypass. Treat every change to how it resolves as a security
// change.
//
// Permitted imports: the standard library.
package config
