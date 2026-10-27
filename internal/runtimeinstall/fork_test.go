package runtimeinstall

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aiguy110/tandem/internal/config"
)

// forkedHome writes a config.yml declaring a fork for agent and returns a Config
// pointing at fresh home/runtime directories.
func forkedHome(t *testing.T, agent string, body string) config.Config {
	t.Helper()
	home := t.TempDir()
	if err := os.WriteFile(config.ConfigFilePath(home), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return config.Config{Home: home, RuntimeRoot: t.TempDir()}
}

const piForkConfig = `acpForks:
  pi:
    repo: https://github.com/me/pi-acp
    ref: tandem
    commit: 52f61f3850a883b9b5a3bb3245908bd5d95b7d43
    upstreamPackage: pi-acp
    upstreamVersion: 0.0.33
    reason: Emits usage_update
`

func TestForkForOnlyAppliesToManagedAgents(t *testing.T) {
	cfg := forkedHome(t, "pi", piForkConfig)
	if _, ok, err := ForkFor(cfg, "pi"); err != nil || !ok {
		t.Fatalf("expected pi fork, ok=%v err=%v", ok, err)
	}
	// An unmanaged agent launches an arbitrary command, so a fork record for it
	// would have nothing to install.
	if _, ok, err := ForkFor(cfg, "some-custom-agent"); err != nil || ok {
		t.Fatalf("expected no fork for unmanaged agent, ok=%v err=%v", ok, err)
	}
	if _, ok, err := ForkFor(cfg, "codex"); err != nil || ok {
		t.Fatalf("expected no fork for untracked managed agent, ok=%v err=%v", ok, err)
	}
}

func TestForkLockCurrentDetectsRepin(t *testing.T) {
	root := t.TempDir()
	dist := agentPins["pi"].dist
	path := filepath.Join(root, dist)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("stub"), 0o644); err != nil {
		t.Fatal(err)
	}
	fork := config.ACPFork{Repo: "https://github.com/me/pi-acp", Ref: "tandem", Commit: "aaaaaaa", UpstreamPackage: "pi-acp", UpstreamVersion: "0.0.33"}
	locked := LockedAgent{Path: root, Fork: &LockedFork{Repo: fork.Repo, Ref: fork.Ref, Commit: fork.Commit}}

	if !forkLockCurrent(locked, fork, dist) {
		t.Fatal("matching commit with a built dist should be current")
	}
	// Re-pinned to a new commit: must reinstall.
	repinned := fork
	repinned.Commit = "bbbbbbb"
	if forkLockCurrent(locked, repinned, dist) {
		t.Fatal("a new commit must invalidate the lock")
	}
	// Re-pointed at a different fork remote: must reinstall.
	moved := fork
	moved.Repo = "https://github.com/other/pi-acp"
	if forkLockCurrent(locked, moved, dist) {
		t.Fatal("a new repo must invalidate the lock")
	}
	// A published install must never satisfy a fork record.
	if forkLockCurrent(LockedAgent{Path: root, Version: "0.0.33"}, fork, dist) {
		t.Fatal("an upstream install must not satisfy a fork record")
	}
	// Pinned correctly, but the tree was never built.
	if forkLockCurrent(LockedAgent{Path: t.TempDir(), Fork: locked.Fork}, fork, dist) {
		t.Fatal("a missing dist must invalidate the lock")
	}
	// Same commit, corrected build recipe: must rebuild.
	rerecipe := fork
	rerecipe.PackageDir, rerecipe.BuildScript = "packages/pi-acp", config.ForkBuildNone
	if forkLockCurrent(locked, rerecipe, dist) {
		t.Fatal("a new build recipe must invalidate the lock")
	}
}

