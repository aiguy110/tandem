// Package runtimeinstall lazily provisions the Node ACP server packages
// Tandem launches per agent (and the Playwright MCP server), installing each
// on first spawn rather than eagerly at daemon boot.
package runtimeinstall

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
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
	// NOTE: tilde, not caret. npm reads ^0.0.x as exactly 0.0.x, so a caret here
	// would make this range a single version and the update check inert.
	"pi": {packageName: "pi-acp", spec: "pi-acp@~0.0.33", dist: filepath.Join("node_modules", "pi-acp", "dist", "index.js")},
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
	// Fork is set when this distribution was installed from a tracked fork
	// rather than from the published package. See fork.go.
	Fork *LockedFork `json:"fork,omitempty"`
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
	fork, forked, err := ForkFor(cfg, agent)
	if err != nil {
		return nil, err
	}
	locked, ok := l.Agents[agent]
	// A fork-tracked agent resolves against the pinned commit, so a re-pinned
	// record (or a stale upstream install left over from before tracking began)
	// must provision the fork tree rather than reuse what the lockfile names.
	if forked && !forkLockCurrent(locked, fork, p.dist) {
		if cfg.Node.Managed {
			if err := noderuntime.Ensure(ctx, cfg.Node.Root, cfg.Node.Version, log); err != nil {
				return nil, err
			}
		}
		npm, err := resolveNpm(cfg)
		if err != nil {
			return nil, err
		}
		next, err := installFork(ctx, cfg, npm, agent, fork, log)
		if err != nil {
			return nil, err
		}
		next.PreviousVersion, next.PreviousPath = locked.Version, locked.Path
		l.Agents[agent] = next
		if err := writeLock(cfg.RuntimeRoot, l); err != nil {
			return nil, err
		}
		return forkDistribution(next), nil
	}
	if forked {
		return forkDistribution(locked), nil
	}
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

// publishedVersions lists every version of a package, newest first. It asks for
// the package's whole `versions` array rather than resolving a range, so Tandem
// decides for itself what is newest and what is compatible; see semver.go for
// why delegating that to `npm view <pkg>@<range>` is unsound.
func publishedVersions(ctx context.Context, cfg config.Config, npm, pkg string) ([]string, error) {
	values, err := npmViewValues(ctx, cfg, npm, pkg, "versions")
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("inspect %s: npm returned no versions", pkg)
	}
	sortVersions(out)
	return out, nil
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

// UpdateKind distinguishes the two things a newer upstream release can mean.
const (
	// UpdateKindRelease is a plain published upgrade: install it.
	UpdateKindRelease = "release"
	// UpdateKindRebase is a new upstream release for an agent whose ACP server
	// Tandem carries as a fork. It cannot be installed directly without losing
	// the fork's change, so it is offered as a rebase for an agent to perform.
	UpdateKindRebase = "rebase"
)

type UpdateInfo struct {
	Agent, Package, Constraint, CurrentVersion, LatestVersion string
	// Compatible reports whether LatestVersion satisfies Constraint. Tandem
	// offers updates optimistically, so an offer may deliberately point beyond
	// the range Tandem has been tested against; this is how the notification and
	// the UI know to say so.
	Compatible bool
	// NewestPublished is the newest release on npm when it is newer than the
	// version being offered, i.e. when the offer was held back to stay inside
	// Constraint. Empty when the offer already is the newest release.
	NewestPublished string
	// Kind is UpdateKindRelease or UpdateKindRebase.
	Kind string
	// Fork carries the tracking record when Kind is UpdateKindRebase, so the
	// notification and the rebase agent have the repo, ref and reason to hand.
	Fork *config.ACPFork
}

