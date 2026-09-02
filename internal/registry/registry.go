// Package registry is the authoritative in-memory owner of live agents.
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aiguy110/tandem/internal/acp"
	"github.com/aiguy110/tandem/internal/acpadapter"
	"github.com/aiguy110/tandem/internal/agentadapter"
	"github.com/aiguy110/tandem/internal/assets"
	"github.com/aiguy110/tandem/internal/browser"
	"github.com/aiguy110/tandem/internal/config"
	"github.com/aiguy110/tandem/internal/eventlog"
	"github.com/aiguy110/tandem/internal/session"
	"github.com/aiguy110/tandem/internal/store"
	"github.com/aiguy110/tandem/internal/uploads"
	"github.com/aiguy110/tandem/internal/workspace"
	"github.com/aiguy110/tandem/internal/workspacefs"
)

var nameWords = []string{
	"einstein",
	"curie",
	"newton",
	"feynman",
	"maxwell",
	"faraday",
	"bohr",
	"planck",
	"dirac",
	"noether",
	"hawking",
	"galileo",
	"tesla",
	"rutherford",
	"wu",
	"meitner",
	"pauli",
	"chandrasekhar",
	"bell",
	"hubble",
}

// Save stores a browser-selected file in the live agent's workspace.
func (r *Registry) Save(id, name string, data []byte) (string, error) {
	r.mu.RLock()
	_, found := r.sessions[id]
	cwd := r.cwds[id]
	r.mu.RUnlock()
	if !found || cwd == "" {
		return "", errors.New("no such live agent")
	}
	return uploads.Save(cwd, name, data)
}

// HasConfiguredDirectory reports whether an agent's workspace opted into
// retaining uploads in a repository-relative directory.
func (r *Registry) HasConfiguredDirectory(id string) (bool, error) {
	r.mu.RLock()
	_, found := r.sessions[id]
	cwd := r.cwds[id]
	r.mu.RUnlock()
	if !found || cwd == "" {
		return false, errors.New("no such live agent")
	}
	return uploads.HasConfiguredDirectory(cwd)
}

type Registry struct {
	mu                sync.RWMutex
	store             *store.Store
	config            config.Config
	workspace         *workspace.Manager
	factory           agentadapter.Factory
	browser           *browser.Broker
	ring              int
	sessions          map[string]*session.Session
	cwds              map[string]string
	known             map[string]bool
	counter           int
	handoffs          map[string]*sync.Mutex
	external          *externalCache
	onSession         func(*session.Session)
	onAudioPreference func(*session.Session, bool)
	onAudioFocus      func(*session.Session, string, bool)
}

type externalCache struct {
	at       time.Time
	sessions []ResumableSession
	adapters []ResumeAdapterInfo
}

type SummaryWorkspace struct {
	Kind        workspace.Kind    `json:"kind"`
	Repo        string            `json:"repo"`
	RepoPath    string            `json:"repoPath"`
	Branch      string            `json:"branch"`
	CWD         string            `json:"cwd"`
	GitState    string            `json:"gitState,omitempty"`
	Ahead       int               `json:"ahead"`
	Behind      int               `json:"behind"`
	TargetRef   string            `json:"targetRef,omitempty"`
	TargetKind  workspace.RefKind `json:"targetKind,omitempty"`
	StartCommit string            `json:"startCommit,omitempty"`
}

// SummaryProfile is the launch profile resolved for an agent. It is included
// in agent summaries so views outside the spawn flow can identify the exact
// settings the running session was created with.
type SummaryProfile struct {
	ID         string `json:"id,omitempty"`
	Model      string `json:"model,omitempty"`
	Effort     string `json:"effort,omitempty"`
	Permission string `json:"permission,omitempty"`
	Snapshot   string `json:"snapshot,omitempty"`
}

type Summary struct {
	ID               string           `json:"id"`
	Name             string           `json:"name"`
	Agent            string           `json:"agent,omitempty"`
	Workspace        SummaryWorkspace `json:"workspace"`
	Status           session.Status   `json:"status"`
	PendingApprovals int              `json:"pendingApprovals"`
	ControlMode      string           `json:"controlMode"`
	// Adapter is the agent's stable adapter kind ("acp" or "pty"); it does not
	// change across an ACP↔CLI handoff. CanHandoff is true when the agent
	// supports swapping to its resumable CLI (the Chat tab's ACP/CLI switch).
	Adapter    string          `json:"adapter"`
	CanHandoff bool            `json:"canHandoff"`
	Profile    *SummaryProfile `json:"profile,omitempty"`
}

// WorkspaceEntry is one immediate file-system completion candidate. Paths are
// relative to an agent workspace, or begin with ../ when browsing its parent.
// Directory paths are distinguished so the UI can continue into them without
// having to infer their type from a trailing slash.
type WorkspaceEntry struct {
	Path  string `json:"path"`
	IsDir bool   `json:"isDir"`
}

type CatalogAgent struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	HasACP      bool   `json:"hasAcp"`
	HasTerminal bool   `json:"hasTerminal"`
	CanResume   bool   `json:"canResume"`
}
type CatalogHarness struct {
	ID           string   `json:"id"`
	Agent        string   `json:"agent"`
	Name         string   `json:"name"`
	ACPArgs      []string `json:"acpArgs"`
	TerminalArgs []string `json:"terminalArgs"`
}
type Catalog struct {
	DefaultAgent   string           `json:"defaultAgent"`
	DefaultHarness string           `json:"defaultHarness,omitempty"`
	Agents         []CatalogAgent   `json:"agents"`
	Harnesses      []CatalogHarness `json:"harnesses"`
}
type SpawnOptions struct {
	Modes         json.RawMessage   `json:"modes"`
	ConfigOptions []json.RawMessage `json:"configOptions"`
}

type persistedSessionConfig struct {
	ModeID        string         `json:"modeId,omitempty"`
	ConfigOptions map[string]any `json:"configOptions,omitempty"`
}

type ResumableSession struct {
	SessionID string `json:"sessionId"`
	Source    string `json:"source"`
	Agent     string `json:"agent"`
	Adapter   string `json:"adapter,omitempty"`
	CWD       string `json:"cwd"`
	// Repo/RepoPath attribute the session to a source repository so History can
	// group by it. RepoPath is the grouping identity; Repo is its display name.
	Repo        string         `json:"repo,omitempty"`
	RepoPath    string         `json:"repoPath,omitempty"`
	Title       string         `json:"title,omitempty"`
	UpdatedAt   string         `json:"updatedAt,omitempty"`
	AgentID     string         `json:"agentId,omitempty"`
	AgentName   string         `json:"agentName,omitempty"`
	Branch      string         `json:"branch,omitempty"`
	Live        *bool          `json:"live,omitempty"`
	Closed      *bool          `json:"closed,omitempty"`
	Status      session.Status `json:"status,omitempty"`
	Resumable   bool           `json:"resumable"`
	HistoryOnly bool           `json:"historyOnly,omitempty"`
	ResumeError string         `json:"resumeError,omitempty"`
}
type ResumeAdapterInfo struct {
	Agent        string `json:"agent"`
	SupportsList bool   `json:"supportsList"`
}
type ResumeCatalog struct {
	Sessions []ResumableSession  `json:"sessions"`
	Adapters []ResumeAdapterInfo `json:"adapters"`
}

type SessionSearchHit struct {
	EntryID   string                `json:"entryId"`
	Role      string                `json:"role,omitempty"`
	Kind      string                `json:"kind,omitempty"`
	Timestamp string                `json:"timestamp,omitempty"`
	Match     store.HistoryExcerpt  `json:"match"`
	Before    *store.HistoryExcerpt `json:"before,omitempty"`
	After     *store.HistoryExcerpt `json:"after,omitempty"`
}

