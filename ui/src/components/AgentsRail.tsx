import { useState } from 'react';
import { useStore, rankedOrder } from '../store';
import type { AgentView } from '../store';
import type { ClosePreview } from '../wire';

const COMMIT_MERGE_PROMPT = 'please commit your changes and merge them back into the main worktree';

// Left rail — the orchestra. One row per agent; blocked/error float to the top
// (rankedOrder). Click = focus.
export function AgentsRail() {
  const order = useStore(rankedOrder);
  const agents = useStore((s) => s.agents);
  const focusedId = useStore((s) => s.focusedId);
  const focus = useStore((s) => s.focus);
  const setPane = useStore((s) => s.setPane);
  const setDraft = useStore((s) => s.setDraft);
  const getClosePreview = useStore((s) => s.getClosePreview);
  const closeAgent = useStore((s) => s.closeAgent);
  const collapsed = useStore((s) => s.agentsRailCollapsed);
  const toggleCollapsed = useStore((s) => s.toggleAgentsRail);
  const [confirmation, setConfirmation] = useState<{ id: string; preview: ClosePreview } | null>(null);
  const [deleteWorktree, setDeleteWorktree] = useState(true);
  const [closeError, setCloseError] = useState('');

  const requestDelete = async (id: string) => {
    setCloseError('');
    try {
      const preview = await getClosePreview(id);
      if (!preview.uncommitted && !preview.unmerged) {
        const result = await closeAgent(id, false, true);
        if (result.error) setCloseError(result.error);
        return;
      }
      setDeleteWorktree(preview.kind === 'worktree');
      setConfirmation({ id, preview });
    } catch (error) {
      setCloseError((error as Error).message);
    }
  };

  const promptForCommitMerge = (id: string) => {
    setConfirmation(null);
    focus(id);
    setPane('transcript');
    setDraft(id, COMMIT_MERGE_PROMPT);
    requestAnimationFrame(() => requestAnimationFrame(() => {
      const textarea = [...document.querySelectorAll<HTMLTextAreaElement>('[data-prompt-agent]')].find((el) => el.dataset.promptAgent === id);
      textarea?.focus();
      textarea?.setSelectionRange(textarea.value.length, textarea.value.length);
    }));
  };

  if (collapsed) {
    return (
      <div className="rail agents collapsed">
        <button className="rail-toggle" title="Show agents" onClick={toggleCollapsed}>
          <span className="chevron">›</span>
          <span className="label">Agents</span>
          {order.length > 0 && <span className="count">{order.length}</span>}
        </button>
      </div>
    );
  }

  return (
    <div className="rail agents">
      <div className="rail-head">
        <button className="rail-toggle-btn" title="Collapse agents" onClick={toggleCollapsed}>
          ‹
        </button>
        Agents <span className="count">{order.length}</span>
      </div>
      {order.length === 0 ? (
        <div className="empty">
          No agents yet.
          <br />
          Press <span className="kbd">C</span> or <b>+ Agent</b> to spawn one.
        </div>
      ) : (
        order.map((id) => (
          <Row
            key={id}
            agent={agents[id]}
            active={id === focusedId}
            onClick={() => focus(id)}
            onDelete={() => void requestDelete(id)}
          />
        ))
      )}
      {closeError && <div className="rail-close-error">{closeError}</div>}
      {confirmation && agents[confirmation.id] && (
        <div className="modal-scrim" onClick={() => setConfirmation(null)}>
          <div className="modal confirm-modal" onClick={(e) => e.stopPropagation()}>
            <div className="confirm-body">
              <div>Delete agent <b>{agents[confirmation.id].name}</b>?</div>
              {confirmation.preview.uncommitted && (
                <section><strong>Uncommitted changes</strong><pre>{confirmation.preview.uncommitted}</pre></section>
              )}
              {confirmation.preview.unmerged && (
                <section><strong>Unmerged commits</strong><pre>{confirmation.preview.unmerged}</pre></section>
              )}
              {confirmation.preview.kind === 'worktree' && (
                <label className="delete-worktree-option">
                  <input type="checkbox" checked={deleteWorktree} onChange={(e) => setDeleteWorktree(e.target.checked)} />
                  Delete worktree
                </label>
              )}
            </div>
            <div className="foot">
              <button className="btn" onClick={() => promptForCommitMerge(confirmation.id)}>
                Prompt for commit+merge
              </button>
              <button className="btn success" onClick={() => setConfirmation(null)}>Cancel</button>
              <button
                className="btn danger"
                onClick={async () => {
                  const result = await closeAgent(confirmation.id, deleteWorktree, deleteWorktree);
                  if (result.error) setCloseError(result.error);
                  else setConfirmation(null);
                }}
              >
                Delete
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}

function Row({
  agent,
  active,
  onClick,
  onDelete,
}: {
  agent: AgentView;
  active: boolean;
  onClick: () => void;
  onDelete: () => void;
}) {
  const ws = agent.workspace;
  const branch = ws.branch || (ws.kind === 'existing' ? 'no-branch' : '');
  const gitStateTitle = ws.gitState
    ? { dirty: 'uncommitted changes', unmerged: 'committed, not yet merged', synced: 'clean and merged' }[ws.gitState]
    : undefined;
  return (
    <div className={`agent-row${active ? ' active' : ''}`} onClick={onClick}>
      <span className={`dot ${agent.status}`} title={agent.status} />
      <div style={{ minWidth: 0 }}>
        <div className="name">
          {agent.status === 'blocked' && '⚠ '}
          {agent.status === 'error' && '⛔ '}
          {agent.name}
          {agent.pendingApprovals.length > 0 && <span className="count hot badge">{agent.pendingApprovals.length}</span>}
        </div>
        <div className="ws" title={ws.cwd}>
          {ws.gitState && <span className={`git-state ${ws.gitState}`} title={gitStateTitle} />}
          <span className="ws-text">
            {ws.repo || '—'}
            {branch && ` · ${branch}`}
          </span>
        </div>
      </div>
      <button
        className="delete-btn"
        title="Delete agent"
        onClick={(e) => {
          e.stopPropagation();
          onDelete();
        }}
      >
        🗑
      </button>
    </div>
  );
}
