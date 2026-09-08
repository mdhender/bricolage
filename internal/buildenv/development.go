// Copyright (c) 2026 Michael D Henderson.

//go:build !production

package buildenv

import "os"

// Verify panics if CMS_ENV is exported as "production". Any other value,
// including unset, is fine.
//
// The asymmetry with the tagged build is intentional. The ordinary binary
// rejects only the one value it must never see, because requiring developers to
// export anything in order to run "go run ./cmd/cmsd" is how you end up with a
// shell profile that exports it everywhere.
func Verify() {
	if v := os.Getenv("CMS_ENV"); v == "production" {
		panic("buildenv: built without -tags production and must not run with CMS_ENV=production")
	}
}
