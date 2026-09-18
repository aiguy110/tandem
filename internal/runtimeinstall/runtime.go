// Package runtimeinstall lazily provisions the Node ACP server packages
// Tandem launches per agent (and the Playwright MCP server), installing each
// on first spawn rather than eagerly at daemon boot.
package runtimeinstall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	tandem "github.com/aiguy110/tandem"
	"github.com/aiguy110/tandem/internal/agentadapter"
	"github.com/aiguy110/tandem/internal/config"
	"github.com/aiguy110/tandem/internal/noderuntime"
)

// pin describes one npm package Tandem installs into Config.RuntimeRoot on
// demand, and the file whose presence indicates it is already installed.
type pin struct {
	packageName string
	spec        string // npm install argument, e.g. "pkg@^1.2.3"
	dist        string // path relative to RuntimeRoot that must exist once installed
}

// agentPins maps an agent id (config.Agent key / SpawnSpec.agent) to the ACP
// server package Tandem installs for it. Agents absent from this map are
// unmanaged/custom launches; EnsureAgent is a no-op for them.
var agentPins = map[string]pin{
	"claude": {packageName: "@agentclientprotocol/claude-agent-acp", spec: "@agentclientprotocol/claude-agent-acp@^0.70.0", dist: filepath.Join("node_modules", "@agentclientprotocol", "claude-agent-acp", "dist", "index.js")},
	"codex":  {packageName: "@agentclientprotocol/codex-acp", spec: "@agentclientprotocol/codex-acp@^1.8.0", dist: filepath.Join("node_modules", "@agentclientprotocol", "codex-acp", "dist", "index.js")},
	"pi":     {packageName: "pi-acp", spec: "pi-acp@^0.0.31", dist: filepath.Join("node_modules", "pi-acp", "dist", "index.js")},
}

// playwrightPin is the Playwright MCP server Tandem installs alongside an
// agent's ACP package when browser MCP tools are enabled.
var playwrightPin = pin{spec: "@playwright/mcp@^0.0.78", dist: filepath.Join("node_modules", "@playwright", "mcp", "cli.js")}

var historyPin = pin{spec: "tsx@4.23.1", dist: filepath.Join("node_modules", "tsx", "dist", "cli.mjs")}

// installMu serializes all installs into the shared RuntimeRoot node_modules;
// concurrent `npm install`s into the same node_modules can corrupt it.
var installMu sync.Mutex

const lockFileName = "agents.lock.json"

type LockedAgent struct {
	Package         string `json:"package"`
	Constraint      string `json:"constraint"`
	Version         string `json:"version"`
	Integrity       string `json:"integrity,omitempty"`
	Path            string `json:"path"`
	PreviousVersion string `json:"previousVersion,omitempty"`
	PreviousPath    string `json:"previousPath,omitempty"`
}
type Lockfile struct {
	Version int                    `json:"version"`
	Agents  map[string]LockedAgent `json:"agents"`
}

func ManagedAgents() map[string]LockedAgent {
	out := make(map[string]LockedAgent, len(agentPins))
	for id, p := range agentPins {
		out[id] = LockedAgent{Package: p.packageName, Constraint: strings.TrimPrefix(p.spec, p.packageName+"@")}
	}
	return out
}

func ReadLock(root string) (Lockfile, error) {
	l := Lockfile{Version: 1, Agents: map[string]LockedAgent{}}
	b, err := os.ReadFile(filepath.Join(root, lockFileName))
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return l, err
	}
	if err := json.Unmarshal(b, &l); err != nil {
		return l, fmt.Errorf("read agent lockfile: %w", err)
	}
	if l.Agents == nil {
		l.Agents = map[string]LockedAgent{}
	}
	return l, nil
}

func writeLock(root string, l Lockfile) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(root, ".agents-lock-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err = tmp.Write(b); err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, filepath.Join(root, lockFileName))
}

