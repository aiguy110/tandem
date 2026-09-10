import { useStore, allApprovals, allTakeovers, allTurnNotifications, notificationsSummary } from '../store';
import type { NotifSeverity } from '../store';
import { PermissionRequestDetails } from './PermissionRequest';

// Green / yellow / red, and the dot + card modifier that render it.
const SEVERITY_CLASS: Record<NotifSeverity, string> = {
  success: 'sev-success',
  attention: 'sev-attention',
  failure: 'sev-failure',
};
const SEVERITY_DOT: Record<NotifSeverity, string> = {
  success: 'idle',
  attention: 'blocked',
  failure: 'error',
};
const SEVERITY_LABEL: Record<NotifSeverity, string> = {
  success: 'Turn complete',
  attention: 'Needs your input',
  failure: 'Turn failed',
};

// Right rail — the conductor's inbox for completed turns, approvals, and browser
// requests across every agent. Clicking a completed-turn card marks it read.
export function ApprovalsRail() {
  const items = useStore(allApprovals);
  const takeovers = useStore(allTakeovers);
  const notifications = useStore(allTurnNotifications);
  const systemNotifications = useStore((s) => s.systemNotifications);
  const agents = useStore((s) => s.agents);
  const respond = useStore((s) => s.respond);
  const focus = useStore((s) => s.focus);
  const setPane = useStore((s) => s.setPane);
  const toggleWheel = useStore((s) => s.toggleWheel);
  const collapsed = useStore((s) => s.approvalsRailCollapsed);
  const toggleCollapsed = useStore((s) => s.toggleApprovalsRail);
  const summary = useStore(notificationsSummary);
  const actOnSystemNotification = useStore((s) => s.actOnSystemNotification);

  const total = summary.total;
  // The panel badge takes the color of its highest-severity item (red > yellow
  // > green).
  const badgeClass = summary.severity ? SEVERITY_CLASS[summary.severity] : '';

  if (collapsed) {
    return (
      <div className="rail rail-r approvals collapsed">
        <button className="rail-toggle" title="Show notifications" onClick={toggleCollapsed}>
          <span className="chevron">‹</span>
          <span className="label">Notifications</span>
          {total > 0 && <span className={`count ${badgeClass}`}>{total}</span>}
        </button>
      </div>
    );
  }

  return (
    <div className="rail rail-r approvals">
      <div className="rail-head">
        <span className="rail-head-label" onClick={toggleCollapsed}>
          Notifications <span className={`count${total ? ` ${badgeClass}` : ''}`}>{total}</span>
        </span>
        <button className="rail-toggle-btn" title="Collapse notifications" onClick={toggleCollapsed}>
          ›
        </button>
      </div>
      {systemNotifications.map((notification) => (
        <div key={notification.id} className={`appr notification system-notification ${SEVERITY_CLASS[notification.severity]}`}>
          <div className="who">
            <span className={`dot ${SEVERITY_DOT[notification.severity]}`} /> Tandem
          </div>
          <div className="what">{notification.title}</div>
          {notification.message && <div className="notification-message">{notification.message}</div>}
          {!!notification.actions?.length && (
            <div className="acts">
              {notification.actions.map((action) => (
                <button
                  key={action.id}
                  className={action.primary ? 'btn-approve' : 'btn-deny'}
                  onClick={() => void actOnSystemNotification(notification.id, action.id)}
                >
                  {action.label}
                </button>
              ))}
            </div>
          )}
        </div>
      ))}
      {notifications.map(({ agentId, notification }) => (
        <div
          key={`${agentId}-${notification.seq}`}
          className={`appr notification ${SEVERITY_CLASS[notification.severity]}`}
          onClick={() => focus(agentId)}
        >
          <div className="who">
            <span className={`dot ${SEVERITY_DOT[notification.severity]}`} /> {agents[agentId]?.name ?? agentId}
          </div>
          <div className="what">{SEVERITY_LABEL[notification.severity]}</div>
        </div>
      ))}
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
      {items.length === 0 && takeovers.length === 0 && notifications.length === 0 && systemNotifications.length === 0 ? (
        <div className="empty">No notifications. Completed agent turns and requests for attention appear here.</div>
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
              <div className="permission-request-rail"><PermissionRequestDetails title={approval.title} /></div>
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
