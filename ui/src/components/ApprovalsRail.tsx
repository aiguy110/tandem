import { useEffect, useState } from 'react';
import { useStore, allApprovals, allBrowserInstanceRequests, allSecretRequests, allTakeovers, allTurnNotifications, notificationsSummary } from '../store';
import type { BrowserInstanceRequest, NotifSeverity } from '../store';
import type { BrowserSnapshot } from '../wire';
import type { SecretRequest } from '../store';
import { storedToken } from '../ws/client';
import { PermissionRequestDetails } from './PermissionRequest';
import { usePresence } from '../transitions';

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


// System notifications raised by the daemon's MCP sign-in service.
const MCP_AUTH_PREFIX = 'mcp-auth:';

// Right rail — the conductor's inbox for completed turns, approvals, and browser
// requests across every agent. Clicking a completed-turn card marks it read.
export function ApprovalsRail({ onResizeStart }: { onResizeStart?: (clientX: number) => void }) {
	const [pendingSystemAction, setPendingSystemAction] = useState<string | null>(null);
  const [systemActionError, setSystemActionError] = useState<{ id: string; message: string } | null>(null);
  const [secretEntry, setSecretEntry] = useState<{ sessionId: string; request: SecretRequest } | null>(null);
  const items = useStore(allApprovals);
  const takeovers = useStore(allTakeovers);
  const secretRequests = useStore(allSecretRequests);
  const browserRequests = useStore(allBrowserInstanceRequests);
  const notifications = useStore(allTurnNotifications);
  const systemNotifications = useStore((s) => s.systemNotifications);
  const agents = useStore((s) => s.sessions);
  const respond = useStore((s) => s.respond);
  const focus = useStore((s) => s.focus);
  const setPane = useStore((s) => s.setPane);
  const toggleWheel = useStore((s) => s.toggleWheel);
  const acknowledgeTakeover = useStore((s) => s.acknowledgeTakeover);
  const setBrowserView = useStore((s) => s.setBrowserView);
  const collapsed = useStore((s) => s.approvalsRailCollapsed);
  const toggleCollapsed = useStore((s) => s.toggleApprovalsRail);
  const { mounted: expandedMounted, closing: railClosing } = usePresence(!collapsed, 225);
  const summary = useStore(notificationsSummary);
  const actOnSystemNotification = useStore((s) => s.actOnSystemNotification);
  const authorizeMcpServer = useStore((s) => s.authorizeMcpServer);

  const total = summary.total;
  // The panel badge takes the color of its highest-severity item (red > yellow
  // > green).
  const badgeClass = summary.severity ? SEVERITY_CLASS[summary.severity] : '';

  return (
    <div className={`rail rail-r approvals${collapsed ? ' collapsed' : ''}`}>
      {collapsed && !expandedMounted && (
        <button className="rail-toggle" title="Show notifications" onClick={toggleCollapsed}>
          <span className="chevron">‹</span>
          <span className="label">Notifications</span>
          {total > 0 && <span className={`count ${badgeClass}`}>{total}</span>}
        </button>
      )}
      {expandedMounted && (
      <div className={`rail-expanded${railClosing ? ' closing' : ' entering'}`} aria-hidden={railClosing}>
      <div
        className="dock-resize-handle dock-resize-handle-left"
        role="separator"
        aria-label="Resize notifications panel"
        aria-orientation="vertical"
        onPointerDown={(event) => {
          event.preventDefault();
          onResizeStart?.(event.clientX);
        }}
      />
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
            {notification.hostId && <span className="notification-host-badge">{notification.hostName || notification.hostId}</span>}
          </div>
          <div className="what">{notification.title}</div>
          {notification.message && <div className="notification-message">{notification.message}</div>}
          {!!notification.actions?.length && (
            <div className="acts">
              {notification.actions.map((action) => {
                // MCP sign-in opens the provider's page in a new tab, which
                // must happen inside this click; the daemon only supplies
                // the URL. A relayed one would return to that host's own
                // daemon, which this browser may not reach.
                const mcpAuthorize = action.id === 'authorize' && notification.id.startsWith(MCP_AUTH_PREFIX);
                const remoteMcpAuthorize = mcpAuthorize && !!notification.hostId;
                return (
                  <button
                    key={action.id}
                    className={action.primary ? 'btn-approve' : 'btn-deny'}
                    disabled={pendingSystemAction === `${notification.id}:${action.id}` || remoteMcpAuthorize}
                    title={remoteMcpAuthorize ? `Authorize from ${notification.hostName || notification.hostId}'s own Tandem UI` : undefined}
                    onClick={() => {
                      const key = `${notification.id}:${action.id}`;
                      setPendingSystemAction(key);
                      setSystemActionError(null);
                      const work = mcpAuthorize
                        ? authorizeMcpServer(notification.id.slice(MCP_AUTH_PREFIX.length))
                        : actOnSystemNotification(notification.id, action.id).then((result) => {
                            if (result.error) throw new Error(result.error);
                          });
                      void work
                        .catch((err) => setSystemActionError({ id: notification.id, message: err instanceof Error ? err.message : String(err) }))
                        .finally(() => setPendingSystemAction(null));
                    }}
                  >
                    {pendingSystemAction === `${notification.id}:${action.id}` ? 'Working…' : action.label}
                  </button>
                );
              })}
            </div>
          )}
          {systemActionError?.id === notification.id && <div className="notification-message notification-error">{systemActionError.message}</div>}
        </div>
      ))}
      {notifications.map(({ sessionId, notification }) => (
        <div
          key={`${sessionId}-${notification.seq}`}
          className={`appr notification ${SEVERITY_CLASS[notification.severity]}`}
          onClick={() => focus(sessionId)}
        >
          <div className="who">
            <span className={`dot ${SEVERITY_DOT[notification.severity]}`} /> {agents[sessionId]?.name ?? sessionId}
          </div>
          <div className="what">{SEVERITY_LABEL[notification.severity]}</div>
        </div>
      ))}
      {takeovers.map(({ sessionId, takeover }) => (
        <div
          key={sessionId + takeover.reqId}
          className="appr takeover"
          onClick={() => {
            focus(sessionId);
            setPane('browser');
            setBrowserView(sessionId, takeover.browserId ?? 'default');
          }}
        >
          <div className="who">
            <span className="dot blocked" /> {agents[sessionId]?.name ?? sessionId} · browser{takeover.browserId && takeover.browserId !== 'default' ? ` · ${agents[sessionId]?.browserInstances.find((i) => i.id === takeover.browserId)?.name ?? takeover.browserId}` : ''}
          </div>
          <div className="what">needs you — {takeover.reason}</div>
          <div className="acts">
            <button
              className="btn-approve"
              onClick={(e) => {
                e.stopPropagation();
                focus(sessionId);
                setPane('browser');
                const browserId = takeover.browserId ?? 'default';
                setBrowserView(sessionId, browserId);
                if (useStore.getState().sessions[sessionId]?.browserStates[browserId]?.owner === 'user') {
                  acknowledgeTakeover(sessionId, browserId);
                } else {
                  toggleWheel(sessionId);
                }
              }}
            >
              🖐 Take the wheel
            </button>
          </div>
        </div>
      ))}
      {secretRequests.map(({ sessionId, request }) => (
        <div key={request.requestId} className="appr secret-request" onClick={() => setSecretEntry({ sessionId, request })}>
          <div className="who"><span className="dot blocked" /> {agents[sessionId]?.name ?? sessionId} · secret</div>
          <div className="what">requests {request.service}</div>
          <div className="notification-message">For {request.usage === 'file' ? request.path : request.origin}</div>
          <div className="acts"><button className="btn-approve" onClick={(e) => { e.stopPropagation(); setSecretEntry({ sessionId, request }); }}>Enter securely…</button></div>
        </div>
      ))}
      {browserRequests.map(({ sessionId, request }) => (
        <BrowserRequestCard key={request.requestId} sessionId={sessionId} request={request} agentName={agents[sessionId]?.name ?? sessionId} />
      ))}
      {items.length === 0 && takeovers.length === 0 && secretRequests.length === 0 && browserRequests.length === 0 && notifications.length === 0 && systemNotifications.length === 0 ? (
        <div className="empty">No notifications. Completed session turns and requests for attention appear here.</div>
      ) : (
        items.map(({ sessionId, approval }) => {
          const status = agents[sessionId]?.status;
          const allow = approval.options.find((o) => /allow|yes|approve/i.test(o.name)) ?? approval.options[0];
          const deny = approval.options.find((o) => /reject|deny|no/i.test(o.name)) ?? approval.options[approval.options.length - 1];
          return (
            <div key={sessionId + approval.reqId} className={`appr${status === 'error' ? ' err' : ''}`} onClick={() => focus(sessionId)}>
              <div className="who">
                <span className={`dot ${status ?? 'blocked'}`} /> {agents[sessionId]?.name ?? sessionId}
              </div>
              <div className="permission-request-rail"><PermissionRequestDetails title={approval.title} /></div>
              <div className="acts">
                <button
                  className="btn-approve"
                  onClick={(e) => {
                    e.stopPropagation();
                    respond(sessionId, approval.reqId, allow.optionId);
                  }}
                >
                  ✓ {allow.name}
                </button>
                <button
                  className="btn-deny"
                  onClick={(e) => {
                    e.stopPropagation();
                    respond(sessionId, approval.reqId, deny.optionId);
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
      )}
      {secretEntry && <SecretEntryModal entry={secretEntry} onClose={() => setSecretEntry(null)} />}
    </div>
  );
}

// An agent's request for an additional browser instance. Nothing launches
// and no saved profile is exposed until the human approves here.
function BrowserRequestCard({ sessionId, request, agentName }: { sessionId: string; request: BrowserInstanceRequest; agentName: string }) {
  const resolveBrowserInstance = useStore((s) => s.resolveBrowserInstance);
  const listSnapshots = useStore((s) => s.listSnapshots);
  const hostId = useStore((s) => s.sessions[sessionId]?.hostId);
  const [profiles, setProfiles] = useState<BrowserSnapshot[] | null>(null);
  const [choice, setChoice] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const userChoice = request.source.kind === 'user_choice';
  useEffect(() => {
    if (!userChoice) return;
    void listSnapshots(hostId).then(setProfiles).catch(() => setProfiles([]));
  }, [userChoice, hostId, listSnapshots]);
  const decide = async (approve: boolean) => {
    setBusy(true);
    setError('');
    const result = await resolveBrowserInstance(sessionId, request.requestId, approve, approve && userChoice ? choice : undefined);
    if (result.error) {
      setError(result.error);
      setBusy(false);
    }
  };
  const what = request.source.kind === 'saved_profile'
    ? <>wants a new browser from saved profile <b>{request.source.profileName ?? request.source.profileId}</b></>
    : request.source.kind === 'fresh'
      ? <>wants a new browser with a fresh profile</>
      : <>wants a new browser — you choose its starting state</>;
  return (
    <div className="appr browser-request" onClick={(e) => e.stopPropagation()}>
      <div className="who"><span className="dot blocked" /> {agentName} · browser</div>
      <div className="what">{what}{request.name ? <> (“{request.name}”)</> : null}</div>
      <div className="notification-message">Agent-provided reason (untrusted): {request.reason}</div>
      {userChoice && (
        <select className="browser-request-profile" value={choice} disabled={busy} onChange={(e) => setChoice(e.target.value)} aria-label="Starting state">
          <option value="">Fresh profile</option>
          {(profiles ?? []).map((profile) => <option key={profile.id} value={profile.id}>{profile.name}</option>)}
        </select>
      )}
      {(request.source.kind === 'saved_profile' || (userChoice && choice)) && (
        <div className="notification-message">Grants this agent the accounts and site data stored in that saved profile. Its existing browsers are unaffected.</div>
      )}
      {error && <div className="modal-err">{error}</div>}
      <div className="acts">
        <button className="btn-approve" disabled={busy} onClick={() => void decide(true)}>{busy ? 'Opening…' : 'Open browser'}</button>
        <button className="btn-deny" disabled={busy} onClick={() => void decide(false)}>Deny</button>
      </div>
    </div>
  );
}

function SecretEntryModal({ entry, onClose }: { entry: { sessionId: string; request: SecretRequest }; onClose: () => void }) {
  const [value, setValue] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const resolve = async (deny: boolean) => {
    setBusy(true); setError('');
    try {
      const token = storedToken();
      if (!token) throw new Error('Tandem authentication is unavailable');
      const response = await fetch('/internal/secrets/resolve', {
        method: 'POST', headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' },
        body: JSON.stringify({ requestId: entry.request.requestId, secret: deny ? '' : value, deny }),
      });
      const body = await response.json().catch(() => ({})) as { error?: string };
      if (!response.ok) throw new Error(body.error || `Request failed (${response.status})`);
      setValue(''); onClose();
    } catch (cause) { setError(cause instanceof Error ? cause.message : String(cause)); setBusy(false); }
  };
  return <div className="modal-scrim" onMouseDown={(e) => { if (e.target === e.currentTarget && !busy) onClose(); }}>
    <div className="modal secret-modal" role="dialog" aria-modal="true" aria-labelledby="secret-title">
      <div className="secret-body">
        <div className="adv-section" id="secret-title">Secure secret request</div>
        <p><strong>{entry.request.service}</strong> credential for <code>{entry.request.usage === 'file' ? entry.request.path : entry.request.origin}</code></p>
        <p className="sub">Agent-provided reason (untrusted): {entry.request.reason}</p>
        <p className="secret-assurance">The value is sent directly to Tandem’s in-memory broker. It is not added to the prompt, transcript, event log, or agent tool result.</p>
        <label>Secret value<input autoFocus type="password" autoComplete="off" spellCheck={false} value={value} onChange={(e) => setValue(e.target.value)} /></label>
        <div className="sub">{entry.request.usage === 'file'
          ? <>Written only to the approved workspace file. A cooperative agent can edit it through redacted Tandem file tools without putting the value in its transcript.</>
          : <>Injected only as <code>{entry.request.headerName}</code> when calling the approved origin.</>}</div>
      </div>
      {error && <div className="modal-err">{error}</div>}
      <div className="foot"><button className="btn-deny" disabled={busy} onClick={() => void resolve(true)}>Deny</button><span style={{ flex: 1 }} /><button disabled={busy || !value} className="btn-approve" onClick={() => void resolve(false)}>{busy ? 'Submitting…' : 'Grant for session'}</button></div>
    </div>
  </div>;
}