// fakeBun writes a stand-in bun that logs "<cwd> <args>" to the returned file,
// makes `pm pack` drop a tarball, and makes `add` install omp's entry point.
func fakeBun(t *testing.T) (bun, logPath string) {
	t.Helper()
	dir := t.TempDir()
	bun, logPath = filepath.Join(dir, "bun"), filepath.Join(dir, "bun.log")
	script := `#!/bin/sh
echo "$(pwd -P) $*" >> "$FAKE_BUN_LOG"
case "$1" in
pm) while [ $# -gt 0 ]; do [ "$1" = --destination ] && : > "$2/fork.tgz"; shift; done ;;
add) mkdir -p node_modules/@oh-my-pi/pi-coding-agent/dist && : > node_modules/@oh-my-pi/pi-coding-agent/dist/cli.js ;;
esac
exit 0
`
	if err := os.WriteFile(bun, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_BUN_LOG", logPath)
	return bun, logPath
}

// monorepoFork commits a Bun-workspace fixture shaped like omp: a private root
// whose `build` builds everything, and the package in packages/coding-agent
// with no `build` of its own, only `prepare`.
func monorepoFork(t *testing.T) config.ACPFork {
	t.Helper()
	if _, err := os.Stat("/usr/bin/git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	files := map[string]string{
		"package.json":                       `{"private":true,"workspaces":["packages/*"],"scripts":{"build":"build-everything"}}`,
		"packages/coding-agent/package.json": `{"name":"@oh-my-pi/pi-coding-agent","scripts":{"prepare":"x","prepack":"make-dist"}}`,
		"packages/other/package.json":        `{"name":"other"}`,
	}
	for name, body := range files {
		path := filepath.Join(repo, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"init", "--initial-branch=tandem"},
		{"add", "."},
		{"-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-m", "seed"},
	} {
		if err := runGit(context.Background(), repo, os.Stderr, args...); err != nil {
			t.Fatalf("seed %v: %v", args, err)
		}
	}
	commit, err := gitOutput(context.Background(), repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return config.ACPFork{Repo: repo, Ref: "tandem", Commit: strings.TrimSpace(commit),
		UpstreamPackage: "@oh-my-pi/pi-coding-agent", UpstreamVersion: "18.12.6", PackageDir: "packages/coding-agent"}
}

func readBunLog(t *testing.T, logPath string) []string {
	t.Helper()
	b, err := os.ReadFile(logPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func TestBuildForkMonorepoInstallsAtRootAndPacksPackage(t *testing.T) {
	fork := monorepoFork(t)
	cfg := config.Config{Home: t.TempDir(), RuntimeRoot: t.TempDir()}
	src := filepath.Join(cfg.RuntimeRoot, ".fork-src")
	pkg := filepath.Join(src, "packages", "coding-agent")
	pack := filepath.Join(cfg.RuntimeRoot, ".fork-pack")

	for name, tc := range map[string]struct {
		script string
		want   []string
	}{
		// The root's build would build every workspace; only the package's own
		// scripts count, and it has `prepare`, not `build`.
		"auto": {"", []string{src + " install", pkg + " run prepare", pkg + " pm pack --quiet --destination " + pack}},
		"none": {config.ForkBuildNone, []string{src + " install", pkg + " pm pack --quiet --destination " + pack}},
	} {
		t.Run(name, func(t *testing.T) {
			bun, logPath := fakeBun(t)
			f := fork
			f.BuildScript = tc.script
			tarball, err := buildFork(context.Background(), cfg, bun, cfg.RuntimeRoot, f, io.Discard)
			if err != nil {
				t.Fatalf("buildFork: %v", err)
			}
			if tarball != filepath.Join(pack, "fork.tgz") {
				t.Fatalf("tarball = %q", tarball)
			}
			if got := readBunLog(t, logPath); !slices.Equal(got, tc.want) {
				t.Fatalf("bun calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(tc.want, "\n"))
			}
		})
	}

	for name, mutate := range map[string]func(*config.ACPFork){
		"missing package dir": func(f *config.ACPFork) { f.PackageDir = "packages/nope" },
		"unknown script":      func(f *config.ACPFork) { f.BuildScript = "build" },
	} {
		t.Run(name, func(t *testing.T) {
			bun, logPath := fakeBun(t)
			f := fork
			mutate(&f)
			if _, err := buildFork(context.Background(), cfg, bun, cfg.RuntimeRoot, f, io.Discard); err == nil {
				t.Fatal("expected buildFork to fail")
			}
			if got := readBunLog(t, logPath); len(got) != 0 {
				t.Fatalf("a bad recipe should fail before running bun, got %q", got)
			}
		})
	}
}

func TestInstallForkRebuildsWhenRecipeChangesAtSameCommit(t *testing.T) {
	fork := monorepoFork(t)
	fork.BuildScript = "prepare"
	cfg := config.Config{Home: t.TempDir(), RuntimeRoot: t.TempDir()}
	bun, logPath := fakeBun(t)
	builds := func() int {
		n := 0
		for _, line := range readBunLog(t, logPath) {
			if strings.HasSuffix(line, " install") {
				n++
			}
		}
		return n
	}

	first, err := installFork(context.Background(), cfg, bun, "omp", fork, io.Discard)
	if err != nil {
		t.Fatalf("first install: %v", err)
	}
	if builds() != 1 || first.Fork.BuildScript != "prepare" || first.Fork.PackageDir != fork.PackageDir {
		t.Fatalf("first install: builds=%d lock=%+v", builds(), first.Fork)
	}
	if _, err := installFork(context.Background(), cfg, bun, "omp", fork, io.Discard); err != nil || builds() != 1 {
		t.Fatalf("same recipe must reuse the distribution: builds=%d err=%v", builds(), err)
	}
	// Same commit, so the same distribution directory, but a corrected recipe.
	fork.BuildScript = config.ForkBuildNone
	second, err := installFork(context.Background(), cfg, bun, "omp", fork, io.Discard)
	if err != nil {
		t.Fatalf("re-recipe install: %v", err)
	}
	if builds() != 2 || second.Path != first.Path || second.Fork.BuildScript != config.ForkBuildNone {
		t.Fatalf("a changed recipe must rebuild in place: builds=%d lock=%+v", builds(), second.Fork)
	}
	if got := readBunLog(t, logPath); strings.Contains(strings.Join(got[len(got)-3:], "\n"), " run ") {
		t.Fatalf("buildScript none must not run a script, got %q", got)
	}
}

func TestInstallUpdateRefusesToOverwriteTrackedFork(t *testing.T) {
	cfg := forkedHome(t, "pi", piForkConfig)
	l := Lockfile{Version: 1, Agents: map[string]LockedAgent{"pi": {
		Package: "pi-acp", Version: "0.0.33+fork.52f61f3850a8", Path: filepath.Join(cfg.RuntimeRoot, "x"),
		Fork: &LockedFork{Repo: "https://github.com/me/pi-acp", Ref: "tandem", Commit: "52f61f3850a883b9b5a3bb3245908bd5d95b7d43"},
	}}}
	if err := writeLock(cfg.RuntimeRoot, l); err != nil {
		t.Fatal(err)
	}
	// PATH is empty so reaching bun at all would fail differently; the guard must
	// trip before any install work.
	t.Setenv("PATH", "")
	_, err := InstallUpdate(context.Background(), cfg, "pi", "0.0.34", os.Stderr)
	if err == nil {
		t.Fatal("expected InstallUpdate to refuse a fork-tracked agent")
	}
	if want := "tandem acp upstream pi"; !strings.Contains(err.Error(), want) {
		t.Fatalf("error should point at the retire command, got: %v", err)
	}
}

func TestReturnToUpstreamRequiresTracking(t *testing.T) {
	cfg := forkedHome(t, "pi", "settings:\n  port: 1\n")
	t.Setenv("PATH", "")
	if _, err := ReturnToUpstream(context.Background(), cfg, "pi", "0.0.33", os.Stderr); err == nil {
		t.Fatal("expected an error for an agent with no fork record")
	}
	if _, err := ReturnToUpstream(context.Background(), cfg, "not-an-agent", "", os.Stderr); err == nil {
		t.Fatal("expected an error for an unmanaged agent")
	}
}

func TestReturnToUpstreamRestoresForkWhenInstallFails(t *testing.T) {
	cfg := forkedHome(t, "pi", piForkConfig)
	// No bun on PATH, so the install inside ReturnToUpstream cannot succeed.
	t.Setenv("PATH", "")
	if _, err := ReturnToUpstream(context.Background(), cfg, "pi", "0.0.33", os.Stderr); err == nil {
		t.Fatal("expected the install to fail")
	}
	// The record must still be there: a failed retirement may not leave the agent
	// with neither a fork nor a working upstream install.
	forks, err := config.LoadACPForks(cfg.Home)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := forks["pi"]; !ok {
		t.Fatalf("fork tracking was not restored after a failed install: %+v", forks)
	}
}

func TestEnsureForkCloneSetsUpstreamRemote(t *testing.T) {
	if _, err := os.Stat("/usr/bin/git"); err != nil {
		t.Skip("git not available")
	}
	// A local bare repo stands in for the fork remote so the test never touches
	// the network. Validate() requires an http(s) URL, but EnsureForkClone itself
	// takes the record as given, so it can be exercised with a path.
	origin := filepath.Join(t.TempDir(), "origin.git")
	if err := runGit(context.Background(), t.TempDir(), os.Stderr, "init", "--bare", "--initial-branch=tandem", origin); err != nil {
		t.Fatal(err)
	}
	seed := t.TempDir()
	for _, args := range [][]string{
		{"init", "--initial-branch=tandem"},
		{"-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "--allow-empty", "-m", "seed"},
		{"remote", "add", "origin", origin},
		{"push", "origin", "tandem"},
	} {
		if err := runGit(context.Background(), seed, os.Stderr, args...); err != nil {
			t.Fatalf("seed %v: %v", args, err)
		}
	}

	upstream := filepath.Join(t.TempDir(), "upstream.git")
	if err := runGit(context.Background(), t.TempDir(), os.Stderr, "init", "--bare", upstream); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Home: t.TempDir(), RuntimeRoot: t.TempDir()}
	fork := config.ACPFork{Repo: origin, Ref: "tandem", Commit: "aaaaaaa", UpstreamPackage: "pi-acp", UpstreamVersion: "0.0.33", UpstreamRepo: upstream}

	dir, err := EnsureForkClone(context.Background(), cfg, "pi", fork, os.Stderr)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	if want := filepath.Join(cfg.RuntimeRoot, "forks", "pi"); dir != want {
		t.Fatalf("clone dir = %q want %q", dir, want)
	}
	got, err := gitOutput(context.Background(), dir, "remote", "get-url", "upstream")
	if err != nil || strings.TrimSpace(got) != upstream {
		t.Fatalf("upstream remote = %q err=%v", got, err)
	}
	// Idempotent: a second call reuses the clone and leaves the remote alone.
	if _, err := EnsureForkClone(context.Background(), cfg, "pi", fork, os.Stderr); err != nil {
		t.Fatalf("second EnsureForkClone: %v", err)
	}
	// Re-pointing upstream in config updates the existing clone's remote.
	moved := filepath.Join(t.TempDir(), "upstream2.git")
	if err := runGit(context.Background(), t.TempDir(), os.Stderr, "init", "--bare", moved); err != nil {
		t.Fatal(err)
	}
	fork.UpstreamRepo = moved
	if _, err := EnsureForkClone(context.Background(), cfg, "pi", fork, os.Stderr); err != nil {
		t.Fatal(err)
	}
	if got, err := gitOutput(context.Background(), dir, "remote", "get-url", "upstream"); err != nil || strings.TrimSpace(got) != moved {
		t.Fatalf("upstream remote not updated: %q err=%v", got, err)
	}
}
