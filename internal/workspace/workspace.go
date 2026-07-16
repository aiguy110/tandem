// Package workspace manages Tandem's isolated Git worktrees and repository discovery.
package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type Kind string

const (
	KindWorktree Kind = "worktree"
	KindExisting Kind = "existing"
)

type RefKind string

const (
	RefLocalBranch  RefKind = "local-branch"
	RefRemoteBranch RefKind = "remote-branch"
	RefTag          RefKind = "tag"
	RefDetached     RefKind = "detached"
)

type Source struct {
	Ref    string `json:"ref"`
	Commit string `json:"commit,omitempty"`
}
type Integration struct {
	Kind RefKind `json:"kind"`
	Ref  string  `json:"ref"`
}

// Workspace deliberately mirrors the Node SpawnSpec workspace JSON. Fields not
// applicable to KindExisting are omitted, preserving rollback compatibility.
type Workspace struct {
	Kind        Kind         `json:"kind"`
	CWD         string       `json:"cwd,omitempty"`
	Repo        string       `json:"repo,omitempty"`
	Branch      string       `json:"branch,omitempty"`
	BranchMode  string       `json:"branchMode,omitempty"`
	Source      *Source      `json:"source,omitempty"`
	Integration *Integration `json:"integration,omitempty"`
	BaseRef     string       `json:"baseRef,omitempty"`
}

type Config struct{ WorktreesDir string }
type Manager struct {
	worktreesDir string
	git          GitRunner
}
type OccupantFunc func(absDir string) (agentName string, occupied bool)
type ProvisionResult struct {
	CWD           string
	Workspace     Workspace
	CreatedBranch bool
}

type Error struct{ Code, Detail string }

func (e *Error) Error() string           { return e.Code + ": " + e.Detail }
func IsCode(err error, code string) bool { var e *Error; return errors.As(err, &e) && e.Code == code }

// GitRunner centralizes cancellable, shell-free Git invocation and diagnostics.
type GitRunner struct{ Path string }

func (g GitRunner) Run(ctx context.Context, cwd string, args ...string) (string, error) {
	bin := g.Path
	if bin == "" {
		bin = "git"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = cwd
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("git %s (cwd=%s) failed: %s", strings.Join(args, " "), cwd, detail)
	}
	return strings.TrimSpace(stdout.String()), nil
}

func New(config Config) *Manager {
	return &Manager{worktreesDir: config.WorktreesDir, git: GitRunner{}}
}
func NewWithGit(config Config, git GitRunner) *Manager {
	return &Manager{worktreesDir: config.WorktreesDir, git: git}
}

func (m *Manager) Provision(ctx context.Context, ws Workspace, agentName string, occupant OccupantFunc) (ProvisionResult, error) {
	if ws.Kind == KindExisting {
		return m.provisionExisting(ws, occupant)
	}
	if ws.Kind != KindWorktree {
		return ProvisionResult{}, &Error{"invalid_workspace", fmt.Sprintf("unknown workspace kind: %s", ws.Kind)}
	}
	return m.provisionWorktree(ctx, ws, agentName)
}

func (m *Manager) provisionExisting(ws Workspace, occupant OccupantFunc) (ProvisionResult, error) {
	cwd, err := filepath.Abs(ws.CWD)
	if err != nil {
		return ProvisionResult{}, err
	}
	info, statErr := os.Stat(cwd)
	if statErr != nil || !info.IsDir() {
		return ProvisionResult{}, &Error{"no_such_dir", "directory does not exist: " + cwd}
	}
	if occupant != nil {
		if name, ok := occupant(cwd); ok {
			return ProvisionResult{}, &Error{"dir_occupied", fmt.Sprintf(`%s is already in use by agent %q — open a worktree instead, or attach to the existing agent`, cwd, name)}
		}
	}
	return ProvisionResult{CWD: cwd, Workspace: Workspace{Kind: KindExisting, CWD: cwd}}, nil
}