// EnsureManaged installs (once) and pins a managed adapter for a newly-created
// session. Existing sessions already carrying a Distribution never call this.
func EnsureManaged(ctx context.Context, cfg config.Config, agent string, log io.Writer) (*agentadapter.Distribution, error) {
	p, ok := agentPins[agent]
	if !ok {
		return nil, nil
	}
	installMu.Lock()
	defer installMu.Unlock()
	l, err := ReadLock(cfg.RuntimeRoot)
	if err != nil {
		return nil, err
	}
	locked, ok := l.Agents[agent]
	if !ok || !distExists(locked.Path, p.dist) {
		if cfg.Node.Managed {
			if err := noderuntime.Ensure(ctx, cfg.Node.Root, cfg.Node.Version, log); err != nil {
				return nil, err
			}
		}
		npm, err := resolveNpm(cfg)
		if err != nil {
			return nil, err
		}
		// Ask npm for the concrete version selected by Tandem's compatibility range.
		version, err := npmView(ctx, cfg, npm, p.spec, "version")
		if err != nil {
			return nil, err
		}
		integrity, _ := npmView(ctx, cfg, npm, p.packageName+"@"+version, "dist.integrity")
		root := filepath.Join(cfg.RuntimeRoot, "agents", agent, version)
		if !distExists(root, p.dist) {
			if err := ensureRuntimeRoot(root); err != nil {
				return nil, err
			}
			if err := installAt(ctx, cfg, npm, root, p.packageName+"@"+version, log); err != nil {
				return nil, err
			}
		}
		if !distExists(root, p.dist) {
			return nil, fmt.Errorf("provision agent %s: npm completed without required ACP module", agent)
		}
		locked = LockedAgent{Package: p.packageName, Constraint: strings.TrimPrefix(p.spec, p.packageName+"@"), Version: version, Integrity: integrity, Path: root}
		l.Agents[agent] = locked
		if err := writeLock(cfg.RuntimeRoot, l); err != nil {
			return nil, err
		}
	}
	return &agentadapter.Distribution{Source: "npm", Package: locked.Package, Version: locked.Version, Integrity: locked.Integrity, Path: locked.Path}, nil
}

func npmView(ctx context.Context, cfg config.Config, npm, spec, field string) (string, error) {
	values, err := npmViewValues(ctx, cfg, npm, spec, field)
	if err != nil {
		return "", err
	}
	return values[len(values)-1], nil
}

func npmViewValues(ctx context.Context, cfg config.Config, npm, spec, field string) ([]string, error) {
	cmd := exec.CommandContext(ctx, npm, "view", spec, field, "--json")
	cmd.Env = buildEnv(cfg)
	b, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %w", spec, err)
	}
	var values []string
	var value string
	if json.Unmarshal(b, &value) == nil {
		values = []string{value}
	} else if json.Unmarshal(b, &values) != nil {
		values = []string{strings.Trim(strings.TrimSpace(string(b)), "\"")}
	}
	if len(values) == 0 || values[len(values)-1] == "" {
		return nil, fmt.Errorf("inspect %s: npm returned no %s", spec, field)
	}
	return values, nil
}

func installAt(ctx context.Context, cfg config.Config, npm, root, spec string, log io.Writer) error {
	fmt.Fprintf(log, "tandem: installing managed ACP distribution %s in %s\n", spec, root)
	cmd := exec.CommandContext(ctx, npm, "install", spec, "--omit=dev", "--no-audit", "--no-fund", "--save-exact")
	cmd.Dir = root
	cmd.Stdout = log
	cmd.Stderr = log
	cmd.Env = buildEnv(cfg)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("provision %s with npm install: %w", spec, err)
	}
	return nil
}

func EntryPoint(agent string, d *agentadapter.Distribution) (string, bool) {
	p, ok := agentPins[agent]
	if !ok || d == nil {
		return "", false
	}
	return filepath.Join(d.Path, p.dist), true
}

type UpdateInfo struct{ Agent, Package, Constraint, CurrentVersion, LatestVersion string }

type AdapterStatus struct {
	Agent             string   `json:"agent"`
	Package           string   `json:"package"`
	Constraint        string   `json:"constraint"`
	CurrentVersion    string   `json:"currentVersion"`
	InstalledVersions []string `json:"installedVersions"`
	AvailableVersions []string `json:"availableVersions"`
}

func AdapterCatalog(ctx context.Context, cfg config.Config) ([]AdapterStatus, error) {
	installMu.Lock()
	defer installMu.Unlock()
	l, err := ReadLock(cfg.RuntimeRoot)
	if err != nil {
		return nil, err
	}
	npm, err := resolveNpm(cfg)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(agentPins))
	for id := range agentPins {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]AdapterStatus, 0, len(ids))
	for _, agent := range ids {
		p := agentPins[agent]
		versions, err := npmViewValues(ctx, cfg, npm, p.spec, "version")
		if err != nil {
			return nil, fmt.Errorf("list %s versions: %w", agent, err)
		}
		sortVersions(versions)
		locked := l.Agents[agent]
		current := locked.Version
		if current == "" {
			current, _ = installedVersion(cfg.RuntimeRoot, p)
		}
		installed := installedVersions(cfg.RuntimeRoot, agent, p)
		if current != "" && !slices.Contains(installed, current) {
			installed = append(installed, current)
			sortVersions(installed)
		}
		out = append(out, AdapterStatus{Agent: agent, Package: p.packageName, Constraint: strings.TrimPrefix(p.spec, p.packageName+"@"), CurrentVersion: current, InstalledVersions: installed, AvailableVersions: versions})
	}
	return out, nil
}