type AdapterStatus struct {
	Agent             string   `json:"agent"`
	Package           string   `json:"package"`
	Constraint        string   `json:"constraint"`
	CurrentVersion    string   `json:"currentVersion"`
	InstalledVersions []string `json:"installedVersions"`
	// AvailableVersions is every published version, newest first — not only the
	// ones inside Constraint. Tandem is optimistic about updates: a newer release
	// is offered even when it falls outside the range Tandem was tested against,
	// because a stale pin should not hide a fix.
	AvailableVersions []string `json:"availableVersions"`
	// CompatibleVersions is the subset of AvailableVersions satisfying
	// Constraint, so the UI can mark everything else as beyond the tested range.
	CompatibleVersions []string `json:"compatibleVersions"`
	// LatestVersion is the newest published *release*. AvailableVersions can lead
	// with a prerelease (1.12.1-preview.1 outranks 1.12.0 in semver), and an
	// "update to latest" action must never mean "move onto a preview", so this is
	// the version such an action should offer.
	LatestVersion string `json:"latestVersion,omitempty"`
	// LatestCompatibleVersion is the newest release inside Constraint, present
	// only when it differs from LatestVersion — i.e. when the newest release is
	// beyond the range Tandem has been tested against.
	LatestCompatibleVersion string `json:"latestCompatibleVersion,omitempty"`
	// Fork is set when this agent's ACP server is tracked as a fork. While it is
	// set, the published versions above are not what runs: they are what the
	// agent would return to once the fork is retired.
	Fork *ForkStatus `json:"fork,omitempty"`
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
	forks, err := config.LoadACPForks(cfg.Home)
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
		constraint := strings.TrimPrefix(p.spec, p.packageName+"@")
		versions, err := publishedVersions(ctx, cfg, npm, p.packageName)
		if err != nil {
			return nil, fmt.Errorf("list %s versions: %w", agent, err)
		}
		compatible := make([]string, 0, len(versions))
		for _, v := range versions {
			if satisfies(v, constraint) {
				compatible = append(compatible, v)
			}
		}
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
		row := AdapterStatus{Agent: agent, Package: p.packageName, Constraint: constraint, CurrentVersion: current, InstalledVersions: installed, AvailableVersions: versions, CompatibleVersions: compatible}
		if latest, ok := newestPublished(versions); ok {
			row.LatestVersion = latest
			if inRange, ok := newestInRange(compatible, constraint); ok && inRange != latest {
				row.LatestCompatibleVersion = inRange
			}
		}
		if fork, tracked := forks[agent]; tracked {
			status := &ForkStatus{
				Repo: fork.Repo, Ref: fork.Ref, Commit: fork.Commit, ShortCommit: fork.ShortCommit(),
				Clone:           config.ForkCloneDir(cfg.RuntimeRoot, agent, fork),
				UpstreamPackage: fork.UpstreamPackage, UpstreamRepo: fork.UpstreamRepo,
				UpstreamVersion: fork.UpstreamVersion, Reason: fork.Reason, UpstreamPRs: fork.UpstreamPRs,
				Installed: forkLockCurrent(locked, fork, p.dist),
			}
			// Newest published release, so the UI can say whether the fork is
			// behind without a second round trip. Only set when it is actually
			// ahead of the fork's baseline.
			if newest, ok := newestPublished(versions); ok && semverNewer(newest, fork.UpstreamVersion) {
				status.LatestUpstreamVersion = newest
			}
			row.CurrentVersion = fork.DistributionVersion()
			row.Fork = status
		}
		out = append(out, row)
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
		// Fork builds live beside published versions but are not points on the
		// published version line: offering one as a selectable version would
		// produce a choice InstallUpdate must then refuse. They are reported
		// through AdapterStatus.Fork instead.
		if entry.IsDir() && !config.IsForkVersion(entry.Name()) && distExists(filepath.Join(root, "agents", agent, entry.Name()), p.dist) {
			out = append(out, entry.Name())
		}
	}
	sortVersions(out)
	return out
}

// CheckUpdates looks for newer published versions of each managed ACP server.
// It never mutates the runtime.
//
// It is deliberately optimistic. A newer release is reported even when it falls
// outside the agent's declared compatibility range, because that range records
// what Tandem has been *tested* against and goes stale the moment upstream
// publishes; treating it as a ceiling means a stale pin silently hides every
// later fix. Where a compatible update exists it is the one offered, and the
// newer out-of-range release is reported alongside it so the operator can see
// both. UpdateInfo.Compatible says which case an offer is.
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
	forks, err := config.LoadACPForks(cfg.Home)
	if err != nil {
		return nil, err
	}
	var out []UpdateInfo
	for agent, p := range agentPins {
		// A fork-tracked agent is compared against the release its fork sits on,
		// not against the installed distribution: the installed version is a
		// fork build and is not a point on the published version line.
		constraint := strings.TrimPrefix(p.spec, p.packageName+"@")
		if fork, tracked := forks[agent]; tracked {
			// A fork-tracked agent is compared against the release its fork sits
			// on, not the installed distribution: the installed version is a fork
			// build and is not a point on the published version line.
			versions, err := publishedVersions(ctx, cfg, npm, fork.UpstreamPackage)
			if err != nil {
				return nil, fmt.Errorf("check %s upstream release: %w", agent, err)
			}
			latest, ok := newestPublished(versions)
			if ok && semverNewer(latest, fork.UpstreamVersion) {
				record := fork
				out = append(out, UpdateInfo{Agent: agent, Package: fork.UpstreamPackage, Constraint: constraint,
					CurrentVersion: fork.UpstreamVersion, LatestVersion: latest, Compatible: satisfies(latest, constraint),
					Kind: UpdateKindRebase, Fork: &record})
			}
			continue
		}
		locked, ok := l.Agents[agent]
		if !ok || locked.Version == "" {
			if version, found := installedVersion(cfg.RuntimeRoot, p); found {
				locked = LockedAgent{Package: p.packageName, Constraint: constraint, Version: version, Path: cfg.RuntimeRoot}
			} else {
				continue
			}
		}
		versions, err := publishedVersions(ctx, cfg, npm, p.packageName)
		if err != nil {
			return nil, fmt.Errorf("check %s update: %w", agent, err)
		}
		update, ok := pickUpdate(agent, p.packageName, constraint, locked.Version, versions)
		if ok {
			out = append(out, update)
		}
	}
	return out, nil
}

