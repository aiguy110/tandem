import { useEffect, useRef, useState } from 'react';
import { useValuePresence } from '../transitions';
import { useStore, rankedOrder, agentBadge } from '../store';
import type { NotifSeverity } from '../store';
import type { AgentView } from '../store';
import type { ClosePreview } from '../wire';

// Badge color per severity, matching the Notifications panel (red > yellow > green).
const SEVERITY_CLASS: Record<NotifSeverity, string> = {
  success: 'sev-success',
  attention: 'sev-attention',
  failure: 'sev-failure',
};

// Left rail — the orchestra. One row per agent in the order arranged by the
// user. Click = focus; drag = reorder.
export function AgentsRail() {
  const order = useStore(rankedOrder);
  const agents = useStore((s) => s.agents);
  const focusedId = useStore((s) => s.focusedId);
  const focus = useStore((s) => s.focus);
  const reorderAgent = useStore((s) => s.reorderAgent);
  const markAgentUnread = useStore((s) => s.markAgentUnread);
  const setPane = useStore((s) => s.setPane);
  const setDraft = useStore((s) => s.setDraft);
  const getClosePreview = useStore((s) => s.getClosePreview);
  const closeAgent = useStore((s) => s.closeAgent);
  const renameAgent = useStore((s) => s.renameAgent);
  const collapsed = useStore((s) => s.agentsRailCollapsed);
  const toggleCollapsed = useStore((s) => s.toggleAgentsRail);
  const [pendingConfirmation, setConfirmation] = useState<{ id: string; preview: ClosePreview } | null>(null);
  // Hold the dialog on screen while it animates away.
  const { rendered: confirmation, closing: confirmClosing } = useValuePresence(pendingConfirmation);
  const [deleteWorktree, setDeleteWorktree] = useState(true);
  const [closeError, setCloseError] = useState('');
  const [draggedId, setDraggedId] = useState<string | null>(null);
  const [dropTarget, setDropTarget] = useState<{ id: string; after: boolean } | null>(null);

  const requestDelete = async (id: string) => {
    setCloseError('');
    try {
      const preview = await getClosePreview(id);
      if (!preview.notGitRepo && !preview.uncommitted && !preview.unmerged) {
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
            onMarkUnread={() => markAgentUnread(id)}
            onRename={(name) => renameAgent(id, name)}
            onDelete={() => void requestDelete(id)}
            dragging={id === draggedId}
            dropPosition={dropTarget?.id === id ? (dropTarget.after ? 'after' : 'before') : null}
            onDragStart={() => setDraggedId(id)}
            onDragOver={(after) => {
              if (draggedId && draggedId !== id) setDropTarget({ id, after });
            }}
            onDrop={(after) => {
              if (draggedId && draggedId !== id) reorderAgent(draggedId, id, after);
              setDraggedId(null);
              setDropTarget(null);
            }}
            onDragEnd={() => {
              setDraggedId(null);
              setDropTarget(null);
            }}
          />
        ))
      )}
      {closeError && <div className="rail-close-error">{closeError}</div>}
      {confirmation && agents[confirmation.id] && (
        <div style={{ display: 'contents' }} className={confirmClosing ? 'popup-closing' : undefined}>
        <div className="modal-scrim" onClick={() => setConfirmation(null)}>
          <div className="modal confirm-modal" onClick={(e) => e.stopPropagation()}>
            <div className="confirm-body">
              <div>Delete agent <b>{agents[confirmation.id].name}</b>?</div>
              {confirmation.preview.notGitRepo ? (
                <>
                  <section>
                    <strong>Not a git repository</strong>
                    <p>This existing directory will not be deleted. Deleting the agent only stops it and removes it from Tandem.</p>
                    <p>Tandem cannot tell whether the directory has any changes.</p>
                  </section>
                  <section>
                    <strong>Directory</strong>
                    <pre className="copyable-path" onClick={(e) => e.stopPropagation()}>{agents[confirmation.id].workspace.cwd}</pre>
                  </section>
                </>
              ) : (
                <>
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
                </>
              )}
            </div>
            <div className="foot">
              {!confirmation.preview.notGitRepo && (
                <button className="btn" onClick={() => promptForCommitMerge(confirmation.id)}>
                  Prompt for commit+merge
                </button>
              )}
              <button className="btn success" onClick={() => setConfirmation(null)}>Cancel</button>
              <button
                className="btn danger"
                onClick={async () => {
                  const result = await closeAgent(confirmation.id, deleteWorktree, deleteWorktree);
                  if (result.error) setCloseError(result.error);
                  else setConfirmation(null);
                }}
              >
                Delete agent
              </button>
            </div>
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
  onMarkUnread,
  onRename,
  onDelete,
  dragging,
  dropPosition,
  onDragStart,
  onDragOver,
  onDrop,
  onDragEnd,
}: {
  agent: AgentView;
  active: boolean;
  onClick: () => void;
  onMarkUnread: () => void;
  onRename: (name: string) => Promise<{ error?: string }>;
  onDelete: () => void;
  dragging: boolean;
  dropPosition: 'before' | 'after' | null;
  onDragStart: () => void;
  onDragOver: (after: boolean) => void;
  onDrop: (after: boolean) => void;
  onDragEnd: () => void;
}) {
  const [editing, setEditing] = useState(false);
  const [name, setName] = useState(agent.name);
  const [renameError, setRenameError] = useState('');
  const [mouseHovered, setMouseHovered] = useState(false);
  const [pendingContextMenu, setContextMenu] = useState<{ x: number; y: number } | null>(null);
  const [pendingDetails, setDetails] = useState<{ x: number; y: number } | null>(null);
  const { rendered: contextMenu, closing: menuClosing } = useValuePresence(pendingContextMenu);
  const { rendered: details, closing: detailsClosing } = useValuePresence(pendingDetails);
  const badge = agentBadge(agent);
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
  const showActions = active || mouseHovered;
  const beginRename = () => {
    setName(agent.name);
    setRenameError('');
    setEditing(true);
  };
  useEffect(() => {
    if (!pendingContextMenu) return;
    const dismiss = () => setContextMenu(null);
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') dismiss();
    };
    window.addEventListener('pointerdown', dismiss);
    window.addEventListener('keydown', onKeyDown);
    return () => {
      window.removeEventListener('pointerdown', dismiss);
      window.removeEventListener('keydown', onKeyDown);
    };
  }, [pendingContextMenu]);
  useEffect(() => {
    if (!pendingDetails) return;
    const dismiss = () => setDetails(null);
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') dismiss();
    };
    window.addEventListener('pointerdown', dismiss);
    window.addEventListener('keydown', onKeyDown);
    return () => {
      window.removeEventListener('pointerdown', dismiss);
      window.removeEventListener('keydown', onKeyDown);
    };
  }, [pendingDetails]);
  const openDetails = (origin: { x: number; y: number }) => {
    // Keep the initial panel near its invocation point without letting it spill
    // outside the viewport. The user can subsequently reposition it by dragging.
    const width = 390;
    const height = 430;
    setDetails({
      x: Math.max(8, Math.min(origin.x, window.innerWidth - width - 8)),
      y: Math.max(8, Math.min(origin.y, window.innerHeight - height - 8)),
    });
    setContextMenu(null);
  };
  return (
    <div
      className={`agent-row${active ? ' active' : ''}${dragging ? ' dragging' : ''}${dropPosition ? ` drop-${dropPosition}` : ''}`}
      draggable={!editing}
      title={editing ? undefined : 'Drag to reorder agent'}
      onClick={onClick}
      onDragStart={(event) => {
        event.dataTransfer.effectAllowed = 'move';
        event.dataTransfer.setData('text/plain', agent.id);
        onDragStart();
      }}
      onDragOver={(event) => {
        event.preventDefault();
        event.dataTransfer.dropEffect = 'move';
        const bounds = event.currentTarget.getBoundingClientRect();
        onDragOver(event.clientY > bounds.top + bounds.height / 2);
      }}
      onDrop={(event) => {
        event.preventDefault();
        const bounds = event.currentTarget.getBoundingClientRect();
        onDrop(event.clientY > bounds.top + bounds.height / 2);
      }}
      onDragEnd={onDragEnd}
      onContextMenu={(event) => {
        event.preventDefault();
        setContextMenu({ x: event.clientX, y: event.clientY });
      }}
      onPointerEnter={(event) => {
        if (event.pointerType === 'mouse') setMouseHovered(true);
      }}
      onPointerLeave={() => setMouseHovered(false)}
    >
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
          {badge.count > 0 && badge.severity && (
            <span className={`count badge notification-badge ${SEVERITY_CLASS[badge.severity]}`}>{badge.count}</span>
          )}
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
      {showActions && <>
        <button
          className="rename-btn"
          title="Rename agent"
          aria-label={`Rename ${agent.name}`}
          onClick={(e) => {
            e.stopPropagation();
            beginRename();
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
      </>}
      {contextMenu && (
        <div
          className={`agent-context-menu${menuClosing ? ' closing' : ''}`}
          style={{ left: contextMenu.x, top: contextMenu.y }}
          role="menu"
          aria-label={`Actions for ${agent.name}`}
          onPointerDown={(event) => event.stopPropagation()}
          onClick={(event) => event.stopPropagation()}
        >
          <button type="button" role="menuitem" onClick={() => { onMarkUnread(); setContextMenu(null); }}>
            Mark as unread
          </button>
          <button type="button" role="menuitem" onClick={() => { beginRename(); setContextMenu(null); }}>
            Edit name
          </button>
          <button type="button" role="menuitem" onClick={() => openDetails(contextMenu)}>
            View details
          </button>
          <button type="button" className="danger" role="menuitem" onClick={() => { onDelete(); setContextMenu(null); }}>
            Delete
          </button>
        </div>
      )}
      {details && <AgentDetails agent={agent} position={details} closing={detailsClosing} onClose={() => setDetails(null)} onMove={setDetails} />}
    </div>
  );
}

function AgentDetails({
  agent,
  position,
  closing,
  onClose,
  onMove,
}: {
  agent: AgentView;
  position: { x: number; y: number };
  closing: boolean;
  onClose: () => void;
  onMove: (position: { x: number; y: number }) => void;
}) {
  const drag = useRef<{ offsetX: number; offsetY: number } | null>(null);
  const ws = agent.workspace;
  const profile = agent.profile;
  const gitState = ws.gitState === 'unknown' ? 'Unavailable' : (ws.gitState ?? 'Unavailable').replace(/_/g, ' ');
  const beginDrag = (event: React.PointerEvent<HTMLDivElement>) => {
    event.preventDefault();
    event.stopPropagation();
    drag.current = { offsetX: event.clientX - position.x, offsetY: event.clientY - position.y };
    event.currentTarget.setPointerCapture(event.pointerId);
  };
  const moveDrag = (event: React.PointerEvent<HTMLDivElement>) => {
    if (!drag.current) return;
    onMove({
      x: Math.max(8, Math.min(event.clientX - drag.current.offsetX, window.innerWidth - 80)),
      y: Math.max(8, Math.min(event.clientY - drag.current.offsetY, window.innerHeight - 42)),
    });
  };
  const endDrag = () => { drag.current = null; };
  const profileBits = [profile?.model, profile?.effort, profile?.permission].filter(Boolean);

  return (
    <section
      className={`agent-details-popover${closing ? ' closing' : ''}`}
      style={{ left: position.x, top: position.y }}
      role="dialog"
      aria-label={`Details for ${agent.name}`}
      onPointerDown={(event) => event.stopPropagation()}
      onClick={(event) => event.stopPropagation()}
    >
      <div className="agent-details-head" onPointerDown={beginDrag} onPointerMove={moveDrag} onPointerUp={endDrag} onPointerCancel={endDrag}>
        <div>
          <strong>{agent.name}</strong>
          <span className={`agent-details-status ${agent.status}`}>{agent.status}</span>
        </div>
        <button type="button" aria-label="Close details" title="Close details" onPointerDown={(event) => event.stopPropagation()} onClick={onClose}>×</button>
      </div>
      <div className="agent-details-body">
        <Detail label="Agent" value={agent.agent || 'Default agent'} />
        <Detail label="Profile" value={profileBits.join(' · ') || (profile?.id ? 'Saved profile' : 'No profile settings')} />
        {profile?.id && <Detail label="Profile ID" value={profile.id} mono />}
        {profile?.snapshot && <Detail label="Browser snapshot" value={profile.snapshot} mono />}
        <Detail label="Adapter" value={`${agent.adapter.toUpperCase()}${agent.canHandoff ? ' · CLI handoff available' : ''}`} />
        <Detail label="Workspace" value={ws.kind === 'worktree' ? 'Isolated worktree' : 'Existing directory'} />
        <Detail label="Worktree path" value={ws.cwd} mono />
        <Detail label="Repository root" value={ws.repoPath || 'Unavailable'} mono />
        <Detail label="Branch" value={ws.branch || 'Detached / not a branch'} mono />
        {ws.targetRef && <Detail label="Integration target" value={ws.targetRef.replace(/^refs\/(heads|remotes)\//, '')} mono />}
        <Detail label="Git status" value={`${gitState}${typeof ws.ahead === 'number' || typeof ws.behind === 'number' ? ` · ${ws.ahead ?? 0} ahead, ${ws.behind ?? 0} behind` : ''}`} />
        {ws.startCommit && <Detail label="Starting commit" value={ws.startCommit} mono />}
        <Detail label="Browser" value={agent.browserActive ? `${agent.browserOwner} has control` : 'Not started'} />
        {agent.pendingApprovals.length > 0 && <Detail label="Pending approvals" value={String(agent.pendingApprovals.length)} />}
      </div>
    </section>
  );
}

function Detail({ label, value, mono = false }: { label: string; value: string; mono?: boolean }) {
  return <div className="agent-detail"><span>{label}</span><code className={mono ? '' : 'plain'} title={value}>{value}</code></div>;
}
