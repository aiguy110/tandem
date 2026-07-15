import { useState } from 'react';
import { useStore, rankedOrder } from '../store';
import type { AgentView } from '../store';

// Left rail — the orchestra. One row per agent; blocked/error float to the top
// (rankedOrder). Click = focus.
export function AgentsRail() {
  const order = useStore(rankedOrder);
  const agents = useStore((s) => s.agents);
  const focusedId = useStore((s) => s.focusedId);
  const focus = useStore((s) => s.focus);
  const closeAgent = useStore((s) => s.closeAgent);
  const collapsed = useStore((s) => s.agentsRailCollapsed);
  const toggleCollapsed = useStore((s) => s.toggleAgentsRail);
  const [confirmId, setConfirmId] = useState<string | null>(null);

  const doDelete = (id: string, force?: boolean) => {
    void closeAgent(id, force).then((r) => {
      if (r.error?.startsWith('dirty_worktree')) {
        if (confirm(`${id} has uncommitted changes. Force close and drop the checkout? (branch is kept)`)) {
          doDelete(id, true);
        }
      }
    });
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
            onDelete={() => setConfirmId(id)}
          />
        ))
      )}
      {confirmId && agents[confirmId] && (
        <div className="modal-scrim" onClick={() => setConfirmId(null)}>
          <div className="modal confirm-modal" onClick={(e) => e.stopPropagation()}>
            <div className="confirm-body">
              Delete agent <b>{agents[confirmId].name}</b>? This tears down its session (the branch is kept).
            </div>
            <div className="foot">
              <button className="btn" onClick={() => setConfirmId(null)}>
                Cancel
              </button>
              <button
                className="btn danger"
                onClick={() => {
                  doDelete(confirmId);
                  setConfirmId(null);
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
          {ws.repo || '—'}
          {branch && ` · ${branch}`}
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
