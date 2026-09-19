package runtimeinstall

import (
	"context"
	"errors"
	"fmt"
	"io"
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
func installFork(ctx context.Context, cfg config.Config, npm, agent string, fork config.ACPFork, log io.Writer) (LockedAgent, error) {
	p := agentPins[agent]
	version := fork.DistributionVersion()
	root := filepath.Join(cfg.RuntimeRoot, "agents", agent, version)
	if !distExists(root, p.dist) {
		if err := ensureRuntimeRoot(root); err != nil {
			return LockedAgent{}, err
		}
		if err := installAt(ctx, cfg, npm, root, fork.InstallSpec(), log); err != nil {
			return LockedAgent{}, err
		}
	}
	if !distExists(root, p.dist) {
		// Most likely the fork renamed the package or stopped building dist/ on
		// `prepare`; npm reports success either way, so say what to check.
		return LockedAgent{}, fmt.Errorf("provision agent %s from fork %s#%s: %s is missing after install (the fork must keep package name %q and build %s during `prepare`)",
			agent, fork.Repo, fork.ShortCommit(), p.dist, p.packageName, p.dist)
	}
	return LockedAgent{
		Package:    p.packageName,
		Constraint: strings.TrimPrefix(p.spec, p.packageName+"@"),
		Version:    version,
		Path:       root,
		Fork:       &LockedFork{Repo: fork.Repo, Ref: fork.Ref, Commit: fork.Commit, UpstreamVersion: fork.UpstreamVersion},
	}, nil
}

// forkLockCurrent reports whether locked already satisfies fork.
func forkLockCurrent(locked LockedAgent, fork config.ACPFork, dist string) bool {
	return locked.Fork != nil && locked.Fork.Commit == fork.Commit &&
		locked.Fork.Repo == fork.Repo && distExists(locked.Path, dist)
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
	npm, err := resolveNpm(cfg)
	if err != nil {
		installMu.Unlock()
		return LockedAgent{}, err
	}
	if version == "" {
		if version, err = npmView(ctx, cfg, npm, p.spec, "version"); err != nil {
			installMu.Unlock()
			return LockedAgent{}, err
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

// ResolveVersion asks npm which concrete version a range selects for a managed
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
	npm, err := resolveNpm(cfg)
	if err != nil {
		return "", err
	}
	version, err := npmView(ctx, cfg, npm, p.packageName+"@"+constraint, "version")
	if err != nil {
		return "", fmt.Errorf("resolve %s@%s: %w", p.packageName, constraint, err)
	}
	return version, nil
}

// forkDistribution converts a fork lock entry into a spawnable distribution.
func forkDistribution(locked LockedAgent) *agentadapter.Distribution {
	return &agentadapter.Distribution{Source: "fork", Package: locked.Package, Version: locked.Version, Path: locked.Path}
}
