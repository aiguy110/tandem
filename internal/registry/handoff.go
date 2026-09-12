package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/aiguy110/tandem/internal/agentadapter"
	"github.com/aiguy110/tandem/internal/eventlog"
	"github.com/aiguy110/tandem/internal/handoff"
	"github.com/aiguy110/tandem/internal/store"
	"github.com/aiguy110/tandem/internal/workspace"
)

// harnessLabel is the human name for whichever harness/agent a spec launched,
// used to tell a receiving agent who it is taking over from.
func (r *Registry) harnessLabel(spec agentadapter.Spec) string {
	if spec.Harness != "" {
		if h, ok := r.config.Harnesses[spec.Harness]; ok && h.Name != "" {
			return h.Name
		}
	}
	agent := spec.Agent
	if spec.ResolvedLaunch != nil && spec.ResolvedLaunch.Agent != "" {
		agent = spec.ResolvedLaunch.Agent
	}
	if def, ok := r.config.Agents[agent]; ok && def.Name != "" {
		return def.Name
	}
	return agent
}

// sourceAgent resolves a hand-off/workspace-sharing source by id, whether or
// not it is still live. A closed agent is deliberately still usable as a
// source: its transcript survives in the event log.
func (r *Registry) sourceAgent(id string) (store.Agent, agentadapter.Spec, error) {
	var spec agentadapter.Spec
	rec, err := r.store.Agent(id)
	if err != nil {
		return store.Agent{}, spec, err
	}
	if rec == nil {
		return store.Agent{}, spec, fmt.Errorf("no such agent: %s", id)
	}
	if err := json.Unmarshal(rec.Spec, &spec); err != nil {
		return store.Agent{}, spec, fmt.Errorf("decode source agent spec: %w", err)
	}
	return *rec, spec, nil
}

// handoffMessage renders the source agent's transcript into the text that will
// become the receiving agent's first user message. It never calls a model.
func (r *Registry) handoffMessage(sourceID string) (string, error) {
	rec, spec, err := r.sourceAgent(sourceID)
	if err != nil {
		return "", err
	}
	log, err := eventlog.New(rec.ID, r.store, 1)
	if err != nil {
		return "", err
	}
	history, err := log.FullHistory()
	if err != nil {
		return "", err
	}
	text := handoff.Render(history, handoff.Source{
		AgentName: rec.Name,
		Harness:   r.harnessLabel(spec),
		CWD:       rec.CWD,
		Branch:    spec.Workspace.Branch,
	})
	if text == "" {
		return "", fmt.Errorf("agent %s has no transcript to hand off", rec.Name)
	}
	return text, nil
}

// Cohabitants names the other agents that have not been closed and are working
// in dir. It is what both spawn-time and delete-time warnings are built from,
// and what stops a shared worktree from being torn down under a live agent.
func (r *Registry) Cohabitants(dir, excludeID string) []string {
	target, err := filepath.Abs(dir)
	if err != nil || dir == "" {
		return nil
	}
	rows, err := r.store.AllAgents()
	if err != nil {
		return nil
	}
	var names []string
	for _, rec := range rows {
		if rec.ID == excludeID || rec.ClosedAt != nil || rec.CWD == "" {
			continue
		}
		if abs, err := filepath.Abs(rec.CWD); err == nil && abs == target {
			names = append(names, rec.Name)
		}
	}
	sort.Strings(names)
	return names
}

// sharedWorkspace lets a new agent join a directory another agent already
// occupies instead of provisioning its own. The joining agent inherits the
// occupant's full workspace descriptor — repo, branch, integration target — so
// a shared Tandem worktree keeps behaving like the worktree it is (git status
// in the rail, a merge target, teardown once the last occupant leaves) rather
// than degrading into an anonymous "existing directory".
//
// It returns ok=false when the requested directory is not already occupied, in
// which case the caller provisions normally.
func (r *Registry) sharedWorkspace(ws workspace.Workspace) (workspace.Workspace, string, bool) {
	if ws.Kind != workspace.KindExisting || ws.CWD == "" {
		return workspace.Workspace{}, "", false
	}
	target, err := filepath.Abs(ws.CWD)
	if err != nil {
		return workspace.Workspace{}, "", false
	}
	rows, err := r.store.AllAgents()
	if err != nil {
		return workspace.Workspace{}, "", false
	}
	for _, rec := range rows {
		if rec.ClosedAt != nil || rec.CWD == "" {
			continue
		}
		abs, err := filepath.Abs(rec.CWD)
		if err != nil || abs != target {
			continue
		}
		var spec agentadapter.Spec
		if json.Unmarshal(rec.Spec, &spec) != nil || spec.Workspace.Kind != workspace.KindWorktree {
			continue
		}
		return spec.Workspace, abs, true
	}
	return workspace.Workspace{}, "", false
}

// provisionOrJoin provisions a workspace, except where the request names a
// directory an existing agent already occupies — then the new agent joins that
// checkout and inherits its descriptor. Multiple agents per worktree is a
// supported arrangement (a hand-off has to be able to pick up uncommitted work
// in place), so occupancy is reported to the user as a warning rather than
// enforced here.
func (r *Registry) provisionOrJoin(ctx context.Context, ws workspace.Workspace, name string) (workspace.ProvisionResult, error) {
	if shared, cwd, ok := r.sharedWorkspace(ws); ok {
		return workspace.ProvisionResult{CWD: cwd, Workspace: shared, Shared: true}, nil
	}
	return r.workspace.Provision(ctx, ws, name)
}

// rollback undoes a failed spawn's provisioning. A joined checkout belongs to
// another agent, so it is left exactly as it was found.
func (r *Registry) rollback(ctx context.Context, ws workspace.Workspace, p workspace.ProvisionResult) {
	if p.Shared {
		return
	}
	r.workspace.Rollback(ctx, ws, p.CWD, p.CreatedBranch)
}