type SessionSearchResult struct {
	Session ResumableSession   `json:"session"`
	Score   float64            `json:"score"`
	Hits    []SessionSearchHit `json:"hits"`
}
type Options struct {
	Store             *store.Store
	Config            config.Config
	Workspace         *workspace.Manager
	Factory           agentadapter.Factory
	Assets            *assets.Store
	Browser           *browser.Broker
	RingCapacity      int
	OnSession         func(*session.Session)
	OnAudioPreference func(*session.Session, bool)
	OnAudioFocus      func(*session.Session, string, bool)
}

func New(o Options) (*Registry, error) {
	if o.Store == nil {
		return nil, errors.New("registry: store is required")
	}
	if o.Workspace == nil {
		o.Workspace = workspace.New(workspace.Config{WorktreesDir: o.Config.WorktreesDir})
	}
	if o.Factory == nil {
		assetStore := o.Assets
		if assetStore == nil {
			var err error
			assetStore, err = assets.Open(o.Config.AssetsDir, o.Store)
			if err != nil {
				return nil, err
			}
		}
		o.Factory = DefaultFactory{Assets: assetStore, Config: o.Config}
	}
	max, err := o.Store.MaxAgentSuffix()
	if err != nil {
		return nil, err
	}
	all, err := o.Store.AllAgents()
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, a := range all {
		known[a.ID] = true
	}
	cap := o.RingCapacity
	if cap == 0 {
		cap = 1000
	}
	return &Registry{store: o.Store, config: o.Config, workspace: o.Workspace, factory: o.Factory, browser: o.Browser, ring: cap, sessions: map[string]*session.Session{}, cwds: map[string]string{}, known: known, counter: max, handoffs: map[string]*sync.Mutex{}, onSession: o.OnSession, onAudioPreference: o.OnAudioPreference, onAudioFocus: o.OnAudioFocus}, nil
}

func (r *Registry) SetAudioEnabled(id string, enabled bool) error {
	s := r.Get(id)
	if s == nil {
		return errors.New("no such live agent")
	}
	if err := r.store.SetAgentAudioEnabled(id, enabled, s.Log.Head()); err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]any{"kind": "audio_preference", "enabled": enabled})
	s.PushEvent(eventlog.Event{Kind: "audio_preference", Payload: payload})
	if enabled && r.onAudioPreference != nil {
		r.onAudioPreference(s, true)
	}
	return nil
}

// SetAudioFocus marks whether one connected browser is actively viewing an
// agent's chat. Focus is intentionally connection-scoped and never persisted.
func (r *Registry) SetAudioFocus(id, clientID string, focused bool) error {
	s := r.Get(id)
	if s == nil {
		return errors.New("no such live agent")
	}
	if r.onAudioFocus != nil {
		r.onAudioFocus(s, clientID, focused)
	}
	return nil
}

func (r *Registry) Get(id string) *session.Session {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.sessions[id]
}
func (r *Registry) List() []*session.Session {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*session.Session, 0, len(r.sessions))
	for _, s := range r.sessions {
		out = append(out, s)
	}
	return out
}

func (r *Registry) Summaries(ctx context.Context) []Summary {
	r.mu.RLock()
	type item struct {
		s   *session.Session
		cwd string
	}
	items := make([]item, 0, len(r.sessions))
	for id, s := range r.sessions {
		items = append(items, item{s, r.cwds[id]})
	}
	r.mu.RUnlock()
	out := make([]Summary, 0, len(items))
	for _, item := range items {
		s, cwd, ws := item.s, item.cwd, item.s.Spec.Workspace
		state, err := r.workspace.GitState(ctx, cwd, ws)
		if err != nil {
			// Do not let a failed git query leave the client displaying the last
			// known state as if it were still authoritative.
			state.Status = "unknown"
		}
		repoPath, repo, branch := ws.Repo, filepath.Base(ws.Repo), ws.Branch
		if ws.Kind == workspace.KindExisting {
			repoPath, repo, branch = ws.CWD, filepath.Base(ws.CWD), ""
		}
		agent := s.Spec.Agent
		if agent == "" {
			agent = r.config.ACP.Default
		}
		summary := Summary{ID: s.ID, Name: s.DisplayName(), Agent: agent, Status: s.Status(), PendingApprovals: len(s.PendingApprovals()), ControlMode: s.ControlMode(), Adapter: s.Spec.Adapter, CanHandoff: canHandoff(s.Spec)}
		if p := s.Spec.Profile; p != nil {
			summary.Profile = &SummaryProfile{ID: p.ID, Model: p.Model, Effort: p.Effort, Permission: p.Permission, Snapshot: p.Snapshot}
		}
		summary.Workspace = SummaryWorkspace{Kind: ws.Kind, Repo: repo, RepoPath: repoPath, Branch: branch, CWD: cwd, GitState: state.Status, Ahead: state.Ahead, Behind: state.Behind, TargetRef: state.TargetRef}
		if ws.Integration != nil {
			summary.Workspace.TargetKind = ws.Integration.Kind
			if summary.Workspace.TargetRef == "" {
				summary.Workspace.TargetRef = ws.Integration.Ref
			}
		}
		if ws.Source != nil {
			summary.Workspace.StartCommit = ws.Source.Commit
		}
		out = append(out, summary)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (r *Registry) ListDirs(ctx context.Context) ([]workspace.RepoInfo, error) {
	r.mu.RLock()
	repos := make(map[string]bool, len(r.sessions))
	for _, s := range r.sessions {
		p := s.Spec.Workspace.Repo
		if s.Spec.Workspace.Kind == workspace.KindExisting {
			p = s.Spec.Workspace.CWD
		}
		if abs, err := filepath.Abs(p); err == nil {
			repos[abs] = true
		}
	}
	r.mu.RUnlock()
	return workspace.ListRepos(ctx, r.config.ProjectRoots, r.config.DirScanDepth, func(p string) bool { abs, _ := filepath.Abs(p); return repos[abs] })
}

// ListWorkspaceEntries returns the direct children of dir in a live agent's
// workspace, its immediate parent, or the daemon user's home directory. The
// workspacefs boundary protects each root from path and symlink escapes even
// though this is a browser-facing convenience API.
func (r *Registry) ListWorkspaceEntries(ctx context.Context, id, dir string) ([]WorkspaceEntry, error) {
	// This browser protocol is deliberately relative even though workspacefs can
	// safely accept an absolute path within the root. Keeping the public result
	// relative makes it directly insertable as an @ mention and prevents a
	// malformed client request from changing that contract. A single leading
	// "../" opts into the workspace's immediate parent, and "~/" opts into the
	// daemon user's home directory; neither can escape its selected root.
	if filepath.IsAbs(dir) {
		return nil, errors.New("workspace path must be relative")
	}
	r.mu.RLock()
	_, found := r.sessions[id]
	cwd := r.cwds[id]
	r.mu.RUnlock()
	if !found || cwd == "" {
		return nil, errors.New("no such live agent")
	}
	root, fsDir := cwd, ""
	if dir == "~" || strings.HasPrefix(dir, "~"+string(filepath.Separator)) {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("find user home directory: %w", err)
		}
		root = home
		fsDir = strings.TrimPrefix(dir, "~")
		fsDir = strings.TrimPrefix(fsDir, string(filepath.Separator))
		fsDir = filepath.Clean(fsDir)
		if fsDir == "." {
			fsDir = ""
		}
		if fsDir == ".." || strings.HasPrefix(fsDir, ".."+string(filepath.Separator)) {
			return nil, errors.New("home path escapes its root")
		}
		dir = "~"
		if fsDir != "" {
			dir = filepath.Join(dir, fsDir)
		}
	} else {
		dir = filepath.Clean(dir)
		if dir == "." {
			dir = ""
		}
		fsDir = dir
	}
	if dir == ".." {
		root, fsDir = filepath.Dir(cwd), ""
	} else if strings.HasPrefix(dir, ".."+string(filepath.Separator)) {
		root, fsDir = filepath.Dir(cwd), strings.TrimPrefix(dir, ".."+string(filepath.Separator))
		if fsDir == ".." || strings.HasPrefix(fsDir, ".."+string(filepath.Separator)) {
			return nil, errors.New("workspace path escapes its parent")
		}
	}
	fsys, err := workspacefs.Open(root)
	if err != nil {
		return nil, err
	}
	defer fsys.Close()
	entries, err := fsys.ReadDir(fsDir)
	if err != nil {
		return nil, err
	}
	result := make([]WorkspaceEntry, 0, len(entries))
	for _, entry := range entries {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		// Git internals are not useful prompt references and can be enormous.
		if entry.Name() == ".git" {
			continue
		}
		result = append(result, WorkspaceEntry{
			Path:  filepath.ToSlash(filepath.Join(dir, entry.Name())),
			IsDir: entry.IsDir(),
		})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].IsDir != result[j].IsDir {
			return result[i].IsDir
		}
		return strings.ToLower(result[i].Path) < strings.ToLower(result[j].Path)
	})
	return result, nil
}

