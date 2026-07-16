//go:build !tandem_dev

// Package ui exposes the production React build embedded in the Tandem binary.
package ui

import (
	"embed"
	"io/fs"
)

// dist is produced reproducibly by scripts/stage-go-ui.sh before a production
// Go build. Keeping the staging step explicit lets frontend dependencies remain
// owned by npm while the resulting daemon is a single executable.
//
//go:embed dist
var dist embed.FS

func Filesystem() (fs.FS, bool) {
	files, err := fs.Sub(dist, "dist")
	return files, err == nil
}
