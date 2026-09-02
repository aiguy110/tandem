package workspace_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aiguy110/tandem/internal/workspace"
)

func TestRepoForDirWalksUpToRepositoryRoot(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "myrepo")
	nested := filepath.Join(repo, "internal", "pkg")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{repo, nested} {
		origin := workspace.RepoForDir(dir)
		if origin.Path != repo || origin.Name != "myrepo" {
			t.Fatalf("RepoForDir(%s) = %+v, want path %s name myrepo", dir, origin, repo)
		}
	}
}

// A linked worktree lives outside the repository it was cut from, so grouping by
// the enclosing directory would file it under the worktrees root instead.
func TestRepoForDirFollowsWorktreeGitdirPointer(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "Projects", "tandem")
	worktree := filepath.Join(root, ".tandem", "worktrees", "tandem", "meitner-336")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	gitdir := filepath.Join(repo, ".git", "worktrees", "meitner-336")
	if err := os.MkdirAll(gitdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: "+gitdir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	origin := workspace.RepoForDir(worktree)
	if origin.Path != repo || origin.Name != "tandem" {
		t.Fatalf("RepoForDir(worktree) = %+v, want path %s name tandem", origin, repo)
	}
}

// A `gitdir:` shape that is not a linked worktree (a submodule, say) must fall
// through to the directory walk rather than inventing a repository from the
// pointer's own path two levels up.
func TestRepoForDirIgnoresNonWorktreeGitdirPointer(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "super", ".git", "modules", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "elsewhere", "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	pointer := filepath.Join(root, "super", ".git", "modules", "sub")
	if err := os.WriteFile(filepath.Join(sub, ".git"), []byte("gitdir: "+pointer+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	origin := workspace.RepoForDir(sub)
	if origin.Path != sub || origin.Name != "sub" {
		t.Fatalf("RepoForDir(submodule) = %+v, want the directory itself (%s)", origin, sub)
	}
}

// History rows record whatever CWD the vendor transcript held, including paths
// that have since been deleted; those still need a stable grouping key.
func TestRepoForDirFallsBackToTheDirectoryItself(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone", "away")
	origin := workspace.RepoForDir(missing)
	if origin.Path != missing || origin.Name != "away" {
		t.Fatalf("RepoForDir(missing) = %+v, want the directory itself", origin)
	}
	if empty := workspace.RepoForDir(""); empty.Path != "" || empty.Name != "" {
		t.Fatalf("RepoForDir(\"\") = %+v, want zero value", empty)
	}
}
