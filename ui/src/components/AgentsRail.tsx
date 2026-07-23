import { useState } from 'react';
import { useStore, rankedOrder } from '../store';
import type { AgentView } from '../store';
import type { ClosePreview } from '../wire';

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
  const renameAgent = useStore((s) => s.renameAgent);
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
    setPane('chat');
    const target = agents[id]?.workspace.targetRef;
    setDraft(id, target
      ? `commit your changes and merge back into ${target.replace(/^refs\/heads\//, '').replace(/^refs\/remotes\//, '')}`
      : 'commit your changes and merge back into the integration target branch');
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
        <span className="rail-head-label" onClick={toggleCollapsed}>
          Agents <span className="count">{order.length}</span>
        </span>
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
            onRename={(name) => renameAgent(id, name)}
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
              {confirmation.preview.targetRef && (
                <section><strong>Integration target</strong><pre>{confirmation.preview.targetRef.replace(/^refs\/heads\//, '').replace(/^refs\/remotes\//, '')}{typeof confirmation.preview.ahead === 'number' ? `\n${confirmation.preview.ahead} ahead · ${confirmation.preview.behind ?? 0} behind` : ''}</pre></section>
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
  onRename,
  onDelete,
}: {
  agent: AgentView;
  active: boolean;
  onClick: () => void;
  onRename: (name: string) => Promise<{ error?: string }>;
  onDelete: () => void;
}) {
  const [editing, setEditing] = useState(false);
  const [name, setName] = useState(agent.name);
  const [renameError, setRenameError] = useState('');
  const ws = agent.workspace;
  const branch = ws.branch || (ws.kind === 'existing' ? 'no-branch' : '');
  const target = ws.targetRef?.replace(/^refs\/heads\//, '').replace(/^refs\/remotes\//, '');
  const gitStateTitle = ws.gitState
    ? {
        dirty: 'uncommitted changes',
        ahead: `${ws.ahead ?? 0} commit(s) ahead of ${target ?? 'target'}`,
        behind: `${ws.behind ?? 0} commit(s) behind ${target ?? 'target'}`,
        diverged: `diverged from ${target ?? 'target'}`,
        merged: `merged into ${target ?? 'target'}`,
        synced: 'clean and synchronized',
        target_missing: 'integration target is missing',
        unknown: 'git status unavailable',
      }[ws.gitState]
    : undefined;
  const commitRename = async () => {
    const next = name.trim();
    if (!next) {
      setRenameError('Name cannot be empty');
      return;
    }
    const result = await onRename(next);
    if (result.error) {
      setRenameError(result.error);
      return;
    }
    setEditing(false);
    setRenameError('');
  };
  return (
    <div className={`agent-row${active ? ' active' : ''}`} onClick={onClick}>
      <span className={`dot ${agent.status}`} title={agent.status} />
      <div style={{ minWidth: 0 }}>
        <div className="name">
          {agent.status === 'blocked' && '⚠ '}
          {agent.status === 'error' && '⛔ '}
          {editing ? (
            <input
              className="agent-rename-input"
              value={name}
              maxLength={80}
              autoFocus
              aria-label="Agent name"
              onClick={(e) => e.stopPropagation()}
              onChange={(e) => setName(e.target.value)}
              onBlur={() => {
                setName(agent.name);
                setRenameError('');
                setEditing(false);
              }}
              onKeyDown={(e) => {
                if (e.key === 'Enter') void commitRename();
                if (e.key === 'Escape') {
                  setName(agent.name);
                  setRenameError('');
                  setEditing(false);
                }
              }}
            />
          ) : agent.name}
          {agent.pendingApprovals.length > 0 && <span className="count hot badge">{agent.pendingApprovals.length}</span>}
        </div>
        {renameError && <div className="agent-rename-error">{renameError}</div>}
        <div className="ws" title={ws.cwd}>
          {ws.gitState && <span className={`git-state ${ws.gitState}`} title={gitStateTitle} />}
          <span className="ws-text">
            {ws.repo || '—'}
            {branch && ` · ${branch}`}
            {target && ` → ${target}`}
            {(ws.ahead || ws.behind) ? ` · +${ws.ahead ?? 0}/-${ws.behind ?? 0}` : ''}
          </span>
        </div>
      </div>
      <button
        className="rename-btn"
        title="Rename agent"
        aria-label={`Rename ${agent.name}`}
        onClick={(e) => {
          e.stopPropagation();
          setName(agent.name);
          setRenameError('');
          setEditing(true);
        }}
      >
        ✎
      </button>
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