// pickUpdate applies Tandem's optimistic update policy to one agent's published
// version list, returning the offer to make (if any).
//
// A compatible update is preferred when one exists, since it is the safer move
// and is still an upgrade. Only when the range admits nothing newer does the
// offer reach past it — that is the case a conservative check would drop on the
// floor, leaving the operator on a stale version with no signal at all.
func pickUpdate(agent, pkg, constraint, current string, versions []string) (UpdateInfo, bool) {
	newest, haveNewest := newestPublished(versions)
	inRange, haveInRange := newestInRange(versions, constraint)

	offer, compatible := "", false
	switch {
	case haveInRange && semverNewer(inRange, current):
		offer, compatible = inRange, true
	case haveNewest && semverNewer(newest, current):
		offer, compatible = newest, false
	default:
		return UpdateInfo{}, false
	}

	info := UpdateInfo{Agent: agent, Package: pkg, Constraint: constraint,
		CurrentVersion: current, LatestVersion: offer, Compatible: compatible, Kind: UpdateKindRelease}
	// Mention a still-newer out-of-range release only when the offer was held
	// back to stay compatible; otherwise the offer already is the newest.
	if haveNewest && semverNewer(newest, offer) {
		info.NewestPublished = newest
	}
	return info, true
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
	// Installing a published version over a tracked fork would silently drop the
	// change the fork carries. Retiring a fork is a deliberate act with its own
	// entry point, so point the caller at it instead of guessing.
	if current.Fork != nil {
		forks, forkErr := config.LoadACPForks(cfg.Home)
		if forkErr != nil {
			return LockedAgent{}, forkErr
		}
		if _, tracked := forks[agent]; tracked {
			return LockedAgent{}, fmt.Errorf("agent %s tracks the ACP fork %s (%s); rebase it, or run 'tandem acp upstream %s' to return to the published package",
				agent, current.Fork.Repo, current.Fork.Ref, agent)
		}
	}
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
	published, err := publishedVersions(ctx, cfg, npm, p.packageName)
	if err != nil {
		return LockedAgent{}, err
	}
	// The version must exist, but it need not be inside Tandem's declared range.
	// Refusing an out-of-range version would make the optimistic update offer
	// unusable, and the range is a record of what has been tested rather than a
	// hard compatibility boundary. Installing beside the current version and
	// leaving Rollback available is what makes this safe to allow.
	if !slices.Contains(published, version) {
		return LockedAgent{}, fmt.Errorf("%s has no published version %s", p.packageName, version)
	}
	constraint := strings.TrimPrefix(p.spec, p.packageName+"@")
	if !satisfies(version, constraint) {
		slog.Warn("installing ACP adapter beyond tested compatibility range",
			"agent", agent, "package", p.packageName, "version", version, "tested_range", constraint,
			"previous_version", current.Version)
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

// agentSupportFiles are embedded helper files ACP agent launches reference
// from RuntimeRoot (see the pi entry in config.yml.example).
var agentSupportFiles = []struct {
	rel      string
	contents []byte
	mode     os.FileMode
}{
	{filepath.Join("pi", "mcp-bridge.ts"), tandem.RuntimePiMCPBridge, 0o644},
	{filepath.Join("pi", "tandem-pi"), tandem.RuntimePiWrapper, 0o755},
}

// StageAgentSupport writes the embedded agent helper files into RuntimeRoot.
// Files already matching the embedded copy are left untouched; changed ones are
// replaced atomically so a concurrently launching agent never execs or loads a
// partially written file.
func StageAgentSupport(cfg config.Config) error {
	for _, f := range agentSupportFiles {
		path := filepath.Join(cfg.RuntimeRoot, f.rel)
		if current, err := os.ReadFile(path); err == nil && bytes.Equal(current, f.contents) {
			if info, err := os.Stat(path); err == nil && info.Mode().Perm() == f.mode {
				continue
			}
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("stage agent support: %w", err)
		}
		tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
		if err != nil {
			return fmt.Errorf("stage agent support: %w", err)
		}
		_, werr := tmp.Write(f.contents)
		cerr := tmp.Close()
		if werr == nil {
			werr = cerr
		}
		if werr == nil {
			werr = os.Chmod(tmp.Name(), f.mode)
		}
		if werr == nil {
			werr = os.Rename(tmp.Name(), path)
		}
		if werr != nil {
			os.Remove(tmp.Name())
			return fmt.Errorf("stage agent support %s: %w", f.rel, werr)
		}
		slog.Info("staged agent support file", "path", path)
	}
	return nil
}
