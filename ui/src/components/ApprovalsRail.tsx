import { useStore, allApprovals, allTakeovers } from '../store';

// Right rail — the conductor's inbox: every pending approval AND browser-takeover
// request across ALL agents, most-urgent first. Clicking a card focuses that agent.
export function ApprovalsRail() {
  const items = useStore(allApprovals);
  const takeovers = useStore(allTakeovers);
  const agents = useStore((s) => s.agents);
  const respond = useStore((s) => s.respond);
  const focus = useStore((s) => s.focus);
  const setPane = useStore((s) => s.setPane);
  const toggleWheel = useStore((s) => s.toggleWheel);
  const collapsed = useStore((s) => s.approvalsRailCollapsed);
  const toggleCollapsed = useStore((s) => s.toggleApprovalsRail);

  const total = items.length + takeovers.length;

  if (collapsed) {
    return (
      <div className="rail rail-r approvals collapsed">
        <button className="rail-toggle" title="Show approvals" onClick={toggleCollapsed}>
          <span className="chevron">‹</span>
          <span className="label">Approvals</span>
          {total > 0 && <span className="count hot">{total}</span>}
        </button>
      </div>
    );
  }

  return (
    <div className="rail rail-r approvals">
      <div className="rail-head">
        Approvals <span className={`count${total ? ' hot' : ''}`}>{total}</span>
        <button className="rail-toggle-btn" title="Collapse approvals" onClick={toggleCollapsed}>
          ›
        </button>
      </div>
      {takeovers.map(({ agentId, takeover }) => (
        <div
          key={agentId + takeover.reqId}
          className="appr takeover"
          onClick={() => {
            focus(agentId);
            setPane('browser');
          }}
        >
          <div className="who">
            <span className="dot blocked" /> {agents[agentId]?.name ?? agentId} · browser
          </div>
          <div className="what">needs you — {takeover.reason}</div>
          <div className="acts">
            <button
              className="btn-approve"
              onClick={(e) => {
                e.stopPropagation();
                focus(agentId);
                setPane('browser');
                if (agents[agentId]?.browserOwner !== 'user') toggleWheel(agentId);
              }}
            >
              🖐 Take the wheel
            </button>
          </div>
        </div>
      ))}
      {items.length === 0 && takeovers.length === 0 ? (
        <div className="empty">No pending approvals. When an agent needs a decision it appears here.</div>
      ) : (
        items.map(({ agentId, approval }) => {
          const status = agents[agentId]?.status;
          const allow = approval.options.find((o) => /allow|yes|approve/i.test(o.name)) ?? approval.options[0];
          const deny = approval.options.find((o) => /reject|deny|no/i.test(o.name)) ?? approval.options[approval.options.length - 1];
          return (
            <div key={agentId + approval.reqId} className={`appr${status === 'error' ? ' err' : ''}`} onClick={() => focus(agentId)}>
              <div className="who">
                <span className={`dot ${status ?? 'blocked'}`} /> {agents[agentId]?.name ?? agentId}
              </div>
              <div className="what">{approval.title}</div>
              <div className="acts">
                <button
                  className="btn-approve"
                  onClick={(e) => {
                    e.stopPropagation();
                    respond(agentId, approval.reqId, allow.optionId);
                  }}
                >
                  ✓ {allow.name}
                </button>
                <button
                  className="btn-deny"
                  onClick={(e) => {
                    e.stopPropagation();
                    respond(agentId, approval.reqId, deny.optionId);
                  }}
                >
                  ✗ {deny.name}
                </button>
              </div>
            </div>
          );
        })
      )}
    </div>
  );
}