func (r *Registry) ListGitRefs(ctx context.Context, repo string) ([]workspace.GitRefInfo, error) {
	refs, err := workspace.ListGitRefs(ctx, repo)
	if err != nil {
		return nil, err
	}
	repoRoot, err := filepath.Abs(repo)
	if err != nil {
		return nil, err
	}
	rows, err := r.store.AllAgents()
	if err != nil {
		return nil, err
	}
	byBranch := make(map[string]workspace.GitRefTandem)
	for _, row := range rows {
		var spec agentadapter.Spec
		if json.Unmarshal(row.Spec, &spec) != nil || spec.Workspace.Kind != workspace.KindWorktree || spec.Workspace.Branch == "" {
			continue
		}
		storedRoot, absErr := filepath.Abs(spec.Workspace.Repo)
		if absErr != nil || storedRoot != repoRoot {
			continue
		}
		ref := "refs/heads/" + spec.Workspace.Branch
		if _, exists := byBranch[ref]; exists {
			continue
		}
		meta := workspace.GitRefTandem{AgentID: row.ID, AgentName: row.Name, Live: r.Get(row.ID) != nil, Closed: row.ClosedAt != nil}
		if integration := spec.Workspace.Integration; integration != nil {
			meta.IntegrationRef, meta.IntegrationKind = integration.Ref, integration.Kind
		}
		byBranch[ref] = meta
	}
	for i := range refs {
		if meta, ok := byBranch[refs[i].Ref]; ok {
			copy := meta
			refs[i].Tandem = &copy
		}
	}
	return refs, nil
}

// OpenUserShell lazily spawns the user's escape-hatch shell (Terminal tab) in
// the agent's worktree. It runs independently of the agent adapter, so it is
// unaffected by ACP↔CLI handoffs.
func (r *Registry) OpenUserShell(id string, cols, rows uint16) error {
	r.mu.RLock()
	s, cwd := r.sessions[id], r.cwds[id]
	r.mu.RUnlock()
	if s == nil {
		return fmt.Errorf("no such agent: %s", id)
	}
	return s.OpenUserShell(cwd, cols, rows)
}

func (r *Registry) ClosePreview(ctx context.Context, id string) (*workspace.ClosePreview, error) {
	r.mu.RLock()
	s, cwd := r.sessions[id], r.cwds[id]
	r.mu.RUnlock()
	if s == nil {
		return nil, nil
	}
	preview, err := r.workspace.ClosePreview(ctx, cwd, s.Spec.Workspace)
	if err != nil {
		return nil, err
	}
	return &preview, nil
}

func (r *Registry) Diff(ctx context.Context, id string) (*workspace.Diff, error) {
	s := r.Get(id)
	if s == nil {
		return nil, fmt.Errorf("no such agent: %s", id)
	}
	r.mu.RLock()
	cwd := r.cwds[id]
	r.mu.RUnlock()
	diff, err := r.workspace.Diff(ctx, cwd, s.Spec.Workspace)
	return &diff, err
}

func (r *Registry) AgentCatalog() Catalog {
	c := Catalog{DefaultAgent: r.config.ACP.Default, DefaultHarness: r.config.DefaultHarness, Agents: make([]CatalogAgent, 0, len(r.config.Agents)), Harnesses: make([]CatalogHarness, 0, len(r.config.Harnesses))}
	for id, a := range r.config.Agents {
		c.Agents = append(c.Agents, CatalogAgent{ID: id, Name: a.Name, HasACP: a.ACP != nil, HasTerminal: a.Terminal != nil, CanResume: a.Terminal != nil && len(a.Terminal.Args) > 0})
	}
	for id, h := range r.config.Harnesses {
		c.Harnesses = append(c.Harnesses, CatalogHarness{ID: id, Agent: h.Agent, Name: h.Name, ACPArgs: append([]string{}, h.ACPArgs...), TerminalArgs: append([]string{}, h.TerminalArgs...)})
	}
	sort.Slice(c.Agents, func(i, j int) bool { return c.Agents[i].ID < c.Agents[j].ID })
	sort.Slice(c.Harnesses, func(i, j int) bool { return c.Harnesses[i].ID < c.Harnesses[j].ID })
	return c
}

// SpawnOptions probes an ACP session without registering an agent or
// provisioning a workspace. It mirrors the advanced spawn palette's Node
// behavior and always tears the probe process down.
func (r *Registry) SpawnOptions(ctx context.Context, agent, harness string, acpArgs []string, cwd string) (SpawnOptions, error) {
	spec, err := r.resolve(agentadapter.Spec{Adapter: "acp", Agent: agent, Harness: harness, ACPArgs: acpArgs, Workspace: workspace.Workspace{Kind: workspace.KindExisting, CWD: cwd}})
	if err != nil {
		return SpawnOptions{}, err
	}
	a, err := r.factory.Start(ctx, agentadapter.StartRequest{AgentID: "spawn-options-" + agent, CWD: cwd, Spec: spec})
	if err != nil {
		return SpawnOptions{}, err
	}
	defer a.Close(context.Background())
	provider, ok := a.(interface {
		SessionState() acpadapter.SessionState
	})
	if !ok {
		return SpawnOptions{}, errors.New("adapter does not expose spawn options")
	}
	state := provider.SessionState()
	modes := state.Modes
	if len(modes) == 0 {
		modes = json.RawMessage("null")
	}
	options := state.ConfigOptions
	if options == nil {
		options = []json.RawMessage{}
	}
	return SpawnOptions{Modes: modes, ConfigOptions: options}, nil
}
func (r *Registry) ActiveTurnCount() int {
	n := 0
	for _, s := range r.List() {
		if s.ActiveTurn() {
			n++
		}
	}
	return n
}
func (r *Registry) nextName(desired string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if desired == "" {
		r.counter++
		desired = fmt.Sprintf("%s-%d", nameWords[(r.counter-1)%len(nameWords)], r.counter)
	}
	candidate := desired
	for i := 2; r.known[candidate]; i++ {
		candidate = fmt.Sprintf("%s-%d", desired, i)
	}
	r.known[candidate] = true
	return candidate
}
func (r *Registry) releaseKnown(id string) { r.mu.Lock(); delete(r.known, id); r.mu.Unlock() }

func (r *Registry) Rename(agentID, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("agent name cannot be empty")
	}
	if len([]rune(name)) > 80 {
		return errors.New("agent name cannot exceed 80 characters")
	}
	s := r.Get(agentID)
	if s == nil {
		return errors.New("no such agent")
	}
	if err := r.store.SetAgentName(agentID, name); err != nil {
		return err
	}
	s.SetDisplayName(name)
	return nil
}

func (r *Registry) occupant(dir string) (string, bool) {
	target, _ := filepath.Abs(dir)
	r.mu.RLock()
	defer r.mu.RUnlock()
	for id, cwd := range r.cwds {
		abs, _ := filepath.Abs(cwd)
		if abs == target {
			return id, true
		}
	}
	return "", false
}

