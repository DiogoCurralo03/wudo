// Package version holds build information shared by the wudo binaries.
package version

import (
	"fmt"
	"runtime"
)

// These values can be overridden at build time with -ldflags "-X".
var (
	// Version is the semantic version of the wudo binaries.
	Version = "0.1.1"
	// Commit is the short git commit the binary was built from.
	Commit = "dev"
	// BuildDate is the date the binary was built.
	BuildDate = "unknown"
)

// String returns a single line description of the build.
func String() string {
	return fmt.Sprintf("%s (commit %s, built %s, %s/%s, %s)",
		Version, Commit, BuildDate, runtime.GOOS, runtime.GOARCH, runtime.Version())
}

// Short returns just the version number.
func Short() string { return Version }
