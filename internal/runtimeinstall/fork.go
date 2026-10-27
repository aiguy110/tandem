package runtimeinstall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/aiguy110/tandem/internal/agentadapter"
	"github.com/aiguy110/tandem/internal/config"
)

// LockedFork records which fork tree a distribution was built from, so the
// daemon can tell a fork install apart from a published one without re-reading
// config, and can detect that the operator has since re-pinned the fork.
type LockedFork struct {
	Repo            string `json:"repo"`
	Ref             string `json:"ref"`
	Commit          string `json:"commit"`
	UpstreamVersion string `json:"upstreamVersion"`
	// PackageDir and BuildScript are the build recipe; changing either must
	// rebuild even at the same commit.
	PackageDir  string `json:"packageDir,omitempty"`
	BuildScript string `json:"buildScript,omitempty"`
}

// ForkStatus is the fork-tracking view of one managed agent, reported to the UI
// and the CLI.
type ForkStatus struct {
	Repo            string `json:"repo"`
	Ref             string `json:"ref"`
	Commit          string `json:"commit"`
	ShortCommit     string `json:"shortCommit"`
	Clone           string `json:"clone"`
	UpstreamPackage string `json:"upstreamPackage"`
	UpstreamRepo    string `json:"upstreamRepo,omitempty"`
	// UpstreamVersion is the published release the fork is rebased onto.
	UpstreamVersion string `json:"upstreamVersion"`
	// LatestUpstreamVersion is the newest published release. When it differs
	// from UpstreamVersion the fork is behind and a rebase is offered.
	LatestUpstreamVersion string `json:"latestUpstreamVersion,omitempty"`
	Reason                string `json:"reason,omitempty"`
	UpstreamPRs           []int  `json:"upstreamPrs,omitempty"`
	// PackageDir and BuildScript are how the fork is built; see config.ACPFork.
	PackageDir  string `json:"packageDir,omitempty"`
	BuildScript string `json:"buildScript,omitempty"`
	// Installed reports whether the pinned commit is the one currently
	// installed. False means the record was re-pinned but no session has
	// spawned since, so the new tree is not built yet.
	Installed bool `json:"installed"`
}

// ForkFor returns the fork record for agent, if the operator has declared one.
// It reads config.yml on each call so `tandem acp` affects new sessions without
// a daemon restart, matching config.LoadMCPServers.
func ForkFor(cfg config.Config, agent string) (config.ACPFork, bool, error) {
	if _, managed := agentPins[agent]; !managed {
		return config.ACPFork{}, false, nil
	}
	forks, err := config.LoadACPForks(cfg.Home)
	if err != nil {
		return config.ACPFork{}, false, err
	}
	fork, ok := forks[agent]
	return fork, ok, nil
}

// installFork provisions a fork-pinned distribution and returns its lock entry.
// Callers must hold installMu.
func installFork(ctx context.Context, cfg config.Config, bun, agent string, fork config.ACPFork, log io.Writer) (LockedAgent, error) {
	p := agentPins[agent]
	version := fork.DistributionVersion()
	root := filepath.Join(cfg.RuntimeRoot, "agents", agent, version)
	recipe := forkRecipe{PackageDir: fork.PackageDir, BuildScript: fork.BuildScript}
	if distExists(root, p.dist) && readForkRecipe(root) != recipe {
		// The directory is named for the commit alone, so a record re-pinned to
		// the same commit with a corrected recipe would otherwise keep the tree
		// the old recipe produced. Start over rather than layer builds.
		slog.Info("rebuilding ACP fork for a changed build recipe", "agent", agent, "dir", root,
			"package_dir", fork.PackageDir, "build_script", fork.BuildScript)
		if err := os.RemoveAll(root); err != nil {
			return LockedAgent{}, err
		}
	}
	if !distExists(root, p.dist) {
		if err := ensureRuntimeRoot(root); err != nil {
			return LockedAgent{}, err
		}
		tarball, err := buildFork(ctx, cfg, bun, root, fork, log)
		if err != nil {
			return LockedAgent{}, fmt.Errorf("provision agent %s from fork %s#%s: %w", agent, fork.Repo, fork.ShortCommit(), err)
		}
		if err := installAt(ctx, cfg, bun, root, tarball, log); err != nil {
			return LockedAgent{}, err
		}
		if err := writeForkRecipe(root, recipe); err != nil {
			return LockedAgent{}, err
		}
	}
	if !distExists(root, p.dist) {
		// Most likely the fork renamed the package or stopped building dist/;
		// the install reports success either way, so say what to check.
		return LockedAgent{}, fmt.Errorf("provision agent %s from fork %s#%s: %s is missing after install (the fork must keep package name %q and its build — packageDir %q, buildScript %q — must produce %s)",
			agent, fork.Repo, fork.ShortCommit(), p.dist, p.packageName, fork.PackageDir, fork.BuildScript, p.dist)
	}
	return LockedAgent{
		Package:    p.packageName,
		Constraint: strings.TrimPrefix(p.spec, p.packageName+"@"),
		Version:    version,
		Path:       root,
		Fork: &LockedFork{Repo: fork.Repo, Ref: fork.Ref, Commit: fork.Commit, UpstreamVersion: fork.UpstreamVersion,
			PackageDir: fork.PackageDir, BuildScript: fork.BuildScript},
	}, nil
}