func (m *Manager) provisionWorktree(ctx context.Context, ws Workspace, agentName string) (ProvisionResult, error) {
	repo, err := m.git.Run(ctx, ws.Repo, "rev-parse", "--show-toplevel")
	if err != nil {
		return ProvisionResult{}, err
	}
	cwd := filepath.Join(m.worktreesDir, filepath.Base(repo), agentName)
	if _, err := os.Stat(cwd); err == nil {
		return ProvisionResult{}, &Error{"worktree_exists", "worktree path already exists: " + cwd}
	}
	if err := os.MkdirAll(filepath.Dir(cwd), 0o755); err != nil {
		return ProvisionResult{}, err
	}

	requestedSource := "HEAD"
	if ws.Source != nil && ws.Source.Ref != "" {
		requestedSource = ws.Source.Ref
	} else if ws.BaseRef != "" {
		requestedSource = ws.BaseRef
	}
	source, err := resolveRef(ctx, m.git, repo, requestedSource)
	if err != nil {
		return ProvisionResult{}, err
	}
	requestedIntegration := source.Ref
	if ws.Integration != nil && ws.Integration.Ref != "" {
		requestedIntegration = ws.Integration.Ref
	}
	target, err := resolveRef(ctx, m.git, repo, requestedIntegration)
	if err != nil {
		return ProvisionResult{}, err
	}
	integration := &Integration{Kind: integrationKind(target.Kind), Ref: target.Ref}

	explicit := ws.Branch != ""
	branch := normalizeLocalBranch(ws.Branch)
	if branch == "" {
		branch = defaultAgentBranch(integration.Ref, agentName)
	}
	if _, err := m.git.Run(ctx, repo, "check-ref-format", "--branch", branch); err != nil {
		return ProvisionResult{}, &Error{"invalid_branch", "invalid branch name: " + branch}
	}
	exists := m.branchExists(ctx, repo, branch)
	mode := ws.BranchMode
	if mode == "" {
		if explicit && exists {
			mode = "attach"
		} else {
			mode = "create"
		}
	}
	if mode != "create" && mode != "attach" {
		return ProvisionResult{}, &Error{"invalid_branch_mode", "branchMode must be create or attach"}
	}
	checkedOut, _ := checkedOutPath(ctx, m.git, repo, "refs/heads/"+branch)
	created := false
	if mode == "attach" {
		if !exists {
			return ProvisionResult{}, &Error{"branch_missing", "local branch does not exist: " + branch}
		}
		if checkedOut != "" {
			return ProvisionResult{}, &Error{"branch_checked_out", fmt.Sprintf(`branch %q is already checked out at %s`, branch, checkedOut)}
		}
		source, err = resolveRef(ctx, m.git, repo, "refs/heads/"+branch)
		if err == nil {
			_, err = m.git.Run(ctx, repo, "worktree", "add", cwd, branch)
		}
	} else {
		if exists && explicit {
			return ProvisionResult{}, &Error{"branch_exists", fmt.Sprintf("local branch already exists: %s — choose Continue existing branch or a different agent branch", branch)}
		}
		if exists {
			for i := 2; ; i++ {
				candidate := branch + "-" + strconv.Itoa(i)
				if !m.branchExists(ctx, repo, candidate) {
					branch = candidate
					break
				}
			}
		}
		_, err = m.git.Run(ctx, repo, "worktree", "add", "-b", branch, cwd, source.Commit)
		created = err == nil
	}
	if err != nil {
		return ProvisionResult{}, err
	}
	resolved := Workspace{Kind: KindWorktree, Repo: repo, Branch: branch, BranchMode: mode, Source: &Source{Ref: source.Ref, Commit: source.Commit}, Integration: integration, BaseRef: source.Commit}
	return ProvisionResult{CWD: cwd, Workspace: resolved, CreatedBranch: created}, nil
}