func installedVersions(root, agent string, p pin) []string {
	entries, err := os.ReadDir(filepath.Join(root, "agents", agent))
	if err != nil {
		return nil
	}
	var out []string
	for _, entry := range entries {
		if entry.IsDir() && distExists(filepath.Join(root, "agents", agent, entry.Name()), p.dist) {
			out = append(out, entry.Name())
		}
	}
	sortVersions(out)
	return out
}
func sortVersions(values []string) {
	sort.Slice(values, func(i, j int) bool { return semverNewer(values[i], values[j]) })
}

// CheckUpdates asks npm for the newest version allowed by Tandem's declared
// compatibility range. It never mutates the runtime.
func CheckUpdates(ctx context.Context, cfg config.Config) ([]UpdateInfo, error) {
	if os.Getenv("TANDEM_NO_UPDATE_CHECK") != "" {
		return nil, nil
	}
	installMu.Lock()
	defer installMu.Unlock()
	l, err := ReadLock(cfg.RuntimeRoot)
	if err != nil {
		return nil, err
	}
	npm, err := resolveNpm(cfg)
	if err != nil {
		return nil, err
	}
	var out []UpdateInfo
	for agent, p := range agentPins {
		locked, ok := l.Agents[agent]
		if !ok || locked.Version == "" {
			if version, found := installedVersion(cfg.RuntimeRoot, p); found {
				locked = LockedAgent{Package: p.packageName, Constraint: strings.TrimPrefix(p.spec, p.packageName+"@"), Version: version, Path: cfg.RuntimeRoot}
			} else {
				continue
			}
		}
		latest, err := npmView(ctx, cfg, npm, p.spec, "version")
		if err != nil {
			return nil, fmt.Errorf("check %s update: %w", agent, err)
		}
		if semverNewer(latest, locked.Version) {
			out = append(out, UpdateInfo{Agent: agent, Package: p.packageName, Constraint: strings.TrimPrefix(p.spec, p.packageName+"@"), CurrentVersion: locked.Version, LatestVersion: latest})
		}
	}
	return out, nil
}

func semverNewer(candidate, current string) bool {
	parse := func(v string) ([3]int, string, bool) {
		var n [3]int
		v = strings.TrimPrefix(v, "v")
		parts := strings.SplitN(v, "-", 2)
		core := strings.Split(parts[0], ".")
		if len(core) != 3 {
			return n, "", false
		}
		for i := range core {
			value, err := strconv.Atoi(core[i])
			if err != nil {
				return n, "", false
			}
			n[i] = value
		}
		pre := ""
		if len(parts) == 2 {
			pre = parts[1]
		}
		return n, pre, true
	}
	a, ap, aok := parse(candidate)
	b, bp, bok := parse(current)
	if !aok || !bok {
		return false
	}
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return ap == "" && bp != ""
}

// InstallUpdate installs a selected compatible version beside all existing
// versions, then atomically changes the preferred resolution for new sessions.
func InstallUpdate(ctx context.Context, cfg config.Config, agent, version string, log io.Writer) (LockedAgent, error) {
	installMu.Lock()
	defer installMu.Unlock()
	p, ok := agentPins[agent]
	if !ok {
		return LockedAgent{}, fmt.Errorf("agent %s is not managed", agent)
	}
	l, err := ReadLock(cfg.RuntimeRoot)
	if err != nil {
		return LockedAgent{}, err
	}
	current, ok := l.Agents[agent]
	if !ok {
		if installed, found := installedVersion(cfg.RuntimeRoot, p); found {
			current = LockedAgent{Package: p.packageName, Constraint: strings.TrimPrefix(p.spec, p.packageName+"@"), Version: installed, Path: cfg.RuntimeRoot}
		} else {
			return LockedAgent{}, fmt.Errorf("agent %s is not installed", agent)
		}
	}
	npm, err := resolveNpm(cfg)
	if err != nil {
		return LockedAgent{}, err
	}
	allowed, err := npmViewValues(ctx, cfg, npm, p.spec, "version")
	if err != nil {
		return LockedAgent{}, err
	}
	if !slices.Contains(allowed, version) {
		return LockedAgent{}, fmt.Errorf("version %s is outside Tandem's compatible range %s", version, strings.TrimPrefix(p.spec, p.packageName+"@"))
	}
	// Adopt a legacy shared-node_modules install into an immutable directory
	// before replacing it, so rollback and old session restoration cannot be
	// affected by a future mutation of the shared runtime.
	if current.Path == cfg.RuntimeRoot {
		oldRoot := filepath.Join(cfg.RuntimeRoot, "agents", agent, current.Version)
		if !distExists(oldRoot, p.dist) {
			if err := ensureRuntimeRoot(oldRoot); err != nil {
				return LockedAgent{}, err
			}
			if err := installAt(ctx, cfg, npm, oldRoot, p.packageName+"@"+current.Version, log); err != nil {
				return LockedAgent{}, err
			}
		}
		current.Path = oldRoot
	}
	root := filepath.Join(cfg.RuntimeRoot, "agents", agent, version)
	if !distExists(root, p.dist) {
		if err := ensureRuntimeRoot(root); err != nil {
			return LockedAgent{}, err
		}
		if err := installAt(ctx, cfg, npm, root, p.packageName+"@"+version, log); err != nil {
			return LockedAgent{}, err
		}
	}
	if !distExists(root, p.dist) {
		return LockedAgent{}, fmt.Errorf("update agent %s: required entry point is missing", agent)
	}
	integrity, _ := npmView(ctx, cfg, npm, p.packageName+"@"+version, "dist.integrity")
	next := LockedAgent{Package: p.packageName, Constraint: strings.TrimPrefix(p.spec, p.packageName+"@"), Version: version, Integrity: integrity, Path: root, PreviousVersion: current.Version, PreviousPath: current.Path}
	l.Agents[agent] = next
	if err := writeLock(cfg.RuntimeRoot, l); err != nil {
		return LockedAgent{}, err
	}
	return next, nil
}

