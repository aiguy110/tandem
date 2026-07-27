// Package runtimeinstall lazily provisions the Node ACP server packages
// Tandem launches per agent (and the Playwright MCP server), installing each
// on first spawn rather than eagerly at daemon boot.
package runtimeinstall

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	tandem "github.com/aiguy110/tandem"
	"github.com/aiguy110/tandem/internal/config"
	"github.com/aiguy110/tandem/internal/noderuntime"
)

// pin describes one npm package Tandem installs into Config.RuntimeRoot on
// demand, and the file whose presence indicates it is already installed.
type pin struct {
	spec string // npm install argument, e.g. "pkg@^1.2.3"
	dist string // path relative to RuntimeRoot that must exist once installed
}

// agentPins maps an agent id (config.Agent key / SpawnSpec.agent) to the ACP
// server package Tandem installs for it. Agents absent from this map are
// unmanaged/custom launches; EnsureAgent is a no-op for them.
var agentPins = map[string]pin{
	"claude": {spec: "@agentclientprotocol/claude-agent-acp@^0.59.0", dist: filepath.Join("node_modules", "@agentclientprotocol", "claude-agent-acp", "dist", "index.js")},
	"codex":  {spec: "@agentclientprotocol/codex-acp@^1.1.2", dist: filepath.Join("node_modules", "@agentclientprotocol", "codex-acp", "dist", "index.js")},
	"pi":     {spec: "pi-acp@^0.0.31", dist: filepath.Join("node_modules", "pi-acp", "dist", "index.js")},
}

// playwrightPin is the Playwright MCP server Tandem installs alongside an
// agent's ACP package when browser MCP tools are enabled.
var playwrightPin = pin{spec: "@playwright/mcp@^0.0.78", dist: filepath.Join("node_modules", "@playwright", "mcp", "cli.js")}

var historyPin = pin{spec: "tsx@4.23.1", dist: filepath.Join("node_modules", "tsx", "dist", "cli.mjs")}

// installMu serializes all installs into the shared RuntimeRoot node_modules;
// concurrent `npm install`s into the same node_modules can corrupt it.
var installMu sync.Mutex

// EnsureAgent installs the ACP server package (and, if enabled, the
// Playwright MCP server) that agentID needs before it can be spawned. It is
// a no-op for agent ids not in agentPins (custom/unmanaged agents launch
// arbitrary commands Tandem does not provision). Safe to call concurrently
// and repeatedly; installs are serialized and skipped once already present.
func EnsureAgent(ctx context.Context, cfg config.Config, agentID string, log io.Writer) error {
	p, ok := agentPins[agentID]
	if !ok {
		return nil
	}
	if ready(cfg, p) {
		return nil
	}

	installMu.Lock()
	defer installMu.Unlock()

	// Re-check now that we hold the lock: another goroutine may have just
	// finished installing this same agent's package.
	if ready(cfg, p) {
		return nil
	}

	if cfg.Node.Managed {
		if err := noderuntime.Ensure(ctx, cfg.Node.Root, cfg.Node.Version, log); err != nil {
			return fmt.Errorf("provision managed node: %w", err)
		}
	}

	npm, err := resolveNpm(cfg)
	if err != nil {
		return err
	}
	if err := ensureRuntimeRoot(cfg.RuntimeRoot); err != nil {
		return err
	}

	if err := install(ctx, cfg, npm, p.spec, log); err != nil {
		return err
	}
	if !distExists(cfg.RuntimeRoot, p.dist) {
		return fmt.Errorf("provision agent %s: npm completed without required ACP module", agentID)
	}

	if cfg.Browser.MCPEnabled && !distExists(cfg.RuntimeRoot, playwrightPin.dist) {
		if err := install(ctx, cfg, npm, playwrightPin.spec, log); err != nil {
			return err
		}
		if !distExists(cfg.RuntimeRoot, playwrightPin.dist) {
			return fmt.Errorf("provision playwright mcp: npm completed without required module")
		}
	}
	return nil
}

