// Package registry is the authoritative in-memory owner of live agents.
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aiguy110/tandem/internal/agentadapter"
	"github.com/aiguy110/tandem/internal/assets"
	"github.com/aiguy110/tandem/internal/config"
	"github.com/aiguy110/tandem/internal/eventlog"
	"github.com/aiguy110/tandem/internal/session"
	"github.com/aiguy110/tandem/internal/store"
	"github.com/aiguy110/tandem/internal/workspace"
)

var nameWords = []string{"web", "api", "db", "cli", "ui", "svc", "job", "net"}

type Registry struct {
	mu        sync.RWMutex
	store     *store.Store
	config    config.Config
	workspace *workspace.Manager
	factory   agentadapter.Factory
	ring      int
	sessions  map[string]*session.Session
	cwds      map[string]string
	known     map[string]bool
	counter   int
}
type Options struct {
	Store        *store.Store
	Config       config.Config
	Workspace    *workspace.Manager
	Factory      agentadapter.Factory
	Assets       *assets.Store
	RingCapacity int
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
		o.Factory = DefaultFactory{Assets: assetStore}
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
	return &Registry{store: o.Store, config: o.Config, workspace: o.Workspace, factory: o.Factory, ring: cap, sessions: map[string]*session.Session{}, cwds: map[string]string{}, known: known, counter: max}, nil
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
	profileID := spec.Profile
	if profileID == "" && spec.Agent == "" {
		profileID = r.config.DefaultProfile
	}
	p, hasProfile := r.config.Profiles[profileID]
	if profileID != "" && !hasProfile {
		return spec, fmt.Errorf("unknown profile: %s", profileID)
	}
	agent := spec.Agent
	if agent == "" && hasProfile {
		agent = p.Agent
	}
	if agent == "" {
		agent = r.config.ACP.Default
	}
	def, ok := r.config.Agents[agent]
	if !ok {
		return spec, fmt.Errorf("unknown agent: %s", agent)
	}
	if hasProfile && p.Agent != agent {
		return spec, fmt.Errorf("profile %s belongs to agent %s", profileID, p.Agent)
	}
	resolved := &agentadapter.ResolvedLaunch{Agent: agent}
	launch := def.ACP
	if r.config.ACP.Override != nil {
		launch = r.config.ACP.Override
	}
	if launch != nil {
		resolved.ACP = &agentadapter.Launch{Cmd: launch.Cmd, Args: append(append(append([]string{}, launch.Args...), p.ACPArgs...), spec.ACPArgs...), Env: launch.Env}
	}
	if def.Terminal != nil {
		resolved.Terminal = &agentadapter.TerminalLaunch{Cmd: def.Terminal.Cmd, StartArgs: append(append(append([]string{}, def.Terminal.StartArgs...), p.TerminalArgs...), spec.TerminalArgs...), ResumeArgs: append(append(append([]string{}, def.Terminal.Args...), p.TerminalArgs...), spec.TerminalArgs...), Env: def.Terminal.Env}
	}
	if spec.Adapter == "acp" && resolved.ACP == nil {
		return spec, fmt.Errorf("agent %s has no ACP launch configured", agent)
	}
	if spec.Adapter == "pty" && resolved.Terminal == nil {
		return spec, fmt.Errorf("agent %s has no terminal launch configured", agent)
	}
	spec.Agent, spec.Profile, spec.ResolvedLaunch = agent, profileID, resolved
	return spec, nil
}

func expand(args []string, id, name, cwd string) []string {
	out := append([]string{}, args...)
	for i := range out {
		out[i] = strings.NewReplacer("{cwd}", cwd, "{agentId}", id, "{agentName}", name).Replace(out[i])
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
		t.StartArgs = expand(t.StartArgs, id, name, provisioned.CWD)
	}
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

func (r *Registry) start(ctx context.Context, rec store.Agent, spec agentadapter.Spec, resume string) (*session.Session, error) {
	log, err := eventlog.New(rec.ID, r.store, r.ring)
	if err != nil {
		return nil, err
	}
	a, err := r.factory.Start(ctx, agentadapter.StartRequest{AgentID: rec.ID, CWD: rec.CWD, ResumeSessionID: resume, Spec: spec, Log: log})
	if err != nil {
		return nil, err
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
	if sid := s.SessionID(); sid != "" {
		_ = r.store.SetSessionID(rec.ID, sid)
	}
	return s, nil
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
			_, err = r.start(ctx, rec, spec, resume)
		}
		if err != nil {
			_ = r.store.SetStatus(rec.ID, "error")
		}
	}
	return nil
}

func (r *Registry) Close(ctx context.Context, id string, force, deleteWorktree bool) (bool, error) {
	r.mu.RLock()
	s := r.sessions[id]
	cwd := r.cwds[id]
	r.mu.RUnlock()
	if s == nil {
		return false, nil
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
