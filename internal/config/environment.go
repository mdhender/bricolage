// Copyright (c) 2026 Michael D Henderson.

package config

// This file implements the environment setting described in DESIGN.md 14: one
// setting, two values, governing everything that should differ between a
// developer's machine and a real deployment. The package doc is in doc.go.

import (
	"fmt"
	"os"
)

// Environment is the resolved environment. There are exactly two. A third was
// considered and rejected: more states mean more combinations nobody tests,
// and anything that is not a developer's machine should behave like production.
type Environment string

const (
	Development Environment = "development"
	Production  Environment = "production"
)

// EnvVar is the environment variable consulted by Resolve, and the same one the
// build/environment interlock reads (DESIGN.md 14).
const EnvVar = "CMS_ENV"

// IsDevelopment reports whether the development affordances are enabled. This
// is the only question the rest of the system should ask. In particular it is
// what gates registration of the /__development/* routes (DESIGN.md 11), so
// treat any change to it as a security change.
func (e Environment) IsDevelopment() bool { return e == Development }

// String makes Environment printable in the startup banner and in logs.
func (e Environment) String() string { return string(e) }

// Source names where a resolved environment came from, so that the startup
// banner can say not just what the environment is but why.
type Source string

const (
	SourceFlag    Source = "--env"
	SourceEnvVar  Source = EnvVar
	SourceFile    Source = "config file"
	SourceDefault Source = "default"
)

// Inputs are the raw, unresolved values in precedence order, highest first.
// An empty string means the source did not supply a value; that is why these
// are strings rather than Environment values.
//
// Deliberately absent: the hostname, the listen address, and whether a terminal
// is attached. The environment is never inferred from any of them.
type Inputs struct {
	Flag string // --env, highest precedence
	Env  string // $CMS_ENV
	File string // the environment key in the config file
}

// FromEnv reads the raw CMS_ENV value. Call it when building Inputs, so that
// Resolve itself stays a pure function of its argument.
func FromEnv() string { return os.Getenv(EnvVar) }

// Resolution is the outcome of Resolve: the environment, where it came from,
// and enough about the raw input to warn when it was not what someone meant.
type Resolution struct {
	Environment Environment
	Source      Source
	Raw         string // the raw value from Source; empty when Source is SourceDefault
	Recognized  bool   // whether Raw was exactly "development" or "production"
}

// Resolve applies the precedence chain from DESIGN.md 14: --env, then $CMS_ENV,
// then the config file, then the default. The default is production, which is
// the fail-safe direction — an unset or misspelled value never accidentally
// unlocks anything.
//
// Two rules do the work, and both matter:
//
//   - The highest source that supplies any value at all wins, even if that
//     value is not a recognized environment. A set-but-misspelled CMS_ENV does
//     not fall through to a config file that says "development"; falling
//     through is how a typo unlocks the thing the typo was supposed to gate.
//   - A value is "development" only if it is exactly that string. Anything
//     else — "dev", "Development", " development", "" — is production. No
//     trimming, no case folding, no synonyms.
//
// Together they mean the only way to reach Development is to have written it
// out exactly, in the highest source that has an opinion.
//
// Resolve never fails. A caller wanting to warn about a value that was set but
// not understood should check Recognized.
func Resolve(in Inputs) Resolution {
	for _, candidate := range []struct {
		raw    string
		source Source
	}{
		{in.Flag, SourceFlag},
		{in.Env, SourceEnvVar},
		{in.File, SourceFile},
	} {
		if candidate.raw == "" {
			continue
		}
		env, recognized := parse(candidate.raw)
		return Resolution{
			Environment: env,
			Source:      candidate.source,
			Raw:         candidate.raw,
			Recognized:  recognized,
		}
	}

	return Resolution{
		Environment: Production,
		Source:      SourceDefault,
		Recognized:  true,
	}
}

// parse maps a raw value to an environment. Only the two exact strings are
// recognized; everything else is production, reported as unrecognized.
func parse(raw string) (env Environment, recognized bool) {
	switch Environment(raw) {
	case Development:
		return Development, true
	case Production:
		return Production, true
	default:
		return Production, false
	}
}

// IsDevelopment reports whether the resolved environment is development.
func (r Resolution) IsDevelopment() bool { return r.Environment.IsDevelopment() }

// String renders the resolution for the startup banner, which cmsd prints on
// every start in every environment so the value is greppable in a log rather
// than inferred from a run script.
func (r Resolution) String() string {
	if r.Source == SourceDefault {
		return fmt.Sprintf("environment=%s source=default", r.Environment)
	}
	if !r.Recognized {
		return fmt.Sprintf("environment=%s source=%s raw=%q unrecognized, defaulted to %s",
			r.Environment, r.Source, r.Raw, Production)
	}
	return fmt.Sprintf("environment=%s source=%s", r.Environment, r.Source)
}