func (m *Manager) branchExists(ctx context.Context, repo, branch string) bool {
	_, err := m.git.Run(ctx, repo, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}
func (m *Manager) IsDirty(ctx context.Context, cwd string) (bool, error) {
	out, err := m.git.Run(ctx, cwd, "status", "--porcelain")
	return out != "", err
}

type GitState struct {
	Status    string `json:"status"`
	Ahead     int    `json:"ahead"`
	Behind    int    `json:"behind"`
	TargetRef string `json:"targetRef,omitempty"`
}

func (m *Manager) GitState(ctx context.Context, cwd string, ws Workspace) (GitState, error) {
	dirty, err := m.IsDirty(ctx, cwd)
	if err != nil {
		return GitState{}, err
	}
	if ws.Kind != KindWorktree {
		status := "synced"
		if dirty {
			status = "dirty"
		}
		return GitState{Status: status}, nil
	}
	target, err := m.targetRef(ctx, ws)
	if err != nil {
		return GitState{}, err
	}
	if _, err := m.git.Run(ctx, ws.Repo, "rev-parse", "--verify", target+"^{commit}"); err != nil {
		status := "target_missing"
		if dirty {
			status = "dirty"
		}
		return GitState{Status: status, TargetRef: target}, nil
	}
	ahead, behind, err := aheadBehind(ctx, m.git, cwd, target, "HEAD")
	if err != nil {
		return GitState{}, err
	}
	status := "synced"
	switch {
	case dirty:
		status = "dirty"
	case ahead > 0 && behind > 0:
		status = "diverged"
	case ahead > 0:
		status = "ahead"
	case behind > 0:
		head, e := m.git.Run(ctx, cwd, "rev-parse", "HEAD")
		if e != nil {
			return GitState{}, e
		}
		status = "behind"
		if ws.Source != nil && ws.Source.Commit != "" && head != ws.Source.Commit {
			status = "merged"
		}
	}
	return GitState{Status: status, Ahead: ahead, Behind: behind, TargetRef: target}, nil
}

type ClosePreview struct {
	Kind        Kind   `json:"kind"`
	Uncommitted string `json:"uncommitted"`
	Unmerged    string `json:"unmerged"`
	TargetRef   string `json:"targetRef,omitempty"`
	Ahead       *int   `json:"ahead,omitempty"`
	Behind      *int   `json:"behind,omitempty"`
}

func (m *Manager) ClosePreview(ctx context.Context, cwd string, ws Workspace) (ClosePreview, error) {
	uncommitted, err := m.git.Run(ctx, cwd, "status", "--short")
	if err != nil {
		return ClosePreview{}, err
	}
	p := ClosePreview{Kind: ws.Kind, Uncommitted: uncommitted}
	if ws.Kind != KindWorktree {
		return p, nil
	}
	p.TargetRef, err = m.targetRef(ctx, ws)
	if err != nil {
		return ClosePreview{}, err
	}
	a, b, countErr := aheadBehind(ctx, m.git, cwd, p.TargetRef, "HEAD")
	if countErr != nil {
		p.Unmerged = "[integration target missing: " + p.TargetRef + "]"
		return p, nil
	}
	p.Ahead, p.Behind = &a, &b
	p.Unmerged, err = m.git.Run(ctx, cwd, "log", "--oneline", p.TargetRef+"..HEAD")
	return p, err
}

func (m *Manager) targetRef(ctx context.Context, ws Workspace) (string, error) {
	if ws.Integration != nil && ws.Integration.Ref != "" {
		return ws.Integration.Ref, nil
	}
	if ws.Source != nil && ws.Source.Ref != "" {
		return ws.Source.Ref, nil
	}
	ref, err := m.git.Run(ctx, ws.Repo, "symbolic-ref", "-q", "HEAD")
	if err == nil {
		return ref, nil
	}
	if ws.BaseRef != "" {
		return ws.BaseRef, nil
	}
	return "HEAD", nil
}

// Rollback removes a provisioned checkout and, only when newly created, its branch.
func (m *Manager) Rollback(ctx context.Context, ws Workspace, cwd string, createdBranch bool) {
	if ws.Kind != KindWorktree {
		return
	}
	if _, err := os.Stat(cwd); err == nil {
		_, _ = m.git.Run(ctx, ws.Repo, "worktree", "remove", "--force", cwd)
	}
	_, _ = m.git.Run(ctx, ws.Repo, "worktree", "prune")
	if createdBranch && ws.Branch != "" {
		_, _ = m.git.Run(ctx, ws.Repo, "branch", "-D", ws.Branch)
	}
}

func (m *Manager) Teardown(ctx context.Context, ws Workspace, cwd string, force bool) error {
	if ws.Kind == KindExisting {
		return nil
	}
	if _, err := os.Stat(cwd); os.IsNotExist(err) {
		_, _ = m.git.Run(ctx, ws.Repo, "worktree", "prune")
		return nil
	}
	if !force {
		dirty, err := m.IsDirty(ctx, cwd)
		if err != nil {
			return err
		}
		if dirty {
			return &Error{"dirty_worktree", fmt.Sprintf(`%s has uncommitted changes — close with force:true to override (branch %q is kept either way)`, cwd, ws.Branch)}
		}
	}
	args := []string{"worktree", "remove", cwd}
	if force {
		args = append(args, "--force")
	}
	_, err := m.git.Run(ctx, ws.Repo, args...)
	return err
}

func (m *Manager) Reattach(ctx context.Context, ws Workspace, cwd string) (string, error) {
	if ws.Kind == KindExisting {
		info, err := os.Stat(cwd)
		if err != nil || !info.IsDir() {
			return "", &Error{"no_such_dir", "directory no longer exists: " + cwd}
		}
		return cwd, nil
	}
	if _, err := os.Stat(cwd); err == nil {
		return cwd, nil
	}
	if ws.Branch == "" {
		return "", &Error{"branch_missing", "persisted worktree has no branch"}
	}
	if err := os.MkdirAll(filepath.Dir(cwd), 0o755); err != nil {
		return "", err
	}
	_, _ = m.git.Run(ctx, ws.Repo, "worktree", "prune")
	_, err := m.git.Run(ctx, ws.Repo, "worktree", "add", cwd, ws.Branch)
	if err != nil {
		return "", err
	}
	return cwd, nil
}

type resolvedRef struct {
	Ref, Commit string
	Kind        RefKind
}

func resolveRef(ctx context.Context, git GitRunner, repo, requested string) (resolvedRef, error) {
	commit, err := git.Run(ctx, repo, "rev-parse", "--verify", requested+"^{commit}")
	if err != nil {
		return resolvedRef{}, &Error{"ref_not_found", "Git ref does not resolve to a commit: " + requested}
	}
	ref, _ := git.Run(ctx, repo, "rev-parse", "--symbolic-full-name", requested)
	if ref == "" && requested == "HEAD" {
		ref, _ = git.Run(ctx, repo, "symbolic-ref", "-q", "HEAD")
	}
	if ref == "" {
		ref = commit
	}
	return resolvedRef{ref, commit, refKind(ref)}, nil
}
func refKind(ref string) RefKind {
	if strings.HasPrefix(ref, "refs/heads/") {
		return RefLocalBranch
	}
	if strings.HasPrefix(ref, "refs/remotes/") {
		return RefRemoteBranch
	}
	if strings.HasPrefix(ref, "refs/tags/") {
		return RefTag
	}
	return RefDetached
}
func integrationKind(kind RefKind) RefKind {
	if kind == RefLocalBranch || kind == RefRemoteBranch {
		return kind
	}
	return RefDetached
}
func displayRef(ref string) string {
	for _, p := range []string{"refs/heads/", "refs/remotes/", "refs/tags/"} {
		if strings.HasPrefix(ref, p) {
			return strings.TrimPrefix(ref, p)
		}
	}
	return ref
}
func normalizeLocalBranch(branch string) string { return strings.TrimPrefix(branch, "refs/heads/") }

var invalidContext = regexp.MustCompile(`[^A-Za-z0-9._/-]+`)
var invalidAgent = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func defaultAgentBranch(integrationRef, agentName string) string {
	contextName := displayRef(integrationRef)
	if strings.HasPrefix(contextName, "origin/") {
		contextName = strings.TrimPrefix(contextName, "origin/")
	}
	contextName = strings.TrimPrefix(contextName, "feature/")
	contextName = invalidContext.ReplaceAllString(contextName, "-")
	contextName = strings.ReplaceAll(contextName, "/", "-")
	contextName = strings.TrimRight(contextName, ".")
	contextName = strings.Trim(contextName, "-./")
	if len(contextName) > 64 {
		contextName = contextName[:64]
	}
	if contextName == "" {
		contextName = "detached"
	}
	agent := strings.Trim(invalidAgent.ReplaceAllString(agentName, "-"), "-.")
	if agent == "" {
		agent = "agent"
	}
	return "tandem/" + contextName + "/" + agent
}
func worktreeBranches(ctx context.Context, git GitRunner, repo string) (map[string]string, error) {
	out, err := git.Run(ctx, repo, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	result := map[string]string{}
	var wt string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "worktree ") {
			wt = strings.TrimPrefix(line, "worktree ")
		} else if strings.HasPrefix(line, "branch ") && wt != "" {
			result[strings.TrimPrefix(line, "branch ")] = wt
		} else if line == "" {
			wt = ""
		}
	}
	return result, nil
}
func checkedOutPath(ctx context.Context, git GitRunner, repo, ref string) (string, error) {
	m, e := worktreeBranches(ctx, git, repo)
	return m[ref], e
}
func aheadBehind(ctx context.Context, git GitRunner, cwd, target, branch string) (int, int, error) {
	raw, e := git.Run(ctx, cwd, "rev-list", "--left-right", "--count", target+"..."+branch)
	if e != nil {
		return 0, 0, e
	}
	f := strings.Fields(raw)
	if len(f) != 2 {
		return 0, 0, fmt.Errorf("unexpected rev-list count %q", raw)
	}
	behind, e := strconv.Atoi(f[0])
	if e != nil {
		return 0, 0, e
	}
	ahead, e := strconv.Atoi(f[1])
	return ahead, behind, e
}

