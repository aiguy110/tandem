import { useEffect, useState } from 'react';
import { useStore } from '../store';
import type { McpServerState, McpServerStatus } from '../wire';

const STATE_LABEL: Record<McpServerState, string> = {
  checking: 'checking',
  open: 'no sign-in needed',
  needs_auth: 'needs sign-in',
  authorized: 'authorized',
  static: 'header auth',
  error: 'unreachable',
};

function describe(row: McpServerStatus): string {
  switch (row.state) {
    case 'authorized': {
      const parts = ['Agents reach it through Tandem, which attaches and refreshes the token.'];
      if (row.expiresAt) parts.push(`Access token ${row.refreshable ? 'refreshes' : 'expires'} ${new Date(row.expiresAt).toLocaleString()}.`);
      if (row.scope) parts.push(`Scope: ${row.scope}.`);
      return parts.join(' ');
    }
    case 'needs_auth':
      return 'Requires OAuth sign-in. Agents do not get its tools until Tandem is authorized.';
    case 'static':
      return 'Uses the Authorization header from config.yml; Tandem passes it through unchanged.';
    case 'open':
      return 'Accepted an unauthenticated connection; agents connect to it directly.';
    case 'error':
      return 'Tandem could not reach it; agents still try to connect themselves.';
    default:
      return 'Checking whether it requires sign-in…';
  }
}

// Sign-in state for remote (HTTP) MCP servers. The daemon holds each OAuth
// authorization and proxies agents' traffic, so one sign-in here covers every
// harness (docs/mcp-auth.md).
export function McpServersModal() {
  const setModal = useStore((s) => s.setModal);
  const servers = useStore((s) => s.mcpServers);
  const list = useStore((s) => s.listMcpServers);
  const authorize = useStore((s) => s.authorizeMcpServer);
  const signOut = useStore((s) => s.signOutMcpServer);
  const recheck = useStore((s) => s.recheckMcpServer);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const refresh = async () => {
    setLoading(true); setError(null);
    try { await list(); }
    catch (err) { setError(err instanceof Error ? err.message : String(err)); }
    finally { setLoading(false); }
  };
  useEffect(() => { void refresh(); }, []);
  // Each action is started synchronously from its click; authorize relies on
  // that to open the sign-in tab.
  const run = (key: string, action: () => Promise<void>) => {
    setBusy(key); setError(null);
    action()
      .catch((err) => setError(err instanceof Error ? err.message : String(err)))
      .finally(() => setBusy(null));
  };
  const rows = servers ?? [];
  const waiting = rows.filter((row) => row.state === 'needs_auth').length;
  return <div className="modal-scrim" onMouseDown={(e) => e.target === e.currentTarget && setModal('none')}>
    <div className="modal adapter-modal" role="dialog" aria-modal="true" aria-labelledby="mcp-title" onKeyDown={(e) => e.key === 'Escape' && setModal('none')}>
      <div className="automation-header"><div><div className="primary" id="mcp-title">MCP servers</div><div className="sub">Sign in once and every new agent session gets the server's tools. Restart a running session's harness to give it access.</div></div><button type="button" className="automation-close" onClick={() => setModal('none')} aria-label="Close MCP servers">×</button></div>
      <div className="rows adapter-rows">
        {loading && servers === null && <div className="empty">Loading MCP servers…</div>}
        {!loading && rows.length === 0 && <div className="empty">No HTTP MCP servers are configured. Add one with <code>tandem mcp add --transport http NAME URL</code>.</div>}
        {rows.map((row) => {
          const working = busy === row.key;
          return <div className={`adapter-row mcp-row mcp-${row.state}`} key={row.key}>
            <div className="adapter-main">
              <div className="adapter-heading"><span className="primary mcp-name">{row.name}</span><span className={`mcp-state mcp-state-${row.state}`}>{STATE_LABEL[row.state] ?? row.state}</span>{!row.configured && <span className="adapter-fork-pending" title="Defined in a project's .tandem/.config.yml">project</span>}</div>
              <div className="automation-path">{row.url}</div>
              <div className="adapter-installed">{describe(row)}</div>
              {row.error && <div className="adapter-fork-reason mcp-error">{row.error}</div>}
            </div>
            <div className="adapter-controls">
              {(row.state === 'needs_auth' || row.state === 'authorized') && <button type="button" className={row.state === 'needs_auth' ? 'adapter-latest' : undefined} disabled={working} onClick={() => run(row.key, () => authorize(row.key))}>{working ? 'Opening…' : row.state === 'authorized' ? 'Re-authorize' : 'Authorize'}</button>}
              {row.state === 'authorized' && <button type="button" disabled={working} onClick={() => run(row.key, () => signOut(row.key))}>Sign out</button>}
              {row.state !== 'authorized' && row.state !== 'static' && <button type="button" disabled={working} onClick={() => run(row.key, () => recheck(row.key))}>Check again</button>}
            </div>
          </div>;
        })}
      </div>
      {error && <div className="modal-err">{error}</div>}
      <div className="foot automation-foot"><span>{rows.length} HTTP {rows.length === 1 ? 'server' : 'servers'}{waiting > 0 && `, ${waiting} awaiting sign-in`}</span><button type="button" disabled={loading} onClick={() => void refresh()}>Refresh</button></div>
    </div>
  </div>;
}
