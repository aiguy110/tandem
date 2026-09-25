package workspace_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aiguy110/tandem/internal/workspace"
)

func git(t *testing.T, cwd string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

type fixture struct{ root, repo, home, initial, feature string }

func newFixture(t *testing.T) fixture {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "fake-home")
	repo := filepath.Join(root, "projects", "demo-repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "-q", "-b", "main")
	git(t, repo, "config", "user.email", "test@example.com")
	git(t, repo, "config", "user.name", "Test")
	write(t, filepath.Join(repo, "README.md"), "demo\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "init")
	initial := git(t, repo, "rev-parse", "HEAD")
	git(t, repo, "switch", "-q", "-c", "feature/migration")
	write(t, filepath.Join(repo, "FEATURE.md"), "feature\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "feature base")
	feature := git(t, repo, "rev-parse", "HEAD")
	git(t, repo, "switch", "-q", "main")
	return fixture{root, repo, home, initial, feature}
}

func TestWorkspaceLifecycleBlackBox(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	m := workspace.New(workspace.Config{WorktreesDir: filepath.Join(f.home, "worktrees")})

	t.Run("existing directory validation", func(t *testing.T) {
		_, err := m.Provision(ctx, workspace.Workspace{Kind: workspace.KindExisting, CWD: filepath.Join(f.root, "missing")}, "x")
		if !workspace.IsCode(err, "no_such_dir") {
			t.Fatalf("got %v", err)
		}
		// Occupancy is no longer refused here: two agents may share a checkout
		// (a hand-off picks up uncommitted work in place), and the registry
		// owns both the warning and the shared-teardown accounting.
		got, err := m.Provision(ctx, workspace.Workspace{Kind: workspace.KindExisting, CWD: f.repo}, "x")
		if err != nil || got.CWD != f.repo {
			t.Fatalf("got result=%+v err=%v", got, err)
		}
	})

	a, err := m.Provision(ctx, workspace.Workspace{Kind: workspace.KindWorktree, Repo: f.repo}, "wt-a")
	if err != nil {
		t.Fatal(err)
	}
	if a.Workspace.Branch != "tandem/main/wt-a" || a.Workspace.Source.Commit != f.initial || a.Workspace.Source.Ref != "refs/heads/main" || a.Workspace.Integration.Ref != "refs/heads/main" || a.Workspace.BaseRef != f.initial {
		t.Fatalf("unexpected resolved workspace: %+v", a.Workspace)
	}
	if got := git(t, a.CWD, "rev-parse", "HEAD"); got != f.initial {
		t.Fatalf("started at %s", got)
	}
	if got := git(t, f.repo, "rev-parse", "--abbrev-ref", "HEAD"); got != "main" {
		t.Fatalf("source checkout changed: %s", got)
	}

	b, err := m.Provision(ctx, workspace.Workspace{Kind: workspace.KindWorktree, Repo: f.repo}, "wt-b")
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(a.CWD, "only-a.txt"), "a\n")
	if _, err := os.Stat(filepath.Join(b.CWD, "only-a.txt")); !os.IsNotExist(err) {
		t.Fatal("worktrees not isolated")
	}
	git(t, a.CWD, "add", ".")
	git(t, a.CWD, "commit", "-q", "-m", "agent work")
	write(t, filepath.Join(a.CWD, "dirty.txt"), "dirty\n")
	p, err := m.ClosePreview(ctx, a.CWD, a.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Uncommitted, "dirty.txt") || !strings.Contains(p.Unmerged, "agent work") || p.Ahead == nil || *p.Ahead != 1 {
		t.Fatalf("bad preview: %+v", p)
	}
	if err := m.Teardown(ctx, a.Workspace, a.CWD, false, false); !workspace.IsCode(err, "dirty_worktree") {
		t.Fatalf("got %v", err)
	}
	if err := m.Teardown(ctx, a.Workspace, a.CWD, true, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.CWD); !os.IsNotExist(err) {
		t.Fatal("forced close kept checkout")
	}
	if got := git(t, f.repo, "branch", "--list", a.Workspace.Branch); got == "" {
		t.Fatal("forced close deleted branch")
	}
	if err := m.Teardown(ctx, b.Workspace, b.CWD, false, false); err != nil {
		t.Fatal(err)
	}
	if got := git(t, f.repo, "branch", "--list", b.Workspace.Branch); got == "" {
		t.Fatal("clean close deleted branch")
	}
	if _, err := m.Reattach(ctx, b.Workspace, b.CWD); err != nil {
		t.Fatal(err)
	}
	if got := git(t, b.CWD, "rev-parse", "--abbrev-ref", "HEAD"); got != b.Workspace.Branch {
		t.Fatalf("reattached %s", got)
	}
}

func TestTeardownDeinitializesSubmodulesOnlyAfterExplicitConfirmation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	sub := filepath.Join(f.root, "submodule-source")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, sub, "init", "-q", "-b", "main")
	git(t, sub, "config", "user.email", "test@example.com")
	git(t, sub, "config", "user.name", "Test")
	write(t, filepath.Join(sub, "README.md"), "submodule\n")
	git(t, sub, "add", ".")
	git(t, sub, "commit", "-q", "-m", "init")
	git(t, f.repo, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "vendor/sub")
	git(t, f.repo, "commit", "-qam", "add submodule")

	m := workspace.New(workspace.Config{WorktreesDir: filepath.Join(f.home, "worktrees")})
	wt, err := m.Provision(ctx, workspace.Workspace{Kind: workspace.KindWorktree, Repo: f.repo}, "with-submodule")
	if err != nil {
		t.Fatal(err)
	}
	git(t, wt.CWD, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "--recursive")
	write(t, filepath.Join(wt.CWD, "vendor", "sub", "dirty.txt"), "discard me\n")
	preview, err := m.ClosePreview(ctx, wt.CWD, wt.Workspace)
	if err != nil || len(preview.Submodules) != 1 || preview.Submodules[0].Path != "vendor/sub" || preview.Submodules[0].Untracked != "dirty.txt" || preview.Submodules[0].Uncommitted != "" {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	if s := preview.Submodules[0]; s.Head == "" || s.Branch != "" || s.Upstream != "origin/main" || s.Ahead == nil || *s.Ahead != 0 || *s.Behind != 0 {
		t.Fatalf("detached submodule sync=%+v", s)
	}
	subDir := filepath.Join(wt.CWD, "vendor", "sub")
	git(t, subDir, "-c", "user.email=test@example.com", "-c", "user.name=Test", "commit", "-q", "--allow-empty", "-m", "local")
	write(t, filepath.Join(subDir, "README.md"), "edited\n")
	preview, err = m.ClosePreview(ctx, wt.CWD, wt.Workspace)
	if s := preview.Submodules[0]; err != nil || s.Ahead == nil || *s.Ahead != 1 || !strings.Contains(s.Uncommitted, "README.md") {
		t.Fatalf("diverged submodule sync=%+v err=%v", s, err)
	}
	git(t, subDir, "reset", "-q", "--hard", "HEAD~1")
	if err := os.Remove(filepath.Join(wt.CWD, "vendor", "sub", "dirty.txt")); err != nil {
		t.Fatal(err)
	}
	if err := m.Teardown(ctx, wt.Workspace, wt.CWD, false, false); !workspace.IsCode(err, "submodules_block_worktree_removal") {
		t.Fatalf("Teardown without confirmation = %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt.CWD, "vendor", "sub")); err != nil {
		t.Fatalf("unconfirmed teardown altered submodule: %v", err)
	}
	if err := m.Teardown(ctx, wt.Workspace, wt.CWD, true, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wt.CWD); !os.IsNotExist(err) {
		t.Fatal("teardown retained checkout")
	}
}

func TestSubmoduleWithUnmappedNestedGitlinkDoesNotBlockClose(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	sub := filepath.Join(f.root, "submodule-source")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, sub, "init", "-q", "-b", "main")
	git(t, sub, "config", "user.email", "test@example.com")
	git(t, sub, "config", "user.name", "Test")
	write(t, filepath.Join(sub, "README.md"), "submodule\n")
	git(t, sub, "add", ".")
	git(t, sub, "commit", "-q", "-m", "init")
	// A gitlink committed without a .gitmodules entry, as left behind by
	// `git add` of a nested checkout.
	git(t, sub, "update-index", "--add", "--cacheinfo", "160000,"+f.initial+",configs")
	git(t, sub, "commit", "-q", "-m", "stray gitlink")
	git(t, f.repo, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "vendor/sub")
	git(t, f.repo, "commit", "-qam", "add submodule")

	m := workspace.New(workspace.Config{WorktreesDir: filepath.Join(f.home, "worktrees")})
	wt, err := m.Provision(ctx, workspace.Workspace{Kind: workspace.KindWorktree, Repo: f.repo}, "with-unmapped")
	if err != nil {
		t.Fatal(err)
	}
	git(t, wt.CWD, "-c", "protocol.file.allow=always", "submodule", "update", "--init")
	preview, err := m.ClosePreview(ctx, wt.CWD, wt.Workspace)
	if err != nil || len(preview.Submodules) != 1 || preview.Submodules[0].Path != "vendor/sub" {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	if err := m.Teardown(ctx, wt.Workspace, wt.CWD, true, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wt.CWD); !os.IsNotExist(err) {
		t.Fatal("teardown retained checkout")
	}
}

func TestTeardownForceRemovesOrphanedManagedWorktree(t *testing.T) {
	root := t.TempDir()
	managed := filepath.Join(root, "worktrees")
	orphan := filepath.Join(managed, "repo", "agent")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}
	m := workspace.New(workspace.Config{WorktreesDir: managed})
	ws := workspace.Workspace{Kind: workspace.KindWorktree, Repo: filepath.Join(root, "missing-repo")}
	if err := m.Teardown(context.Background(), ws, orphan, false, false); !workspace.IsCode(err, "orphaned_worktree") {
		t.Fatalf("expected orphaned_worktree, got %v", err)
	}
	if err := m.Teardown(context.Background(), ws, orphan, true, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan still exists: %v", err)
	}
}

func TestDiffSeparatesUncommittedAndCommittedChanges(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	m := workspace.New(workspace.Config{WorktreesDir: filepath.Join(f.home, "worktrees")})
	provisioned, err := m.Provision(ctx, workspace.Workspace{Kind: workspace.KindWorktree, Repo: f.repo}, "diff-agent")
	if err != nil {
		t.Fatal(err)
	}

	write(t, filepath.Join(provisioned.CWD, "committed.txt"), "committed\n")
	git(t, provisioned.CWD, "add", "committed.txt")
	git(t, provisioned.CWD, "commit", "-q", "-m", "committed change")
	write(t, filepath.Join(provisioned.CWD, "README.md"), "changed\n")
	write(t, filepath.Join(provisioned.CWD, "untracked.txt"), "new\n")

	got, err := m.Diff(ctx, provisioned.CWD, provisioned.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Uncommitted, "README.md") || !strings.Contains(got.Uncommitted, "untracked.txt") {
		t.Fatalf("uncommitted patch missing changes:\n%s", got.Uncommitted)
	}
	if strings.Contains(got.Uncommitted, "committed.txt") {
		t.Fatalf("committed file leaked into uncommitted patch:\n%s", got.Uncommitted)
	}
	if !strings.Contains(got.Committed, "committed.txt") || strings.Contains(got.Committed, "untracked.txt") {
		t.Fatalf("committed patch incorrect:\n%s", got.Committed)
	}
	if got.TargetRef != "refs/heads/main" {
		t.Fatalf("target ref=%q", got.TargetRef)
	}
}

func TestFeatureAwareProvisionDiscoveryAndState(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	m := workspace.New(workspace.Config{WorktreesDir: filepath.Join(f.home, "worktrees")})
	refs, err := workspace.ListGitRefs(ctx, f.repo)
	if err != nil {
		t.Fatal(err)
	}
	var main, feature *workspace.GitRefInfo
	for i := range refs {
		if refs[i].Ref == "refs/heads/main" {
			main = &refs[i]
		}
		if refs[i].Ref == "refs/heads/feature/migration" {
			feature = &refs[i]
		}
	}
	if main == nil || !main.IsCurrent || main.CheckedOutAt != f.repo || feature == nil || feature.Commit != f.feature || feature.Kind != workspace.RefLocalBranch {
		t.Fatalf("refs=%+v", refs)
	}
	_, err = m.Provision(ctx, workspace.Workspace{Kind: workspace.KindWorktree, Repo: f.repo, Branch: "main", BranchMode: "attach"}, "checked")
	if !workspace.IsCode(err, "branch_checked_out") {
		t.Fatalf("got %v", err)
	}

	ws := workspace.Workspace{Kind: workspace.KindWorktree, Repo: f.repo, BranchMode: "create", Source: &workspace.Source{Ref: "refs/heads/feature/migration"}, Integration: &workspace.Integration{Kind: workspace.RefLocalBranch, Ref: "refs/heads/feature/migration"}}
	result, err := m.Provision(ctx, ws, "wt-feature")
	if err != nil {
		t.Fatal(err)
	}
	if result.Workspace.Branch != "tandem/migration/wt-feature" || result.Workspace.Source.Commit != f.feature {
		t.Fatalf("resolved=%+v", result.Workspace)
	}
	write(t, filepath.Join(result.CWD, "agent.txt"), "agent\n")
	git(t, result.CWD, "add", ".")
	git(t, result.CWD, "commit", "-q", "-m", "agent feature work")
	target := filepath.Join(f.root, "target-checkout")
	git(t, f.repo, "worktree", "add", "-q", target, "feature/migration")
	write(t, filepath.Join(target, "target.txt"), "target\n")
	git(t, target, "add", ".")
	git(t, target, "commit", "-q", "-m", "target moved")
	git(t, f.repo, "worktree", "remove", target)
	state, err := m.GitState(ctx, result.CWD, result.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != "diverged" || state.Ahead != 1 || state.Behind != 1 || state.TargetRef != "refs/heads/feature/migration" {
		t.Fatalf("state=%+v", state)
	}
	p, err := m.ClosePreview(ctx, result.CWD, result.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	if p.Ahead == nil || *p.Ahead != 1 || p.Behind == nil || *p.Behind != 1 || !strings.Contains(p.Unmerged, "agent feature work") {
		t.Fatalf("preview=%+v", p)
	}
	_, err = m.Provision(ctx, workspace.Workspace{Kind: workspace.KindWorktree, Repo: f.repo, BranchMode: "create", Branch: result.Workspace.Branch, Source: &workspace.Source{Ref: f.feature}}, "collision")
	if !workspace.IsCode(err, "branch_exists") {
		t.Fatalf("got %v", err)
	}
	if err = m.Teardown(ctx, result.Workspace, result.CWD, false, false); err != nil {
		t.Fatal(err)
	}
	attached, err := m.Provision(ctx, workspace.Workspace{Kind: workspace.KindWorktree, Repo: f.repo, BranchMode: "attach", Branch: result.Workspace.Branch, Integration: result.Workspace.Integration}, "attached")
	if err != nil {
		t.Fatal(err)
	}
	if got := git(t, attached.CWD, "rev-parse", "--abbrev-ref", "HEAD"); got != result.Workspace.Branch {
		t.Fatalf("attached %s", got)
	}
}

func TestRollbackAndAutoBranchCollision(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	m := workspace.New(workspace.Config{WorktreesDir: filepath.Join(f.home, "worktrees")})
	git(t, f.repo, "branch", "tandem/main/auto", "main")
	auto, err := m.Provision(ctx, workspace.Workspace{Kind: workspace.KindWorktree, Repo: f.repo}, "auto")
	if err != nil {
		t.Fatal(err)
	}
	if auto.Workspace.Branch != "tandem/main/auto-2" {
		t.Fatalf("automatic collision branch=%s", auto.Workspace.Branch)
	}
	if err = m.Teardown(ctx, auto.Workspace, auto.CWD, false, false); err != nil {
		t.Fatal(err)
	}
	first, err := m.Provision(ctx, workspace.Workspace{Kind: workspace.KindWorktree, Repo: f.repo}, "same")
	if err != nil {
		t.Fatal(err)
	}
	// Keep the first branch but remove its checkout; an automatically derived collision gets a suffix.
	if err = m.Teardown(ctx, first.Workspace, first.CWD, false, false); err != nil {
		t.Fatal(err)
	}
	second, err := m.Provision(ctx, workspace.Workspace{Kind: workspace.KindWorktree, Repo: f.repo}, "same-2")
	if err != nil {
		t.Fatal(err)
	}
	if second.Workspace.Branch != "tandem/main/same-2" {
		t.Fatalf("branch=%s", second.Workspace.Branch)
	}
	m.Rollback(ctx, second.Workspace, second.CWD, second.CreatedBranch)
	if _, err := os.Stat(second.CWD); !os.IsNotExist(err) {
		t.Fatal("rollback kept checkout")
	}
	if got := git(t, f.repo, "branch", "--list", second.Workspace.Branch); got != "" {
		t.Fatal("rollback kept newly-created branch")
	}
}

func TestAutoBranchAvoidsExistingTandemBranchNamespace(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	m := workspace.New(workspace.Config{WorktreesDir: filepath.Join(f.home, "worktrees")})
	// An ACP fork carries its commits on a branch literally named `tandem`,
	// which makes every `tandem/...` ref impossible to create.
	git(t, f.repo, "switch", "-q", "-c", "tandem")
	res, err := m.Provision(ctx, workspace.Workspace{Kind: workspace.KindWorktree, Repo: f.repo}, "einstein-501")
	if err != nil {
		t.Fatal(err)
	}
	if res.Workspace.Branch != "tandem-tandem-einstein-501" {
		t.Fatalf("branch=%s", res.Workspace.Branch)
	}
}

func TestRepositoryDiscoveryIsBoundedAndReportsCollisions(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	skipped := filepath.Join(f.root, "projects", "node_modules", "ignored")
	if err := os.MkdirAll(skipped, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, skipped, "init", "-q")
	tooDeep := filepath.Join(f.root, "projects", "a", "b", "deep")
	if err := os.MkdirAll(tooDeep, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, tooDeep, "init", "-q")
	repos, err := workspace.ListRepos(ctx, []string{filepath.Join(f.root, "projects")}, 1, func(path string) bool { return path == f.repo })
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 || repos[0].Path != f.repo || repos[0].Name != "demo-repo" || repos[0].CurrentBranch != "main" || repos[0].Dirty || !repos[0].HasLiveAgent {
		t.Fatalf("repos=%+v", repos)
	}
}

func TestGitRunnerHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := (workspace.GitRunner{}).Run(ctx, t.TempDir(), "status")
	if err == nil || (!errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "context canceled")) {
		t.Fatalf("got %v", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	if _, err = (workspace.GitRunner{}).Run(ctx, t.TempDir(), "status"); err == nil {
		t.Fatal("expected timeout")
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}