// GitRefInfo is a read-only spawn-picker view of a ref.
type GitRefInfo struct {
	Ref          string  `json:"ref"`
	DisplayName  string  `json:"displayName"`
	Kind         RefKind `json:"kind"`
	Commit       string  `json:"commit"`
	Subject      string  `json:"subject,omitempty"`
	UpdatedAt    string  `json:"updatedAt,omitempty"`
	Upstream     string  `json:"upstream,omitempty"`
	Ahead        *int    `json:"ahead,omitempty"`
	Behind       *int    `json:"behind,omitempty"`
	CheckedOutAt string  `json:"checkedOutAt,omitempty"`
	IsCurrent    bool    `json:"isCurrent"`
	IsDefault    bool    `json:"isDefault"`
}

func ListGitRefs(ctx context.Context, repo string) ([]GitRefInfo, error) {
	return ListGitRefsWith(ctx, GitRunner{}, repo)
}
func ListGitRefsWith(ctx context.Context, git GitRunner, repo string) ([]GitRefInfo, error) {
	root, e := git.Run(ctx, repo, "rev-parse", "--show-toplevel")
	if e != nil {
		return nil, e
	}
	raw, e := git.Run(ctx, root, "for-each-ref", "--format=%(refname)%1f%(objectname)%1f%(subject)%1f%(committerdate:iso-strict)%1f%(upstream:short)%00", "refs/heads", "refs/remotes", "refs/tags")
	if e != nil {
		return nil, e
	}
	current, _ := git.Run(ctx, root, "symbolic-ref", "-q", "HEAD")
	def, _ := git.Run(ctx, root, "symbolic-ref", "-q", "refs/remotes/origin/HEAD")
	checked, e := worktreeBranches(ctx, git, root)
	if e != nil {
		return nil, e
	}
	var refs []GitRefInfo
	for _, record := range strings.Split(raw, "\x00") {
		record = strings.TrimSpace(record)
		if record == "" {
			continue
		}
		f := strings.Split(record, "\x1f")
		if len(f) < 5 {
			continue
		}
		symbolic, _ := git.Run(ctx, root, "symbolic-ref", "-q", f[0])
		if symbolic != "" {
			continue
		}
		item := GitRefInfo{Ref: f[0], DisplayName: displayRef(f[0]), Kind: refKind(f[0]), Commit: f[1], Subject: f[2], UpdatedAt: f[3], Upstream: f[4], CheckedOutAt: checked[f[0]], IsCurrent: f[0] == current, IsDefault: f[0] == def || (def == "" && f[0] == current)}
		if item.Kind == RefLocalBranch && item.Upstream != "" {
			a, b, err := aheadBehind(ctx, git, root, item.Upstream, item.Ref)
			if err == nil {
				item.Ahead = &a
				item.Behind = &b
			}
		}
		refs = append(refs, item)
	}
	rank := func(r GitRefInfo) int {
		if r.IsCurrent {
			return 0
		}
		if r.IsDefault {
			return 1
		}
		if r.Kind == RefLocalBranch {
			return 2
		}
		if r.Kind == RefRemoteBranch {
			return 3
		}
		return 4
	}
	sort.Slice(refs, func(i, j int) bool {
		ri, rj := rank(refs[i]), rank(refs[j])
		if ri != rj {
			return ri < rj
		}
		return refs[i].DisplayName < refs[j].DisplayName
	})
	return refs, nil
}