// EnsureHistory provisions the TypeScript execution dependency and the
// embedded, version-matched SDK/runner used by historyimport.Runner. It is
// explicit rather than daemon-startup work; lifecycle callers decide when an
// importer scan should incur provisioning.
func EnsureHistory(ctx context.Context, cfg config.Config, log io.Writer) error {
	installMu.Lock()
	defer installMu.Unlock()

	if cfg.Node.Managed {
		if err := noderuntime.Ensure(ctx, cfg.Node.Root, cfg.Node.Version, log); err != nil {
			return fmt.Errorf("provision managed node: %w", err)
		}
	}
	if err := ensureRuntimeRoot(cfg.RuntimeRoot); err != nil {
		return err
	}
	files := map[string][]byte{
		filepath.Join("history", "sdk.ts"):    tandem.RuntimeHistorySDK,
		filepath.Join("history", "runner.ts"): tandem.RuntimeHistoryRunner,
		filepath.Join("history", "importers", "common.ts"): tandem.RuntimeHistoryImporterCommon,
		filepath.Join("history", "importers", "claude.ts"): tandem.RuntimeHistoryImporterClaude,
		filepath.Join("history", "importers", "codex.ts"): tandem.RuntimeHistoryImporterCodex,
		filepath.Join("history", "importers", "pi.ts"): tandem.RuntimeHistoryImporterPi,
		filepath.Join("history", "importers", "opencode.ts"): tandem.RuntimeHistoryImporterOpenCode,
		"tsconfig.json":                       tandem.RuntimeTSConfig,
	}
	for rel, contents := range files {
		path := filepath.Join(cfg.RuntimeRoot, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("provision history runtime: %w", err)
		}
		if err := os.WriteFile(path, contents, 0o644); err != nil {
			return fmt.Errorf("provision history runtime: %w", err)
		}
	}
	if distExists(cfg.RuntimeRoot, historyPin.dist) {
		return nil
	}
	npm, err := resolveNpm(cfg)
	if err != nil {
		return err
	}
	if err := install(ctx, cfg, npm, historyPin.spec, log); err != nil {
		return err
	}
	if !distExists(cfg.RuntimeRoot, historyPin.dist) {
		return errors.New("provision history runtime: npm completed without tsx")
	}
	return nil
}

// ready reports whether agentID's package (and, when browser MCP is
// enabled, the Playwright MCP server) is already installed.
func ready(cfg config.Config, p pin) bool {
	if !distExists(cfg.RuntimeRoot, p.dist) {
		return false
	}
	if cfg.Browser.MCPEnabled && !distExists(cfg.RuntimeRoot, playwrightPin.dist) {
		return false
	}
	return true
}

func distExists(root, rel string) bool {
	info, err := os.Stat(filepath.Join(root, rel))
	return err == nil && !info.IsDir()
}

func resolveNpm(cfg config.Config) (string, error) {
	if cfg.Node.Npm != "" {
		if info, err := os.Stat(cfg.Node.Npm); err == nil && !info.IsDir() {
			return cfg.Node.Npm, nil
		}
	}
	npm, err := exec.LookPath("npm")
	if err != nil {
		return "", fmt.Errorf("provision runtime: npm is required: %w", err)
	}
	return npm, nil
}

func ensureRuntimeRoot(root string) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("provision runtime: %w", err)
	}
	pkgJSON := filepath.Join(root, "package.json")
	if _, err := os.Stat(pkgJSON); err == nil {
		return nil
	}
	minimal := []byte(`{"private":true,"name":"tandem-agent-runtime"}` + "\n")
	if err := os.WriteFile(pkgJSON, minimal, 0o644); err != nil {
		return fmt.Errorf("provision runtime package.json: %w", err)
	}
	return nil
}

// buildEnv copies the daemon's environment for the npm subprocess, prepending
// the managed Node's directory onto PATH when cfg.Node.Managed so npm's
// shebang and the child `node` calls it makes resolve the managed Node
// instead of whatever (if anything) is on the host PATH.
func buildEnv(cfg config.Config) []string {
	env := os.Environ()
	if !cfg.Node.Managed || cfg.Node.Command == "" {
		return env
	}
	nodeDir := filepath.Dir(cfg.Node.Command)
	out := make([]string, 0, len(env)+1)
	found := false
	for _, entry := range env {
		if len(entry) >= 5 && entry[:5] == "PATH=" {
			out = append(out, "PATH="+nodeDir+string(os.PathListSeparator)+entry[5:])
			found = true
			continue
		}
		out = append(out, entry)
	}
	if !found {
		out = append(out, "PATH="+nodeDir)
	}
	return out
}

func install(ctx context.Context, cfg config.Config, npm, spec string, log io.Writer) error {
	fmt.Fprintf(log, "tandem: installing %s in %s\n", spec, cfg.RuntimeRoot)
	// NOTE: deliberately no --no-save. Each package must be recorded in the
	// RuntimeRoot package.json so a later `npm install <other-agent>` does not
	// prune this agent's package as extraneous (verified: --no-save drops the
	// previously installed package from node_modules).
	cmd := exec.CommandContext(ctx, npm, "install", spec, "--omit=dev", "--no-audit", "--no-fund")
	cmd.Dir = cfg.RuntimeRoot
	cmd.Stdout = log
	cmd.Stderr = log
	cmd.Env = buildEnv(cfg)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("provision %s with npm install: %w", spec, err)
	}
	return nil
}
