// Package tandem exposes build-time assets shared by the native daemon.
package tandem

import _ "embed"

// DefaultCatalog is the checked-in agent catalog embedded verbatim.
//
//go:embed config.yml.example
var DefaultCatalog []byte
