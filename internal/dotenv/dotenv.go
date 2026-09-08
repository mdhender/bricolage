// Copyright (c) 2026 Michael D Henderson.

// Package dotenv wraps the joho/godotenv package to load environment files.
package dotenv

import (
	"fmt"
	"os"

	"github.com/joho/godotenv"
	"github.com/mdhender/bricolage/internal/cerrs"
)

const (
	ErrMissingEnvironment = cerrs.Error("missing environment")
	ErrUnknownEnvironment = cerrs.Error("unknown environment")
)

// Load uses the `joho/godotenv` package to load environment files in the working directory.
//
// The env must be exactly "development" or "production". There is no third
// value: more states mean more combinations nobody tests, and anything that is
// not a developer's machine should behave like production. See DESIGN.md 14.
//
// Load the following files depending on `env`, with the first file having the highest precedence,
// and .env having the lowest precedence:
//
// Priority  Filename__________   .gitignore?  Secrets?  Notes_______________________________
// Highest   .env.{{env}}.local   Yes          Yes       Environment-specific local overrides
// 2nd       .env.local           Yes          Yes       Local overrides
// 3rd       .env.{{env}}         No           Never     Shared environment-specific variables
// Lowest    .env                 No           Never     Shared for all environments
//
// `.env.local` is loaded in every environment. bkeepers/dotenv skips it under
// test to keep local overrides out of test runs
// (https://github.com/bkeepers/dotenv/issues/418); we have no test
// environment, so the question does not arise.
func Load(env string) error {
	if env == "" {
		return ErrMissingEnvironment
	}
	if env != "development" && env != "production" {
		return ErrUnknownEnvironment
	}

	for _, path := range []string{
		".env." + env + ".local", // highest priority: .env.{{env}}.local
		".env.local",             // 2nd     priority: .env.local
		".env." + env,            // 3rd     priority: .env.{{env}}
		".env",                   // lowest  priority: .env
	} {
		// verify path exists and is a regular file
		if sb, err := os.Stat(path); err != nil || !sb.Mode().IsRegular() {
			continue
		}
		err := godotenv.Load(path)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}

	return nil
}
