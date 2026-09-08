// Copyright (c) 2026 Michael D Henderson.

//go:build production

package buildenv

import (
	"fmt"
	"os"
)

// Verify panics unless CMS_ENV is exported as exactly "production".
//
// A release binary requires the value to be set explicitly, because a server
// should say what it is.
func Verify() {
	if v := os.Getenv("CMS_ENV"); v != "production" {
		panic(fmt.Sprintf(
			"buildenv: built with -tags production, which requires CMS_ENV=production; got %q", v))
	}
}
