import { useEffect, useState } from 'react';
import { useStore } from '../store';
import type { ManagedAdapter } from '../wire';

export function AdapterManagerModal() {
  const setModal = useStore((s) => s.setModal);
  const list = useStore((s) => s.listAgentDistributions);
  const install = useStore((s) => s.installAgentDistribution);
  const [adapters, setAdapters] = useState<ManagedAdapter[]>([]);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<Record<string, string>>({});
  const refresh = async () => {
    setLoading(true); setError(null);
    try { const rows = await list(); setAdapters(rows); setSelected(Object.fromEntries(rows.map((row) => [row.agent, row.currentVersion || row.availableVersions[0] || '']))); }
    catch (err) { setError(err instanceof Error ? err.message : String(err)); }
    finally { setLoading(false); }
  };
  useEffect(() => { void refresh(); }, []);
  const choose = async (agent: string, version: string) => {
    setBusy(agent); setError(null);
    try { await install(agent, version); await refresh(); }
    catch (err) { setError(err instanceof Error ? err.message : String(err)); }
    finally { setBusy(null); }
  };
  return <div className="modal-scrim" onMouseDown={(e) => e.target === e.currentTarget && setModal('none')}>
    <div className="modal adapter-modal" role="dialog" aria-modal="true" aria-labelledby="adapter-title" onKeyDown={(e) => e.key === 'Escape' && setModal('none')}>
      <div className="automation-header"><div><div className="primary" id="adapter-title">Managed ACP adapters</div><div className="sub">Choose the version used by new sessions. Existing sessions remain pinned.</div></div><button type="button" className="automation-close" onClick={() => setModal('none')} aria-label="Close adapter manager">×</button></div>
      <div className="rows adapter-rows">
        {loading && adapters.length === 0 && <div className="empty">Checking adapter versions…</div>}
        {adapters.map((row) => { const latest = row.availableVersions[0] ?? ''; const choice = selected[row.agent] ?? row.currentVersion; const changing = busy === row.agent; return <div className="adapter-row" key={row.agent}>
          <div className="adapter-main"><div className="adapter-heading"><span className="primary">{row.agent}</span><span className="adapter-current">current {row.currentVersion || 'not installed'}</span></div><div className="automation-path">{row.package} · compatible {row.constraint}</div><div className="adapter-installed">Installed: {row.installedVersions.length ? row.installedVersions.join(', ') : 'none'}</div></div>
          <div className="adapter-controls"><select aria-label={`${row.agent} version`} value={choice} disabled={changing} onChange={(e) => setSelected((all) => ({ ...all, [row.agent]: e.target.value }))}>{row.availableVersions.map((version) => <option value={version} key={version}>{version}{version === row.currentVersion ? ' (current)' : row.installedVersions.includes(version) ? ' (installed)' : ''}</option>)}</select><button type="button" disabled={changing || !choice || choice === row.currentVersion} onClick={() => void choose(row.agent, choice)}>{changing ? 'Installing…' : row.installedVersions.includes(choice) ? 'Roll back' : 'Use version'}</button>{latest !== row.currentVersion && <button type="button" className="adapter-latest" disabled={changing} onClick={() => void choose(row.agent, latest)}>Update to latest</button>}</div>
        </div>; })}
      </div>
      {error && <div className="modal-err">{error}</div>}
      <div className="foot automation-foot"><span>{adapters.length} managed {adapters.length === 1 ? 'adapter' : 'adapters'}</span><button type="button" disabled={loading || !!busy} onClick={() => void refresh()}>Refresh</button></div>
    </div>
  </div>;
}