// forkRecipeFile records, inside a fork distribution, the recipe that built it.
const forkRecipeFile = ".fork-recipe.json"

type forkRecipe struct {
	PackageDir  string `json:"packageDir,omitempty"`
	BuildScript string `json:"buildScript,omitempty"`
}

// readForkRecipe returns the recipe a distribution was built with. A missing
// file reads as the default recipe, which is how every fork was built before
// the recipe was configurable; an unreadable one reads as an impossible recipe
// so the distribution is rebuilt.
func readForkRecipe(root string) forkRecipe {
	b, err := os.ReadFile(filepath.Join(root, forkRecipeFile))
	if errors.Is(err, os.ErrNotExist) {
		return forkRecipe{}
	}
	var r forkRecipe
	if err != nil || json.Unmarshal(b, &r) != nil {
		return forkRecipe{BuildScript: " unreadable"}
	}
	return r
}

func writeForkRecipe(root string, r forkRecipe) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, forkRecipeFile), b, 0o644)
}

// buildFork checks out the fork's pinned commit, builds it, and packs it into a
// tarball kept beside the distribution in root, returning the tarball's path.
// Bun (unlike npm) neither installs a git dependency's devDependencies nor runs
// its `prepare` script, so installing the git URL directly would yield an
// unbuilt package; building here reproduces what npm did implicitly.
//
// Dependencies are installed at the clone root so a workspace monorepo's
// members resolve; the build script and the pack run in fork.PackageDir.
func buildFork(ctx context.Context, cfg config.Config, bun, root string, fork config.ACPFork, log io.Writer) (string, error) {
	src := filepath.Join(root, ".fork-src")
	if err := os.RemoveAll(src); err != nil {
		return "", err
	}
	defer os.RemoveAll(src)
	pkg := filepath.Join(src, filepath.FromSlash(fork.PackageDir))
	slog.Info("building ACP fork", "repo", fork.Repo, "commit", fork.Commit, "dir", src,
		"package_dir", fork.PackageDir, "build_script", fork.BuildScript)
	fmt.Fprintf(log, "tandem: building ACP fork %s#%s\n", fork.Repo, fork.ShortCommit())
	if err := runGit(ctx, root, log, "clone", "--quiet", fork.Repo, src); err != nil {
		return "", err
	}
	if err := runGit(ctx, src, log, "checkout", "--quiet", "--detach", fork.Commit); err != nil {
		return "", err
	}
	run := func(dir string, args ...string) error {
		cmd := exec.CommandContext(ctx, bun, args...)
		cmd.Dir = dir
		cmd.Stdout = log
		cmd.Stderr = log
		cmd.Env = buildEnv(cfg, bun)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("bun %s: %w", strings.Join(args, " "), err)
		}
		return nil
	}
	scripts, err := packageScripts(filepath.Join(pkg, "package.json"))
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("fork packageDir %q has no package.json at commit %s", fork.PackageDir, fork.ShortCommit())
	}
	if err != nil {
		return "", err
	}
	script := fork.BuildScript
	switch script {
	case "":
		// A fork's `prepare` is often `npm run build`, which fails on a host
		// with only Bun, so prefer the build script it delegates to.
		switch {
		case scripts["build"] != "":
			script = "build"
		case scripts["prepare"] != "":
			script = "prepare"
		}
	case config.ForkBuildNone:
		script = ""
	default:
		if scripts[script] == "" {
			return "", fmt.Errorf("fork buildScript %q is not a script in %s", script, filepath.ToSlash(filepath.Join(fork.PackageDir, "package.json")))
		}
	}
	if err := run(src, "install"); err != nil {
		return "", err
	}
	if script != "" {
		if err := run(pkg, "run", script); err != nil {
			return "", err
		}
	}
	packDir := filepath.Join(root, ".fork-pack")
	if err := os.RemoveAll(packDir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		return "", err
	}
	// Packing runs the package's prepack lifecycle, which is where some
	// packages (omp) generate dist/ — hence buildScript `none`.
	if err := run(pkg, "pm", "pack", "--quiet", "--destination", packDir); err != nil {
		return "", err
	}
	tarballs, _ := filepath.Glob(filepath.Join(packDir, "*.tgz"))
	if len(tarballs) != 1 {
		return "", fmt.Errorf("bun pm pack produced %d tarballs in %s", len(tarballs), packDir)
	}
	return tarballs[0], nil
}