func (r *Registry) resolve(spec agentadapter.Spec) (agentadapter.Spec, error) {
	if spec.Adapter == "" {
		spec.Adapter = "acp"
	}
	if spec.Adapter != "acp" && spec.Adapter != "pty" {
		return spec, fmt.Errorf("unknown adapter: %s", spec.Adapter)
	}
	if spec.ResolvedLaunch != nil {
		return spec, nil
	}
	harnessID := spec.Harness
	if harnessID == "" && spec.Agent == "" {
		harnessID = r.config.DefaultHarness
	}
	h, hasHarness := r.config.Harnesses[harnessID]
	if harnessID != "" && !hasHarness {
		return spec, fmt.Errorf("unknown harness: %s", harnessID)
	}
	agent := spec.Agent
	if agent == "" && hasHarness {
		agent = h.Agent
	}
	if agent == "" {
		agent = r.config.ACP.Default
	}
	def, ok := r.config.Agents[agent]
	if !ok {
		return spec, fmt.Errorf("unknown agent: %s", agent)
	}
	if hasHarness && h.Agent != agent {
		return spec, fmt.Errorf("harness %s belongs to agent %s", harnessID, h.Agent)
	}
	resolved := &agentadapter.ResolvedLaunch{Agent: agent}
	launch := def.ACP
	if r.config.ACP.Override != nil {
		launch = r.config.ACP.Override
	}
	if launch != nil {
		resolved.ACP = &agentadapter.Launch{Cmd: launch.Cmd, Args: append(append(append([]string{}, launch.Args...), h.ACPArgs...), spec.ACPArgs...), Env: launch.Env}
		if launch.Meta != nil {
			resolved.ACP.ParentToolCallIDPath = launch.Meta.ParentToolCallIDPath
		}
	}
	if def.Terminal != nil {
		resolved.Terminal = &agentadapter.TerminalLaunch{Cmd: def.Terminal.Cmd, StartArgs: append(append(append([]string{}, def.Terminal.StartArgs...), h.TerminalArgs...), spec.TerminalArgs...), ResumeArgs: append(append(append([]string{}, def.Terminal.Args...), h.TerminalArgs...), spec.TerminalArgs...), Env: def.Terminal.Env}
	}
	if spec.Adapter == "acp" && resolved.ACP == nil {
		return spec, fmt.Errorf("agent %s has no ACP launch configured", agent)
	}
	if spec.Adapter == "pty" && resolved.Terminal == nil {
		return spec, fmt.Errorf("agent %s has no terminal launch configured", agent)
	}
	spec.Agent, spec.Harness, spec.ResolvedLaunch = agent, harnessID, resolved
	return spec, nil
}

func expand(args []string, id, name, cwd, sessionID string) []string {
	out := append([]string{}, args...)
	for i := range out {
		out[i] = strings.NewReplacer("{cwd}", cwd, "{agentId}", id, "{agentName}", name, "{sessionId}", sessionID).Replace(out[i])
	}
	return out
}

func (r *Registry) Spawn(ctx context.Context, spec agentadapter.Spec) (*session.Session, error) {
	var err error
	spec, err = r.resolve(spec)
	if err != nil {
		return nil, err
	}
	name := r.nextName(spec.Name)
	id := name
	provisioned, err := r.workspace.Provision(ctx, spec.Workspace, name, r.occupant)
	if err != nil {
		r.releaseKnown(id)
		return nil, err
	}
	spec.Name = name
	spec.Workspace = provisioned.Workspace
	if t := spec.ResolvedLaunch.Terminal; t != nil {
		t.StartArgs = expand(t.StartArgs, id, name, provisioned.CWD, "")
	}
	project := spec.Workspace.Repo
	if project == "" {
		project = provisioned.CWD
	}
	r.applyProfile(id, project, &spec)
	raw, err := json.Marshal(spec)
	if err != nil {
		r.workspace.Rollback(ctx, spec.Workspace, provisioned.CWD, provisioned.CreatedBranch)
		r.releaseKnown(id)
		return nil, err
	}
	rec := store.Agent{ID: id, Name: name, Spec: raw, CWD: provisioned.CWD, Status: "idle", CreatedAt: time.Now().UnixMilli()}
	if err = r.store.UpsertAgent(rec); err != nil {
		r.workspace.Rollback(ctx, spec.Workspace, provisioned.CWD, provisioned.CreatedBranch)
		r.releaseKnown(id)
		return nil, err
	}
	s, err := r.start(ctx, rec, spec, "")
	if err != nil {
		_ = r.store.DeleteAgent(id)
		r.workspace.Rollback(ctx, spec.Workspace, provisioned.CWD, provisioned.CreatedBranch)
		r.releaseKnown(id)
		return nil, err
	}
	if spec.Task != "" {
		go s.Prompt(context.Background(), []agentadapter.PromptBlock{{Type: "text", Text: spec.Task}})
	}
	return s, nil
}

func (r *Registry) start(ctx context.Context, rec store.Agent, spec agentadapter.Spec, resume string, captureReplay ...bool) (*session.Session, error) {
	log, err := eventlog.New(rec.ID, r.store, r.ring)
	if err != nil {
		return nil, err
	}
	if len(spec.SessionConfig) == 0 {
		if recovered := recoverSessionConfig(log); len(recovered) > 0 {
			spec.SessionConfig = recovered
			rec.Spec, err = json.Marshal(spec)
			if err != nil {
				return nil, err
			}
			if err = r.store.UpsertAgent(rec); err != nil {
				return nil, err
			}
		}
	}
	capture := len(captureReplay) > 0 && captureReplay[0]
	a, err := r.factory.Start(ctx, agentadapter.StartRequest{AgentID: rec.ID, CWD: rec.CWD, ResumeSessionID: resume, CaptureReplay: capture, Spec: spec, Log: log})
	if err != nil {
		return nil, err
	}
	if err = applySessionConfig(ctx, a, spec.SessionConfig); err != nil {
		a.Close(ctx)
		return nil, fmt.Errorf("restore session config: %w", err)
	}
	s, err := session.NewWithStatus(rec.ID, rec.Name, spec, a, log, session.Status(rec.Status))
	if err != nil {
		a.Close(ctx)
		return nil, err
	}
	s.OnEvent(func(le eventlog.LoggedEvent) {
		if le.Event.Kind == "status" {
			_ = r.store.SetStatus(rec.ID, string(s.Status()))
		}
	})
	r.mu.Lock()
	if _, exists := r.sessions[rec.ID]; exists {
		r.mu.Unlock()
		s.Dispose(ctx)
		return nil, fmt.Errorf("agent already live: %s", rec.ID)
	}
	r.sessions[rec.ID] = s
	r.cwds[rec.ID] = rec.CWD
	r.known[rec.ID] = true
	r.mu.Unlock()
	if r.onSession != nil {
		r.onSession(s)
	}
	if sid := s.SessionID(); sid != "" {
		_ = r.store.SetSessionID(rec.ID, sid)
	}
	return s, nil
}

func recoverSessionConfig(log *eventlog.Log) json.RawMessage {
	history, err := log.FullHistory()
	if err != nil {
		return nil
	}
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Event.Kind != "session_config" {
			continue
		}
		var wire struct {
			Modes *struct {
				CurrentModeID string `json:"currentModeId"`
			} `json:"modes"`
			ConfigOptions []struct {
				ID           string `json:"id"`
				Category     string `json:"category"`
				CurrentValue any    `json:"currentValue"`
			} `json:"configOptions"`
		}
		if json.Unmarshal(history[i].Event.Payload, &wire) != nil {
			continue
		}
		saved := persistedSessionConfig{ConfigOptions: make(map[string]any)}
		hasModeOption := false
		for _, option := range wire.ConfigOptions {
			if option.ID == "" || option.CurrentValue == nil {
				continue
			}
			saved.ConfigOptions[option.ID] = option.CurrentValue
			if option.Category == "mode" {
				hasModeOption = true
			}
		}
		if len(saved.ConfigOptions) == 0 {
			saved.ConfigOptions = nil
		}
		if !hasModeOption && wire.Modes != nil {
			saved.ModeID = wire.Modes.CurrentModeID
		}
		if saved.ModeID == "" && len(saved.ConfigOptions) == 0 {
			continue
		}
		raw, _ := json.Marshal(saved)
		return raw
	}
	return nil
}

