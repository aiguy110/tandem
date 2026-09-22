// Package tandem exposes build-time assets shared by the native daemon.
package tandem

import _ "embed"

// DefaultCatalog is the checked-in agent catalog embedded verbatim.
//
//go:embed config.yml.example
var DefaultCatalog []byte

// RuntimePackageJSON and RuntimePackageLock describe the locked Node runtime
// provisioned by standalone binaries under TANDEM_HOME.
//
//go:embed runtime/package.json
var RuntimePackageJSON []byte

//go:embed runtime/package-lock.json
var RuntimePackageLock []byte

// History importer sources are copied into a standalone installation's managed
// runtime before the first importer invocation.
//
//go:embed runtime/history/sdk.ts
var RuntimeHistorySDK []byte

//go:embed runtime/history/runner.ts
var RuntimeHistoryRunner []byte

//go:embed runtime/history/importers/common.ts
var RuntimeHistoryImporterCommon []byte

//go:embed runtime/history/importers/claude.ts
var RuntimeHistoryImporterClaude []byte

//go:embed runtime/history/importers/codex.ts
var RuntimeHistoryImporterCodex []byte

//go:embed runtime/history/importers/pi.ts
var RuntimeHistoryImporterPi []byte

//go:embed runtime/history/importers/opencode.ts
var RuntimeHistoryImporterOpenCode []byte

//go:embed runtime/tsconfig.json
var RuntimeTSConfig []byte

// Pi MCP bridge: pi has no MCP client, so Tandem launches pi through a wrapper
// that loads an extension exposing the session's MCP servers as pi tools.
//
//go:embed runtime/pi/mcp-bridge.ts
var RuntimePiMCPBridge []byte

//go:embed runtime/pi/tandem-pi
var RuntimePiWrapper []byte