func packageScripts(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var manifest struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(b, &manifest); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return manifest.Scripts, nil
}

// forkLockCurrent reports whether locked already satisfies fork.
func forkLockCurrent(locked LockedAgent, fork config.ACPFork, dist string) bool {
	return locked.Fork != nil && locked.Fork.Commit == fork.Commit &&
		locked.Fork.Repo == fork.Repo && locked.Fork.PackageDir == fork.PackageDir &&
		locked.Fork.BuildScript == fork.BuildScript && distExists(locked.Path, dist)
}

// EnsureForkClone makes sure a fork-tracked agent has a working clone on disk
// with both `origin` (the fork) and `upstream` remotes, and returns its path.
// This is the repository a rebase agent is spawned against; it is deliberately
// separate from the installed distribution, which is an immutable npm install.
func EnsureForkClone(ctx context.Context, cfg config.Config, agent string, fork config.ACPFork, log io.Writer) (string, error) {
	dir := config.ForkCloneDir(cfg.RuntimeRoot, agent, fork)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		// Already cloned; only reconcile the remotes, which may have been
		// re-pointed in config since the clone was made.
		if err := setForkRemotes(ctx, dir, fork, log); err != nil {
			return "", err
		}
		return dir, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", err
	}
	fmt.Fprintf(log, "tandem: cloning ACP fork %s into %s\n", fork.Repo, dir)
	if err := runGit(ctx, filepath.Dir(dir), log, "clone", fork.Repo, dir); err != nil {
		return "", fmt.Errorf("clone ACP fork %s: %w", fork.Repo, err)
	}
	if err := setForkRemotes(ctx, dir, fork, log); err != nil {
		return "", err
	}
	// Check out the fork branch so an agent spawned here starts on the right ref.
	if err := runGit(ctx, dir, log, "checkout", fork.Ref); err != nil {
		return "", fmt.Errorf("check out fork ref %s: %w", fork.Ref, err)
	}
	return dir, nil
}

// setForkRemotes ensures `upstream` points at the fork's source repository. A
// rebase agent needs it to fetch the new release and to inspect whether the
// carried change has been upstreamed.
func setForkRemotes(ctx context.Context, dir string, fork config.ACPFork, log io.Writer) error {
	upstream := strings.TrimSpace(fork.UpstreamRepo)
	if upstream == "" {
		return nil
	}
	existing, err := gitOutput(ctx, dir, "remote", "get-url", "upstream")
	if err == nil {
		if strings.TrimSpace(existing) == upstream {
			return nil
		}
		return runGit(ctx, dir, log, "remote", "set-url", "upstream", upstream)
	}
	return runGit(ctx, dir, log, "remote", "add", "upstream", upstream)
}