type RepoInfo struct {
	Path          string `json:"path"`
	Name          string `json:"name"`
	CurrentBranch string `json:"currentBranch"`
	Dirty         bool   `json:"dirty"`
	HasLiveAgent  bool   `json:"hasLiveAgent"`
}

var skipDirs = map[string]bool{"node_modules": true, "dist": true, "build": true, ".next": true, "target": true, "vendor": true, ".venv": true, "__pycache__": true}

func ListRepos(ctx context.Context, roots []string, depth int, live func(string) bool) ([]RepoInfo, error) {
	return ListReposWith(ctx, GitRunner{}, roots, depth, live)
}
func ListReposWith(ctx context.Context, git GitRunner, roots []string, depth int, live func(string) bool) ([]RepoInfo, error) {
	var out []RepoInfo
	seen := map[string]bool{}
	for _, root := range roots {
		dirs := findGitRepos(ctx, root, depth)
		for _, dir := range dirs {
			real, e := filepath.Abs(dir)
			if e != nil || seen[real] {
				continue
			}
			seen[real] = true
			branch, _ := git.Run(ctx, real, "rev-parse", "--abbrev-ref", "HEAD")
			status, _ := git.Run(ctx, real, "status", "--porcelain")
			occupied := false
			if live != nil {
				occupied = live(real)
			}
			out = append(out, RepoInfo{real, filepath.Base(real), branch, status != "", occupied})
		}
	}
	return out, nil
}
func findGitRepos(ctx context.Context, root string, maxDepth int) []string {
	var found []string
	var walk func(string, int)
	walk = func(dir string, left int) {
		if ctx.Err() != nil {
			return
		}
		entries, e := os.ReadDir(dir)
		if e != nil {
			return
		}
		for _, x := range entries {
			if x.Name() == ".git" {
				found = append(found, dir)
				return
			}
		}
		if left <= 0 {
			return
		}
		for _, x := range entries {
			if !x.IsDir() || strings.HasPrefix(x.Name(), ".") || skipDirs[x.Name()] {
				continue
			}
			walk(filepath.Join(dir, x.Name()), left-1)
		}
	}
	walk(root, maxDepth)
	return found
}
