// Copyright (c) 2026 Michael D Henderson.

package config

import (
	"strings"
	"testing"

	"github.com/mdhender/bricolage/internal/dotenv"
)

// TestParseOnlyExactStrings is the rule the /__development/* gate rests on:
// nothing but the exact lowercase string reaches Development, and everything
// unrecognized fails safe to Production.
func TestParseOnlyExactStrings(t *testing.T) {
	for _, tc := range []struct {
		raw        string
		want       Environment
		recognized bool
	}{
		{"development", Development, true},
		{"production", Production, true},
		{"dev", Production, false},
		{"prod", Production, false},
		{"Development", Production, false},
		{"DEVELOPMENT", Production, false},
		{"developmen", Production, false},
		{"development ", Production, false},
		{" development", Production, false},
		{"\tdevelopment", Production, false},
		{"development\n", Production, false},
		{"test", Production, false},
		{"agents", Production, false},
		{"staging", Production, false},
	} {
		env, recognized := parse(tc.raw)
		if env != tc.want || recognized != tc.recognized {
			t.Errorf("parse(%q) = %v, %v; want %v, %v", tc.raw, env, recognized, tc.want, tc.recognized)
		}
	}
}

// TestResolveDefaultsToProduction covers the fail-safe direction: with nothing
// set anywhere, the answer is production.
func TestResolveDefaultsToProduction(t *testing.T) {
	got := Resolve(Inputs{})
	if got.Environment != Production {
		t.Errorf("Resolve({}) = %v, want %v", got.Environment, Production)
	}
	if got.Source != SourceDefault {
		t.Errorf("Source = %v, want %v", got.Source, SourceDefault)
	}
	if got.IsDevelopment() {
		t.Error("IsDevelopment() = true for the zero Inputs, want false")
	}
}

// TestResolvePrecedence walks the chain from DESIGN.md 14: --env, then CMS_ENV,
// then the config file, then the default.
func TestResolvePrecedence(t *testing.T) {
	for _, tc := range []struct {
		name       string
		in         Inputs
		wantEnv    Environment
		wantSource Source
	}{
		{"flag over everything", Inputs{Flag: "development", Env: "production", File: "production"}, Development, SourceFlag},
		{"flag over everything, reversed", Inputs{Flag: "production", Env: "development", File: "development"}, Production, SourceFlag},
		{"env var when no flag", Inputs{Env: "development", File: "production"}, Development, SourceEnvVar},
		{"env var over file, reversed", Inputs{Env: "production", File: "development"}, Production, SourceEnvVar},
		{"file when nothing else", Inputs{File: "development"}, Development, SourceFile},
		{"default when nothing at all", Inputs{}, Production, SourceDefault},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Resolve(tc.in)
			if got.Environment != tc.wantEnv {
				t.Errorf("Environment = %v, want %v", got.Environment, tc.wantEnv)
			}
			if got.Source != tc.wantSource {
				t.Errorf("Source = %v, want %v", got.Source, tc.wantSource)
			}
		})
	}
}

// TestResolveSetButMisspelledDoesNotFallThrough is the subtle half of the
// precedence rule. A higher source that supplies an unrecognized value still
// wins, and resolves to production. If it fell through instead, a typo in
// CMS_ENV would hand control to a config file — which is how a typo unlocks the
// thing the typo was meant to gate.
func TestResolveSetButMisspelledDoesNotFallThrough(t *testing.T) {
	for _, tc := range []struct {
		name       string
		in         Inputs
		wantSource Source
	}{
		{"misspelled flag beats a development env var", Inputs{Flag: "developmnt", Env: "development"}, SourceFlag},
		{"misspelled env var beats a development file", Inputs{Env: "Development", File: "development"}, SourceEnvVar},
		{"misspelled file does not reach the default quietly", Inputs{File: "dev"}, SourceFile},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Resolve(tc.in)
			if got.Environment != Production {
				t.Errorf("Environment = %v, want %v", got.Environment, Production)
			}
			if got.Source != tc.wantSource {
				t.Errorf("Source = %v, want %v", got.Source, tc.wantSource)
			}
			if got.Recognized {
				t.Error("Recognized = true, want false so the banner can warn")
			}
		})
	}
}

// TestResolveReportsRawForTheBanner confirms a caller can tell a deliberate
// production from a misspelling that landed on production.
func TestResolveReportsRawForTheBanner(t *testing.T) {
	deliberate := Resolve(Inputs{Env: "production"})
	if !deliberate.Recognized {
		t.Error("Recognized = false for an exact production value, want true")
	}

	typo := Resolve(Inputs{Env: "prodcution"})
	if typo.Recognized {
		t.Error("Recognized = true for a misspelling, want false")
	}
	if typo.Raw != "prodcution" {
		t.Errorf("Raw = %q, want the raw input back", typo.Raw)
	}
	if s := typo.String(); !strings.Contains(s, "unrecognized") || !strings.Contains(s, "prodcution") {
		t.Errorf("String() = %q, want it to name the unrecognized raw value", s)
	}
}

func TestFromEnvReadsCMSEnv(t *testing.T) {
	t.Setenv(EnvVar, "development")
	if got := FromEnv(); got != "development" {
		t.Errorf("FromEnv() = %q, want %q", got, "development")
	}

	t.Setenv(EnvVar, "")
	if got := FromEnv(); got != "" {
		t.Errorf("FromEnv() = %q for an empty %s, want an empty string so it counts as absent", got, EnvVar)
	}
	if got := Resolve(Inputs{Env: FromEnv()}); got.Source != SourceDefault {
		t.Errorf("Source = %v for an empty %s, want %v", got.Source, EnvVar, SourceDefault)
	}
}

// TestResolvedEnvironmentIsAlwaysLoadable is the handoff to internal/dotenv,
// which accepts only the two environments. Resolve can only ever produce those
// two, so the pairing cannot drift without this test failing.
func TestResolvedEnvironmentIsAlwaysLoadable(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, in := range []Inputs{
		{},
		{Flag: "development"},
		{Flag: "production"},
		{Env: "garbage"},
		{File: "also garbage"},
	} {
		env := Resolve(in).Environment
		if err := dotenv.Load(env.String()); err != nil {
			t.Errorf("dotenv.Load(%q) from Resolve(%+v) = %v, want nil", env, in, err)
		}
	}
}
