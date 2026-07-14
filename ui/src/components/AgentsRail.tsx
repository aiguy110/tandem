import { useStore, rankedOrder } from '../store';
import type { AgentView } from '../store';

// Left rail — the orchestra. One row per agent; blocked/error float to the top
// (rankedOrder). Click = focus.
export function AgentsRail() {
  const order = useStore(rankedOrder);
  const agents = useStore((s) => s.agents);
  const focusedId = useStore((s) => s.focusedId);
  const focus = useStore((s) => s.focus);

  return (
    <div className="rail agents">
      <div className="rail-head">
        Agents <span className="count">{order.length}</span>
      </div>
      {order.length === 0 ? (
        <div className="empty">
          No agents yet.
          <br />
          Press <span className="kbd">C</span> or <b>+ Agent</b> to spawn one.
        </div>
      ) : (
        order.map((id) => <Row key={id} agent={agents[id]} active={id === focusedId} onClick={() => focus(id)} />)
      )}
    </div>
  );
}

function Row({ agent, active, onClick }: { agent: AgentView; active: boolean; onClick: () => void }) {
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
    </div>
  );
}
