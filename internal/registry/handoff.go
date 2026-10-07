package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"time"

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
func (r *Registry) sourceAgent(id string) (store.Session, agentadapter.Spec, error) {
	var spec agentadapter.Spec
	rec, err := r.store.Session(id)
	if err != nil {
		return store.Session{}, spec, err
	}
	if rec == nil {
		return store.Session{}, spec, fmt.Errorf("no such agent: %s", id)
	}
	if err := json.Unmarshal(rec.Spec, &spec); err != nil {
		return store.Session{}, spec, fmt.Errorf("decode source agent spec: %w", err)
	}
	return *rec, spec, nil
}

// handoffMessage renders the source agent's transcript into the text that will
// become the receiving agent's first user message. It never calls a model.
func (r *Registry) handoffMessage(sourceID, mode string, cutoff int64) (string, handoff.DelegationSummary, error) {
	rec, spec, err := r.sourceAgent(sourceID)
	if err != nil {
		return "", handoff.DelegationSummary{}, err
	}
	log, err := eventlog.New(rec.ID, r.store, 1)
	if err != nil {
		return "", handoff.DelegationSummary{}, err
	}
	history, err := log.FullHistoryThrough(cutoff)
	if err != nil {
		return "", handoff.DelegationSummary{}, err
	}
	delegations := handoff.AnalyzeDelegations(history)
	text := handoff.Render(history, handoff.Source{
		SessionName: rec.Name,
		Harness:     r.harnessLabel(spec),
		CWD:         rec.CWD,
		Branch:      spec.Workspace.Branch,
	}, handoff.ParseMode(mode))
	if text == "" {
		return "", handoff.DelegationSummary{}, fmt.Errorf("agent %s has no transcript to hand off", rec.Name)
	}
	return text, delegations, nil
}

const (
	handoffInterruptTimeout = 5 * time.Second
	handoffDrainQuiet       = 300 * time.Millisecond
)

type handoffSnapshot struct {
	Text            string
	SourceSessionID string
	CutoffSeq       int64
	Mode            string
	SourceWasActive bool
	Delegations     handoff.DelegationSummary
}

// prepareHandoff establishes the write boundary before rendering a source.
// Active sources are cancelled; idle and closed sources remain untouched. The
// per-source handoff lock also excludes ACP/terminal adapter swaps while the
// cutoff is selected and the durable history is read.
func (r *Registry) prepareHandoff(ctx context.Context, sourceID, mode string) (handoffSnapshot, error) {
	lock := r.handoffLock(sourceID)
	lock.Lock()
	defer lock.Unlock()

	rec, _, err := r.sourceAgent(sourceID)
	if err != nil {
		return handoffSnapshot{}, err
	}
	snapshot := handoffSnapshot{SourceSessionID: sourceID, Mode: string(handoff.ParseMode(mode))}
	live := r.Get(sourceID)
	if live != nil {
		var release func()
		snapshot.SourceWasActive, release = live.BeginHandoffSnapshot()
		defer release()
		if snapshot.SourceWasActive {
			slog.Info("quiescing handoff source", "source_session_id", sourceID, "status", live.Status())
			waitCtx, cancel := context.WithTimeout(ctx, handoffInterruptTimeout)
			err = live.InterruptAndWait(waitCtx)
			cancel()
			if err != nil {
				slog.Warn("handoff source failed to quiesce", "source_session_id", sourceID, "timeout", handoffInterruptTimeout, "error", err)
				return handoffSnapshot{}, fmt.Errorf("quiesce hand-off source %s: %w", rec.Name, err)
			}
		}
		drainCtx, cancel := context.WithTimeout(ctx, handoffInterruptTimeout)
		snapshot.CutoffSeq, err = live.WaitForEventDrain(drainCtx, handoffDrainQuiet)
		cancel()
		if err != nil {
			return handoffSnapshot{}, fmt.Errorf("drain hand-off source %s: %w", rec.Name, err)
		}
	} else {
		log, logErr := eventlog.New(sourceID, r.store, 1)
		if logErr != nil {
			return handoffSnapshot{}, logErr
		}
		snapshot.CutoffSeq = log.Head()
	}
	snapshot.Text, snapshot.Delegations, err = r.handoffMessage(sourceID, mode, snapshot.CutoffSeq)
	if err != nil {
		return handoffSnapshot{}, err
	}
	slog.Info("handoff snapshot prepared", "source_session_id", sourceID, "cutoff_seq", snapshot.CutoffSeq, "mode", snapshot.Mode, "source_was_active", snapshot.SourceWasActive, "delegations_total", snapshot.Delegations.Total(), "delegations_running", snapshot.Delegations.Running, "delegations_unresolved", snapshot.Delegations.Unresolved)
	return snapshot, nil
}

// Cohabitants names the other agents that have not been closed and are working
// in dir. It is what both spawn-time and delete-time warnings are built from,
// and what stops a shared worktree from being torn down under a live agent.
func (r *Registry) Cohabitants(dir, excludeID string) []string {
	target, err := filepath.Abs(dir)
	if err != nil || dir == "" {
		return nil
	}
	rows, err := r.store.AllSessions()
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
	rows, err := r.store.AllSessions()
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