func runGit(ctx context.Context, dir string, log io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Stdout = log
	cmd.Stderr = log
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	b, err := cmd.Output()
	return string(b), err
}

// ReturnToUpstream drops an agent's fork tracking and installs a published
// version again. It is the mechanism behind `tandem acp upstream`, used once the
// fork's change has landed upstream so the fork is no longer needed.
//
// version may be empty, in which case the newest version inside Tandem's
// declared compatibility range is installed.
func ReturnToUpstream(ctx context.Context, cfg config.Config, agent, version string, log io.Writer) (LockedAgent, error) {
	p, ok := agentPins[agent]
	if !ok {
		return LockedAgent{}, fmt.Errorf("agent %s is not managed", agent)
	}
	forks, err := config.LoadACPForks(cfg.Home)
	if err != nil {
		return LockedAgent{}, err
	}
	fork, tracked := forks[agent]
	if !tracked {
		return LockedAgent{}, fmt.Errorf("agent %s is not tracking a fork", agent)
	}

	installMu.Lock()
	if version == "" {
		published, listErr := publishedVersions(ctx, p.packageName)
		if listErr != nil {
			installMu.Unlock()
			return LockedAgent{}, listErr
		}
		// Return to the newest release, regardless of the declared constraint.
		if pick, ok := newestPublished(published); ok {
			version = pick
		} else {
			installMu.Unlock()
			return LockedAgent{}, fmt.Errorf("%s has no published release to return to", p.packageName)
		}
	}
	installMu.Unlock()

	// Clear the record before installing: InstallUpdate deliberately refuses to
	// overwrite a tracked fork, so the record has to be gone for the install to
	// proceed. A failed install restores it, so a broken upstream release cannot
	// strand the agent with neither a fork nor a working install.
	if _, _, err := config.DeleteACPFork(cfg.Home, agent); err != nil {
		return LockedAgent{}, err
	}
	locked, err := InstallUpdate(ctx, cfg, agent, version, log)
	if err != nil {
		if _, saveErr := config.SaveACPFork(cfg.Home, agent, fork); saveErr != nil {
			return LockedAgent{}, fmt.Errorf("install upstream %s@%s failed (%v) and restoring fork tracking also failed: %w", p.packageName, version, err, saveErr)
		}
		return LockedAgent{}, fmt.Errorf("install upstream %s@%s: %w (fork tracking left in place)", p.packageName, version, err)
	}
	fmt.Fprintf(log, "tandem: %s returned to upstream %s@%s; fork %s#%s is no longer tracked\n",
		agent, p.packageName, version, fork.Repo, fork.ShortCommit())
	return locked, nil
}

// ResolveVersion asks the registry which concrete version a range selects for a managed
// agent's package. `tandem acp upstream` uses it so an operator or agent can say
// "the release that has the change" as a range rather than pinning a version by
// hand.
func ResolveVersion(ctx context.Context, cfg config.Config, agent, constraint string) (string, error) {
	p, ok := agentPins[agent]
	if !ok {
		return "", fmt.Errorf("agent %s is not managed", agent)
	}
	installMu.Lock()
	defer installMu.Unlock()
	published, err := publishedVersions(ctx, p.packageName)
	if err != nil {
		return "", fmt.Errorf("resolve %s@%s: %w", p.packageName, constraint, err)
	}
	version, ok := newestInRange(published, constraint)
	if !ok {
		return "", fmt.Errorf("resolve %s@%s: no published version satisfies it", p.packageName, constraint)
	}
	return version, nil
}

// forkDistribution converts a fork lock entry into a spawnable distribution.
func forkDistribution(locked LockedAgent) *agentadapter.Distribution {
	return &agentadapter.Distribution{Source: "fork", Package: locked.Package, Version: locked.Version, Path: locked.Path}
}
