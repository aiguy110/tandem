//go:build tandem_dev

package ui

import "io/fs"

// Filesystem reports no embedded UI in lightweight development builds. The
// HTTP server will use TANDEM_UI_DIR when configured or show its placeholder.
func Filesystem() (fs.FS, bool) { return nil, false }