func installedVersion(root string, p pin) (string, bool) {
	packageDir := filepath.Dir(filepath.Dir(filepath.Join(root, p.dist)))
	// Scoped packages have one additional directory between node_modules and package.
	if strings.HasPrefix(p.packageName, "@") {
		packageDir = filepath.Dir(filepath.Dir(filepath.Join(root, p.dist)))
	}
	b, err := os.ReadFile(filepath.Join(packageDir, "package.json"))
	if err != nil {
		return "", false
	}
	var manifest struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(b, &manifest) != nil || manifest.Version == "" {
		return "", false
	}
	return manifest.Version, true
}

func Rollback(ctx context.Context, cfg config.Config, agent string) (LockedAgent, error) {
	installMu.Lock()
	defer installMu.Unlock()
	l, err := ReadLock(cfg.RuntimeRoot)
	if err != nil {
		return LockedAgent{}, err
	}
	current, ok := l.Agents[agent]
	if !ok || current.PreviousVersion == "" {
		return LockedAgent{}, fmt.Errorf("agent %s has no rollback version", agent)
	}
	p := agentPins[agent]
	if !distExists(current.PreviousPath, p.dist) {
		return LockedAgent{}, fmt.Errorf("rollback distribution %s is missing", current.PreviousVersion)
	}
	previous := LockedAgent{Package: current.Package, Constraint: current.Constraint, Version: current.PreviousVersion, Path: current.PreviousPath, PreviousVersion: current.Version, PreviousPath: current.Path}
	l.Agents[agent] = previous
	if err := writeLock(cfg.RuntimeRoot, l); err != nil {
		return LockedAgent{}, err
	}
	return previous, nil
}

// EnsureAgent installs the ACP server package (and, if enabled, the
// Playwright MCP server) that sessionID needs before it can be spawned. It is
// a no-op for agent ids not in agentPins (custom/unmanaged agents launch
// arbitrary commands Tandem does not provision). Safe to call concurrently
// and repeatedly; installs are serialized and skipped once already present.
func EnsureAgent(ctx context.Context, cfg config.Config, sessionID string, log io.Writer) error {
	p, ok := agentPins[sessionID]
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
		return fmt.Errorf("provision agent %s: npm completed without required ACP module", sessionID)
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
		filepath.Join("history", "sdk.ts"):                   tandem.RuntimeHistorySDK,
		filepath.Join("history", "runner.ts"):                tandem.RuntimeHistoryRunner,
		filepath.Join("history", "importers", "common.ts"):   tandem.RuntimeHistoryImporterCommon,
		filepath.Join("history", "importers", "claude.ts"):   tandem.RuntimeHistoryImporterClaude,
		filepath.Join("history", "importers", "codex.ts"):    tandem.RuntimeHistoryImporterCodex,
		filepath.Join("history", "importers", "pi.ts"):       tandem.RuntimeHistoryImporterPi,
		filepath.Join("history", "importers", "opencode.ts"): tandem.RuntimeHistoryImporterOpenCode,
		"tsconfig.json": tandem.RuntimeTSConfig,
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

// ready reports whether sessionID's package (and, when browser MCP is
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