func applySessionConfig(ctx context.Context, adapter agentadapter.Adapter, raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var saved persistedSessionConfig
	if err := json.Unmarshal(raw, &saved); err != nil {
		return fmt.Errorf("invalid persisted session config: %w", err)
	}
	if saved.ModeID != "" {
		configurable, ok := adapter.(interface {
			SetMode(context.Context, string) error
		})
		if !ok {
			return errors.New("agent does not support persisted session modes")
		}
		if err := configurable.SetMode(ctx, saved.ModeID); err != nil {
			return err
		}
	}
	if len(saved.ConfigOptions) > 0 {
		configurable, ok := adapter.(interface {
			SetConfigOption(context.Context, string, any) error
		})
		if !ok {
			return errors.New("agent does not support persisted session config options")
		}
		for id, value := range saved.ConfigOptions {
			if err := configurable.SetConfigOption(ctx, id, value); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *Registry) SetMode(ctx context.Context, id, modeID string) error {
	lock := r.handoffLock(id)
	lock.Lock()
	defer lock.Unlock()
	s := r.Get(id)
	if s == nil {
		return fmt.Errorf("no such agent: %s", id)
	}
	if err := s.SetMode(ctx, modeID); err != nil {
		return err
	}
	return r.persistSessionConfig(s, func(saved *persistedSessionConfig) { saved.ModeID = modeID })
}

func (r *Registry) SetConfigOption(ctx context.Context, id, configID string, value any) error {
	lock := r.handoffLock(id)
	lock.Lock()
	defer lock.Unlock()
	s := r.Get(id)
	if s == nil {
		return fmt.Errorf("no such agent: %s", id)
	}
	if err := s.SetConfigOption(ctx, configID, value); err != nil {
		return err
	}
	return r.persistSessionConfig(s, func(saved *persistedSessionConfig) {
		if saved.ConfigOptions == nil {
			saved.ConfigOptions = make(map[string]any)
		}
		saved.ConfigOptions[configID] = value
	})
}

func (r *Registry) persistSessionConfig(s *session.Session, update func(*persistedSessionConfig)) error {
	var saved persistedSessionConfig
	if len(s.Spec.SessionConfig) > 0 {
		if err := json.Unmarshal(s.Spec.SessionConfig, &saved); err != nil {
			return err
		}
	}
	update(&saved)
	rawConfig, err := json.Marshal(saved)
	if err != nil {
		return err
	}
	rec, err := r.store.Agent(s.ID)
	if err != nil {
		return err
	}
	if rec == nil {
		return fmt.Errorf("no persisted agent: %s", s.ID)
	}
	spec := cloneSpec(s.Spec)
	spec.SessionConfig = rawConfig
	rec.Spec, err = json.Marshal(spec)
	if err != nil {
		return err
	}
	if err = r.store.UpsertAgent(*rec); err != nil {
		return err
	}
	s.SetPersistedSessionConfig(rawConfig)
	return nil
}

func (r *Registry) RestoreAll(ctx context.Context) error {
	rows, err := r.store.LiveAgents()
	if err != nil {
		return err
	}
	for _, rec := range rows {
		var spec agentadapter.Spec
		if err = json.Unmarshal(rec.Spec, &spec); err == nil {
			rec.CWD, err = r.workspace.Reattach(ctx, spec.Workspace, rec.CWD)
		}
		if err == nil {
			resume := ""
			if rec.ACPSessionID != nil {
				resume = *rec.ACPSessionID
			}
			// Some ACP agents do not persist a session/new result until the
			// first prompt. Loading that ID after a daemon restart fails and
			// leaves the durable Tandem agent visible but not live. An
			// unprompted agent has no conversation to recover, so start a new
			// ACP session in the same durable agent/workspace instead.
			if resume != "" {
				var prompted bool
				prompted, err = hasUserMessage(r.store, rec.ID)
				if err == nil && !prompted {
					resume = ""
				}
			}
			if err == nil {
				_, err = r.start(ctx, rec, spec, resume)
			}
		}
		if err != nil {
			_ = r.store.SetStatus(rec.ID, "error")
		}
	}
	return nil
}

func hasUserMessage(db *store.Store, agentID string) (bool, error) {
	log, err := eventlog.New(agentID, db, 1)
	if err != nil {
		return false, err
	}
	history, err := log.FullHistory()
	if err != nil {
		return false, err
	}
	for _, event := range history {
		if event.Event.Kind == "user_message" {
			return true, nil
		}
	}
	return false, nil
}

// ResumeCatalog unions durable Tandem rows, ACP-discovered sessions, and
// importer-owned history. Identity is (agent definition, opaque session ID).
// Live Tandem > closed Tandem > ACP > history.
func (r *Registry) ResumeCatalog(ctx context.Context) (ResumeCatalog, error) {
	rows, err := r.store.AllAgents()
	if err != nil {
		return ResumeCatalog{}, err
	}
	history, err := r.store.HistorySessions("history")
	if err != nil {
		return ResumeCatalog{}, err
	}
	type ranked struct {
		session ResumableSession
		rank    int
	}
	byKey := map[string]ranked{}
	key := func(agent, id string) string { return agent + "\x00" + id }
	// Repo attribution stats the filesystem, and a catalog routinely holds many
	// sessions per repository, so resolve each directory at most once.
	resolved := map[string]workspace.RepoOrigin{}
	repoOf := func(cwd string) workspace.RepoOrigin {
		origin, seen := resolved[cwd]
		if !seen {
			origin = workspace.RepoForDir(cwd)
			resolved[cwd] = origin
		}
		return origin
	}
	merge := func(entry ResumableSession, rank int) {
		k := key(entry.Agent, entry.SessionID)
		if prior, ok := byKey[k]; ok {
			if rank <= prior.rank {
				enrichResumable(&prior.session, entry)
				byKey[k] = prior
				return
			}
			enrichResumable(&entry, prior.session)
		}
		byKey[k] = ranked{session: entry, rank: rank}
	}
	for _, rec := range rows {
		if rec.ACPSessionID == nil || *rec.ACPSessionID == "" {
			continue
		}
		var spec agentadapter.Spec
		if json.Unmarshal(rec.Spec, &spec) != nil {
			continue
		}
		agent := spec.Agent
		if agent == "" {
			agent = r.config.ACP.Default
		}
		r.mu.RLock()
		live := r.sessions[rec.ID]
		r.mu.RUnlock()
		updated := rec.CreatedAt
		if rec.ClosedAt != nil {
			updated = *rec.ClosedAt
		}
		isLive, isClosed := live != nil, rec.ClosedAt != nil
		entry := ResumableSession{SessionID: *rec.ACPSessionID, Source: "tandem", Agent: agent, Adapter: spec.Adapter, CWD: rec.CWD, Title: rec.Name, UpdatedAt: time.UnixMilli(updated).UTC().Format(time.RFC3339Nano), AgentID: rec.ID, AgentName: rec.Name, Live: &isLive, Closed: &isClosed, Resumable: true}
		if spec.Workspace.Kind == workspace.KindWorktree {
			entry.RepoPath, entry.Repo = spec.Workspace.Repo, filepath.Base(spec.Workspace.Repo)
			entry.Branch = spec.Workspace.Branch
			if entry.Branch == "" {
				entry.Branch = "tandem/" + rec.ID
			}
		} else {
			entry.RepoPath, entry.Repo = spec.Workspace.CWD, filepath.Base(spec.Workspace.CWD)
		}
		// A record written before the workspace carried a repo, or an existing-dir
		// spec with no CWD, still needs a grouping key.
		attributeRepo(&entry, repoOf)
		if live != nil {
			entry.Status = live.Status()
		}
		rank := 4
		if isLive {
			rank = 5
		}
		merge(entry, rank)
	}
	external, adapters := r.externalSessions(ctx)
	for _, entry := range external {
		attributeRepo(&entry, repoOf)
		merge(entry, 3)
	}
	for _, item := range history {
		entry := r.historyCatalogSession(item)
		attributeRepo(&entry, repoOf)
		merge(entry, 2)
	}
	sessions := make([]ResumableSession, 0, len(byKey))
	for _, item := range byKey {
		sessions = append(sessions, item.session)
	}
	sort.SliceStable(sessions, func(i, j int) bool { return sessions[i].UpdatedAt > sessions[j].UpdatedAt })
	return ResumeCatalog{Sessions: sessions, Adapters: adapters}, nil
}

// SearchSessions groups normalized FTS entry hits by their preferred resumable
// catalog session. SearchHistory sanitizes ordinary user input into literal
// token-prefix MATCH terms; callers never supply SQLite FTS syntax.
func (r *Registry) SearchSessions(ctx context.Context, query string, limit, maxHitsPerSession int) ([]SessionSearchResult, error) {
	if limit <= 0 {
		limit = 30
	} else if limit > 50 {
		limit = 50
	}
	if maxHitsPerSession <= 0 {
		maxHitsPerSession = 3
	} else if maxHitsPerSession > 5 {
		maxHitsPerSession = 5
	}
	raw, err := r.store.SearchHistory(query, min(limit*maxHitsPerSession*2, 100))
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return []SessionSearchResult{}, nil
	}
	catalog, err := r.ResumeCatalog(ctx)
	if err != nil {
		return nil, err
	}
	key := func(agent, id string) string { return agent + "\x00" + id }
	byKey := make(map[string]ResumableSession, len(catalog.Sessions))
	byAgentID := make(map[string]ResumableSession, len(catalog.Sessions))
	for _, item := range catalog.Sessions {
		byKey[key(item.Agent, item.SessionID)] = item
		if item.AgentID != "" {
			byAgentID[item.AgentID] = item
		}
	}

	grouped := make(map[string]*SessionSearchResult)
	order := make([]string, 0, min(limit, len(raw)))
	for _, hit := range raw {
		item, ok := byKey[key(hit.Session.Agent, hit.Session.ExternalID)]
		if hit.Session.AgentID != "" {
			if preferred, found := byAgentID[hit.Session.AgentID]; found {
				item, ok = preferred, true
			}
		}
		if !ok {
			item = r.historyCatalogSession(hit.Session)
		}
		groupKey := key(item.Agent, item.SessionID)
		group := grouped[groupKey]
		if group == nil {
			if len(order) >= limit {
				continue
			}
			group = &SessionSearchResult{Session: item, Score: hit.Score, Hits: []SessionSearchHit{}}
			grouped[groupKey] = group
			order = append(order, groupKey)
		}
		if len(group.Hits) >= maxHitsPerSession {
			continue
		}
		timestamp := ""
		if hit.Timestamp != nil {
			timestamp = time.UnixMilli(*hit.Timestamp).UTC().Format(time.RFC3339Nano)
		}
		group.Hits = append(group.Hits, SessionSearchHit{
			EntryID: hit.ExternalID, Role: hit.Role, Kind: hit.Kind, Timestamp: timestamp,
			Match: hit.Match, Before: hit.Before, After: hit.After,
		})
		if hit.Score < group.Score {
			group.Score = hit.Score
		}
	}
	results := make([]SessionSearchResult, 0, len(order))
	for _, groupKey := range order {
		results = append(results, *grouped[groupKey])
	}
	return results, nil
}

// attributeRepo fills in a session's source repository from its working
// directory, leaving an attribution the caller already knows to be authoritative
// (a Tandem workspace spec) untouched.
func attributeRepo(entry *ResumableSession, repoOf func(string) workspace.RepoOrigin) {
	if entry.RepoPath != "" || entry.CWD == "" {
		if entry.RepoPath != "" && entry.Repo == "" {
			entry.Repo = filepath.Base(entry.RepoPath)
		}
		return
	}
	origin := repoOf(entry.CWD)
	entry.RepoPath, entry.Repo = origin.Path, origin.Name
}

func enrichResumable(dst *ResumableSession, src ResumableSession) {
	if dst.CWD == "" {
		dst.CWD = src.CWD
	}
	if dst.RepoPath == "" {
		dst.RepoPath, dst.Repo = src.RepoPath, src.Repo
	}
	if src.Source == "history" && src.Title != "" {
		// Preserve Tandem's display name separately in AgentName while using
		// the vendor transcript's usually more descriptive conversation title.
		dst.Title = src.Title
	} else if dst.Title == "" {
		dst.Title = src.Title
	}
	if dst.UpdatedAt == "" || (src.Source == "history" && src.UpdatedAt > dst.UpdatedAt) {
		dst.UpdatedAt = src.UpdatedAt
	}
}

func historyTimeString(value *int64) string {
	if value == nil {
		return ""
	}
	return time.UnixMilli(*value).UTC().Format(time.RFC3339Nano)
}

func (r *Registry) historyCatalogSession(item store.HistorySession) ResumableSession {
	updatedAt := item.UpdatedAt
	if updatedAt == nil {
		updatedAt = item.CreatedAt
	}
	if updatedAt == nil {
		updatedAt = &item.IndexedAt
	}
	entry := ResumableSession{SessionID: item.ExternalID, Source: "history", Agent: item.Agent,
		CWD: item.CWD, Title: item.Title, UpdatedAt: historyTimeString(updatedAt)}
	def, exists := r.config.Agents[item.Agent]
	if !exists || def.History == nil || !def.History.Enabled {
		entry.HistoryOnly, entry.ResumeError = true, "history is searchable but its agent history configuration is unavailable"
		return entry
	}
	if !item.Resumable {
		entry.HistoryOnly, entry.ResumeError = true, "the history importer marked this session as not resumable"
		return entry
	}
	if item.CWD == "" {
		entry.HistoryOnly, entry.ResumeError = true, "history is searchable but has no working directory for a safe resume"
		return entry
	}
	mode := def.History.Resume
	hasACP := def.ACP != nil || r.config.ACP.Override != nil
	hasTerminal := def.Terminal != nil && len(def.Terminal.Args) > 0
	switch mode {
	case "acp":
		entry.Adapter, entry.Resumable = "acp", hasACP
		if !hasACP {
			entry.ResumeError = "agent has no ACP launch configured"
		}
	case "terminal":
		entry.Adapter, entry.Resumable = "pty", hasTerminal
		if !hasTerminal {
			entry.ResumeError = "agent has no terminal resumeArgs configured"
		}
	default:
		entry.Resumable = hasACP || hasTerminal
		if hasACP {
			entry.Adapter = "acp"
		} else if hasTerminal {
			entry.Adapter = "pty"
		}
		if !entry.Resumable {
			entry.ResumeError = "agent has neither ACP load nor terminal resumeArgs configured"
		}
	}
	entry.HistoryOnly = !entry.Resumable
	return entry
}

func (r *Registry) externalSessions(ctx context.Context) ([]ResumableSession, []ResumeAdapterInfo) {
	r.mu.RLock()
	cache := r.external
	r.mu.RUnlock()
	if cache != nil && time.Since(cache.at) < 15*time.Second {
		return append([]ResumableSession{}, cache.sessions...), append([]ResumeAdapterInfo{}, cache.adapters...)
	}
	type probe struct {
		agent  string
		launch config.Launch
	}
	probes := []probe{}
	if r.config.ACP.Override != nil {
		probes = append(probes, probe{r.config.ACP.Default, *r.config.ACP.Override})
	} else {
		keys := make([]string, 0, len(r.config.ACP.Agents))
		for agent := range r.config.ACP.Agents {
			keys = append(keys, agent)
		}
		sort.Strings(keys)
		for _, agent := range keys {
			probes = append(probes, probe{agent, r.config.ACP.Agents[agent]})
		}
	}
	type result struct {
		agent     string
		supported bool
		sessions  []acpadapter.ProbedSession
	}
	results := make(chan result, len(probes))
	for _, p := range probes {
		go func(p probe) {
			supported, sessions := acpadapter.ProbeSessions(ctx, acp.Config{Command: p.launch.Cmd, Args: p.launch.Args, Env: envList(p.launch.Env)})
			results <- result{p.agent, supported, sessions}
		}(p)
	}
	discovered := []ResumableSession{}
	adapters := []ResumeAdapterInfo{}
	dedupe := map[string]bool{}
	for range probes {
		res := <-results
		adapters = append(adapters, ResumeAdapterInfo{Agent: res.agent, SupportsList: res.supported})
		for _, s := range res.sessions {
			identity := res.agent + "\x00" + s.SessionID
			if dedupe[identity] {
				continue
			}
			dedupe[identity] = true
			entry := ResumableSession{SessionID: s.SessionID, Source: "acp", Agent: res.agent, Adapter: "acp", CWD: s.CWD, Title: s.Title, UpdatedAt: s.UpdatedAt, Resumable: s.CWD != ""}
			if s.CWD == "" {
				entry.ResumeError = "ACP discovery returned no working directory for a safe resume"
			}
			discovered = append(discovered, entry)
		}
	}
	sort.Slice(adapters, func(i, j int) bool { return adapters[i].Agent < adapters[j].Agent })
	adapters = append(adapters, ResumeAdapterInfo{Agent: "pty (raw terminal)", SupportsList: false})
	cache = &externalCache{at: time.Now(), sessions: discovered, adapters: adapters}
	r.mu.Lock()
	r.external = cache
	r.mu.Unlock()
	return append([]ResumableSession{}, discovered...), append([]ResumeAdapterInfo{}, adapters...)
}

// Resume focuses an existing live session, restores a closed Tandem row, or
// imports an ACP/history session using only the selected agent's configured
// resume policy. Source disambiguates history policy from ACP discovery.
func (r *Registry) Resume(ctx context.Context, sessionID, agent, cwd, source string) (*session.Session, error) {
	for _, s := range r.List() {
		sessionAgent := s.Spec.Agent
		if sessionAgent == "" {
			sessionAgent = r.config.ACP.Default
		}
		if s.SessionID() == sessionID && (agent == "" || sessionAgent == agent) {
			return s, nil
		}
	}
	rec, err := r.tandemSession(agent, sessionID)
	if err != nil {
		return nil, err
	}
	if rec != nil {
		if live := r.Get(rec.ID); live != nil {
			return live, nil
		}
		var spec agentadapter.Spec
		if err := json.Unmarshal(rec.Spec, &spec); err != nil {
			return nil, err
		}
		rec.CWD, err = r.workspace.Reattach(ctx, spec.Workspace, rec.CWD)
		if err != nil {
			return nil, err
		}
		if err := r.store.ReopenAgent(rec.ID); err != nil {
			return nil, err
		}
		rec.ClosedAt, rec.Status = nil, "idle"
		resume := sessionID
		// Same rationale as RestoreAll: an ACP session that never saw a
		// prompt may never have been persisted by the agent harness, so
		// loading it after the live connection is gone fails. Start a fresh
		// ACP session in the same durable agent/workspace instead.
		if prompted, err := hasUserMessage(r.store, rec.ID); err == nil && !prompted {
			resume = ""
		}
		return r.start(ctx, *rec, spec, resume)
	}
	if agent == "" || cwd == "" {
		return nil, errors.New("resume: unknown session — agent and cwd are required to resume an external session")
	}
	if source == "history" {
		return r.resumeHistory(ctx, agent, cwd, sessionID)
	}
	return r.spawnResumed(ctx, agent, cwd, sessionID, "acp")
}

func (r *Registry) tandemSession(agent, sessionID string) (*store.Agent, error) {
	rows, err := r.store.AllAgents()
	if err != nil {
		return nil, err
	}
	var match *store.Agent
	matchAgent := ""
	for i := range rows {
		rec := rows[i]
		if rec.ACPSessionID == nil || *rec.ACPSessionID != sessionID {
			continue
		}
		var spec agentadapter.Spec
		if json.Unmarshal(rec.Spec, &spec) != nil {
			continue
		}
		specAgent := spec.Agent
		if specAgent == "" {
			specAgent = r.config.ACP.Default
		}
		if agent != "" && specAgent != agent {
			continue
		}
		copy := rec
		if agent != "" {
			// AllAgents is newest-first, matching catalog precedence among
			// multiple durable rows for one vendor identity.
			return &copy, nil
		}
		if match != nil && matchAgent != specAgent {
			return nil, errors.New("resume: session ID is ambiguous — agent is required")
		}
		if match == nil {
			match, matchAgent = &copy, specAgent
		}
	}
	return match, nil
}

func (r *Registry) resumeHistory(ctx context.Context, agent, cwd, sessionID string) (*session.Session, error) {
	var imported *store.HistorySession
	history, err := r.store.HistorySessions("history")
	if err != nil {
		return nil, err
	}
	for i := range history {
		if history[i].Agent == agent && history[i].ExternalID == sessionID {
			imported = &history[i]
			break
		}
	}
	if imported == nil {
		return nil, errors.New("resume: selected history session is no longer indexed")
	}
	entry := r.historyCatalogSession(*imported)
	if !entry.Resumable {
		return nil, fmt.Errorf("resume: %s", entry.ResumeError)
	}
	// The indexed source, not mutable client input, owns the workspace used to
	// resume imported history.
	cwd = imported.CWD
	def := r.config.Agents[agent]
	switch def.History.Resume {
	case "acp":
		return r.spawnResumed(ctx, agent, cwd, sessionID, "acp")
	case "terminal":
		return r.spawnResumed(ctx, agent, cwd, sessionID, "pty")
	default:
		if def.ACP != nil || r.config.ACP.Override != nil {
			s, acpErr := r.spawnResumed(ctx, agent, cwd, sessionID, "acp")
			if acpErr == nil {
				return s, nil
			}
			if def.Terminal != nil && len(def.Terminal.Args) > 0 {
				s, terminalErr := r.spawnResumed(ctx, agent, cwd, sessionID, "pty")
				if terminalErr == nil {
					return s, nil
				}
				return nil, fmt.Errorf("resume: ACP load failed (%v); terminal fallback failed (%v)", acpErr, terminalErr)
			}
			return nil, fmt.Errorf("resume: ACP load failed: %w", acpErr)
		}
		return r.spawnResumed(ctx, agent, cwd, sessionID, "pty")
	}
}

func (r *Registry) spawnResumed(ctx context.Context, agent, cwd, sessionID, adapter string) (*session.Session, error) {
	spec, err := r.resolve(agentadapter.Spec{Adapter: adapter, Agent: agent, Workspace: workspace.Workspace{Kind: workspace.KindExisting, CWD: cwd}})
	if err != nil {
		return nil, err
	}
	name := r.nextName("")
	p, err := r.workspace.Provision(ctx, spec.Workspace, name, r.occupant)
	if err != nil {
		r.releaseKnown(name)
		return nil, err
	}
	spec.Name, spec.Workspace = name, p.Workspace
	if adapter == "pty" {
		terminal := spec.ResolvedLaunch.Terminal
		if terminal == nil || len(terminal.ResumeArgs) == 0 {
			r.workspace.Rollback(ctx, spec.Workspace, p.CWD, p.CreatedBranch)
			r.releaseKnown(name)
			return nil, fmt.Errorf("agent has no resumable terminal command configured: %s", agent)
		}
		terminal.StartArgs = expand(terminal.ResumeArgs, name, name, p.CWD, sessionID)
	}
	raw, _ := json.Marshal(spec)
	rec := store.Agent{ID: name, Name: name, Spec: raw, CWD: p.CWD, ACPSessionID: &sessionID, Status: "idle", CreatedAt: time.Now().UnixMilli()}
	if err = r.store.UpsertAgent(rec); err == nil {
		var s *session.Session
		if adapter == "acp" {
			s, err = r.start(ctx, rec, spec, sessionID, true)
		} else {
			s, err = r.start(ctx, rec, spec, "")
		}
		if err == nil {
			return s, nil
		}
	}
	_ = r.store.DeleteAgent(name)
	r.workspace.Rollback(ctx, spec.Workspace, p.CWD, p.CreatedBranch)
	r.releaseKnown(name)
	return nil, err
}

func (r *Registry) handoffLock(id string) *sync.Mutex {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.handoffs[id] == nil {
		r.handoffs[id] = &sync.Mutex{}
	}
	return r.handoffs[id]
}

func cloneSpec(spec agentadapter.Spec) agentadapter.Spec {
	b, _ := json.Marshal(spec)
	var out agentadapter.Spec
	_ = json.Unmarshal(b, &out)
	return out
}

// canHandoff reports whether an agent can swap to its resumable CLI. It mirrors
// the preconditions enforced in EnterTerminal so the UI only offers the switch
// when it will succeed.
func canHandoff(spec agentadapter.Spec) bool {
	return spec.Adapter == "acp" &&
		spec.ResolvedLaunch != nil &&
		spec.ResolvedLaunch.Terminal != nil &&
		len(spec.ResolvedLaunch.Terminal.ResumeArgs) > 0
}

func (r *Registry) EnterTerminal(ctx context.Context, id string, interrupt bool) error {
	lock := r.handoffLock(id)
	lock.Lock()
	defer lock.Unlock()
	s := r.Get(id)
	if s == nil {
		return fmt.Errorf("no such agent: %s", id)
	}
	if s.Spec.Adapter != "acp" {
		return errors.New("terminal handoff is only available for ACP agents")
	}
	if s.ControlMode() == "terminal" {
		return nil
	}
	if (s.Status() == session.Working || s.Status() == session.Blocked || s.ActiveTurn()) && !interrupt {
		return errors.New("agent_busy: switching to Terminal will interrupt the active Transcript turn")
	}
	rec, err := r.store.Agent(id)
	if err != nil {
		return err
	}
	if rec == nil || rec.ACPSessionID == nil || *rec.ACPSessionID == "" {
		return errors.New("agent session has no resumable ACP session id")
	}
	terminal := s.Spec.ResolvedLaunch.Terminal
	if terminal == nil {
		return fmt.Errorf("no resume CLI configured for agent: %s", s.Spec.Agent)
	}
	if len(terminal.ResumeArgs) == 0 {
		return fmt.Errorf("agent has no resumable terminal command configured: %s", s.Spec.Agent)
	}
	if interrupt {
		wait, cancel := context.WithTimeout(ctx, 4*time.Second)
		err = s.InterruptAndWait(wait)
		cancel()
		if err != nil && !errors.Is(err, context.DeadlineExceeded) {
			return err
		}
	}
	s.SetControlMode("switching")
	ptySpec := cloneSpec(s.Spec)
	ptySpec.Adapter = "pty"
	ptySpec.ResolvedLaunch.Terminal.StartArgs = expand(terminal.ResumeArgs, id, s.DisplayName(), rec.CWD, *rec.ACPSessionID)
	startPTY := func() (agentadapter.Adapter, error) {
		return r.factory.Start(ctx, agentadapter.StartRequest{AgentID: id, CWD: rec.CWD, Spec: ptySpec, Log: s.Log})
	}
	err = s.SwapAdapter(ctx, startPTY, "terminal", func() {
		if e := r.LeaveTerminal(context.Background(), id); e != nil {
			payload, _ := json.Marshal(map[string]any{"kind": "error", "message": "failed to return to Transcript: " + e.Error()})
			s.PushEvent(eventlog.Event{Kind: "error", Payload: payload})
		}
	})
	if err == nil {
		return nil
	}
	// The ACP process was already stopped. Reload immediately so a broken CLI
	// does not strand the durable agent in switching mode.
	_ = r.reloadACP(ctx, s, rec.CWD, *rec.ACPSessionID)
	return err
}

func (r *Registry) reloadACP(ctx context.Context, s *session.Session, cwd, sessionID string) error {
	startACP := func() (agentadapter.Adapter, error) {
		// session/load re-streams the conversation. Unlike a daemon restart,
		// this follows a live CLI handoff, so retaining that replay is how the
		// structured transcript catches up with messages written through the
		// resumable CLI while ACP was stopped.
		return r.factory.Start(ctx, agentadapter.StartRequest{AgentID: s.ID, CWD: cwd, ResumeSessionID: sessionID, CaptureReplay: true, Spec: s.Spec, Log: s.Log})
	}
	return s.SwapAdapter(ctx, startACP, "transcript", nil)
}

func (r *Registry) LeaveTerminal(ctx context.Context, id string) error {
	lock := r.handoffLock(id)
	lock.Lock()
	defer lock.Unlock()
	s := r.Get(id)
	if s == nil {
		return fmt.Errorf("no such agent: %s", id)
	}
	if s.ControlMode() == "transcript" {
		return nil
	}
	rec, err := r.store.Agent(id)
	if err != nil {
		return err
	}
	if rec == nil || rec.ACPSessionID == nil || *rec.ACPSessionID == "" {
		return errors.New("agent session has no resumable ACP session id")
	}
	s.SetControlMode("switching")
	return r.reloadACP(ctx, s, rec.CWD, *rec.ACPSessionID)
}

func (r *Registry) ResumeCLICommand(id string) (string, error) {
	s := r.Get(id)
	if s == nil {
		return "", fmt.Errorf("no such agent: %s", id)
	}
	if s.Spec.ResolvedLaunch != nil && s.Spec.ResolvedLaunch.Terminal != nil {
		return s.Spec.ResolvedLaunch.Terminal.Cmd, nil
	}
	return "", nil
}

func (r *Registry) Close(ctx context.Context, id string, force, deleteWorktree bool) (bool, error) {
	r.mu.RLock()
	s := r.sessions[id]
	cwd := r.cwds[id]
	r.mu.RUnlock()
	if s == nil {
		// A durable row may survive a daemon restart even when its ACP child
		// could not be restored. Let force-close clean up that orphan instead
		// of leaving an undeletable agent in the UI.
		rec, err := r.store.Agent(id)
		if err != nil {
			return false, err
		}
		if rec == nil || rec.ClosedAt != nil {
			return false, nil
		}
		var spec agentadapter.Spec
		if err := json.Unmarshal(rec.Spec, &spec); err != nil {
			return false, fmt.Errorf("decode orphaned agent workspace: %w", err)
		}
		if deleteWorktree {
			if err := r.workspace.Teardown(ctx, spec.Workspace, rec.CWD, force); err != nil {
				return false, err
			}
		}
		if r.browser != nil {
			_ = r.browser.Teardown(ctx, id)
		}
		if err := r.store.CloseAgent(id); err != nil {
			return false, err
		}
		return true, nil
	}
	if deleteWorktree {
		if err := r.workspace.Teardown(ctx, s.Spec.Workspace, cwd, force); err != nil {
			return false, err
		}
	}
	r.mu.Lock()
	delete(r.sessions, id)
	delete(r.cwds, id)
	r.mu.Unlock()
	if r.browser != nil {
		_ = r.browser.Teardown(ctx, id)
	}
	if err := disposeSession(ctx, s); err != nil {
		return false, err
	}
	if err := r.store.CloseAgent(id); err != nil {
		return false, err
	}
	return true, nil
}
func (r *Registry) DisposeAll(ctx context.Context) error {
	var errs []error
	for _, s := range r.List() {
		if r.browser != nil {
			// Daemon shutdown: detach without ending the browser so an
			// externalized (Steel) session survives for re-attach on restart.
			r.browser.Detach(s.ID)
		}
		if err := disposeSession(ctx, s); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func disposeSession(ctx context.Context, s *session.Session) error {
	err := s.Dispose(ctx)
	select {
	case <-s.Done():
	case <-ctx.Done():
		if err == nil {
			err = ctx.Err()
		}
	}
	return err
}

// WaitForIdle implements deferred restart: it waits for active turns without
// preventing new status/event persistence, then returns false on deadline.
func (r *Registry) WaitForIdle(ctx context.Context) bool {
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if r.ActiveTurnCount() == 0 {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-tick.C:
		}
	}
}
