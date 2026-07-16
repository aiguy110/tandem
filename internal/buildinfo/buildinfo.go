// Package buildinfo exposes metadata injected into release binaries with -ldflags -X.
package buildinfo

import "fmt"

// These development defaults are replaced by release and CI builds. Keep them variables
// so the Go linker's -X flag can set them without generated source files.
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildTime = "unknown"
)

// String returns the stable, human-readable version command output.
func String() string {
	return fmt.Sprintf("tandem version=%s commit=%s built=%s", Version, Commit, BuildTime)
}
