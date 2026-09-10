// Copyright (c) 2026 Michael D Henderson.

package bricolage

import (
	"fmt"
	"runtime"

	"github.com/maloquacious/semver"
)

func Version() semver.Version {
	return semver.Version{
		Major:      0,
		Minor:      17,
		Patch:      0,
		PreRelease: "beta",
		Build:      semver.Commit(),
	}
}

// VersionString is what every command's "version" subcommand prints: the
// version, the commit that produced it, and the Go toolchain and platform it
// was built with (DESIGN.md 11).
//
// It lives here so that the three commands print the same thing in the same
// shape. Formatting a version is not application logic, and the alternative is
// three copies that drift.
func VersionString(program string) string {
	return fmt.Sprintf("%s %s %s %s/%s",
		program, Version(), runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
