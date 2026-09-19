import { useEffect, useState } from 'react';
import { useStore } from '../store';
import type { ForkTracking, ManagedAdapter } from '../wire';

/**
 * A fork-tracked row does not get a version picker. Its running code is a git
 * commit, not a published version, so offering the npm dropdown would present a
 * choice the daemon deliberately refuses (installing over a tracked fork would
 * silently drop the change the fork carries). The row instead states what the
 * fork is, whether upstream has moved, and the two ways out: rebase it, or
 * retire it once the change is upstreamed.
 */
function ForkRow({ row, fork }: { row: ManagedAdapter; fork: ForkTracking }) {
  const behind = Boolean(fork.latestUpstreamVersion);
  const prs = fork.upstreamPrs ?? [];
  const commitUrl = `${fork.repo.replace(/\.git$/, '')}/commit/${fork.commit}`;
  const upstreamName = fork.upstreamRepo?.replace(/^https?:\/\/github\.com\//, '');
  return (
    <div className={`adapter-row adapter-row-fork${behind ? ' is-behind' : ''}`} key={row.agent}>
      <div className="adapter-main">
        <div className="adapter-heading">
          <span className="primary">{row.agent}</span>
          <span className="adapter-fork-badge" title={`Installed from ${fork.repo} at ${fork.commit}`}>
            tracking fork
          </span>
          {behind ? (
            <span className="adapter-fork-behind">upstream {fork.latestUpstreamVersion} available</span>
          ) : (
            <span className="adapter-current">level with upstream</span>
          )}
          {!fork.installed && (
            <span className="adapter-fork-pending" title="The pinned commit is not built yet; it installs on the next spawn of this agent.">
              not built yet
            </span>
          )}
        </div>
        <div className="automation-path">
          <a href={commitUrl} target="_blank" rel="noreferrer">
            {fork.repo.replace(/^https?:\/\//, '')}
          </a>
          {' · '}
          {fork.ref} @ <code>{fork.shortCommit}</code>
          {' · rebased onto '}
          {fork.upstreamPackage}@{fork.upstreamVersion}
        </div>
        {fork.reason && <div className="adapter-fork-reason">{fork.reason}</div>}
        <div className="adapter-installed">
          {behind
            ? 'A rebase agent checks whether the change was upstreamed, then either retires the fork or rebases it.'
            : 'Tandem will offer a rebase when a newer upstream release appears.'}
          {prs.length > 0 && upstreamName && (
            <>
              {' Watching '}
              {prs.map((n, i) => (
                <span key={n}>
                  {i > 0 && ', '}
                  <a href={`https://github.com/${upstreamName}/pull/${n}`} target="_blank" rel="noreferrer">
                    #{n}
                  </a>
                </span>
              ))}
              {'.'}
            </>
          )}
        </div>
      </div>
      <div className="adapter-controls adapter-controls-fork">
        <code className="adapter-fork-cmd" title="Run in a terminal, or let a rebase agent do it">
          tandem acp status
        </code>
      </div>
    </div>
  );
}

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
  const forked = adapters.filter((row) => row.fork).length;
  return <div className="modal-scrim" onMouseDown={(e) => e.target === e.currentTarget && setModal('none')}>
    <div className="modal adapter-modal" role="dialog" aria-modal="true" aria-labelledby="adapter-title" onKeyDown={(e) => e.key === 'Escape' && setModal('none')}>
      <div className="automation-header"><div><div className="primary" id="adapter-title">Managed ACP adapters</div><div className="sub">Choose the version used by new sessions. Existing sessions remain pinned.</div></div><button type="button" className="automation-close" onClick={() => setModal('none')} aria-label="Close adapter manager">×</button></div>
      <div className="rows adapter-rows">
        {loading && adapters.length === 0 && <div className="empty">Checking adapter versions…</div>}
        {adapters.map((row) => { if (row.fork) return <ForkRow row={row} fork={row.fork} key={row.agent} />; const latest = row.availableVersions[0] ?? ''; const choice = selected[row.agent] ?? row.currentVersion; const changing = busy === row.agent; return <div className="adapter-row" key={row.agent}>
          <div className="adapter-main"><div className="adapter-heading"><span className="primary">{row.agent}</span><span className="adapter-current">current {row.currentVersion || 'not installed'}</span></div><div className="automation-path">{row.package} · compatible {row.constraint}</div><div className="adapter-installed">Installed: {row.installedVersions.length ? row.installedVersions.join(', ') : 'none'}</div></div>
          <div className="adapter-controls"><select aria-label={`${row.agent} version`} value={choice} disabled={changing} onChange={(e) => setSelected((all) => ({ ...all, [row.agent]: e.target.value }))}>{row.availableVersions.map((version) => <option value={version} key={version}>{version}{version === row.currentVersion ? ' (current)' : row.installedVersions.includes(version) ? ' (installed)' : ''}</option>)}</select><button type="button" disabled={changing || !choice || choice === row.currentVersion} onClick={() => void choose(row.agent, choice)}>{changing ? 'Installing…' : row.installedVersions.includes(choice) ? 'Roll back' : 'Use version'}</button>{latest !== row.currentVersion && <button type="button" className="adapter-latest" disabled={changing} onClick={() => void choose(row.agent, latest)}>Update to latest</button>}</div>
        </div>; })}
      </div>
      {error && <div className="modal-err">{error}</div>}
      <div className="foot automation-foot"><span>{adapters.length} managed {adapters.length === 1 ? 'adapter' : 'adapters'}{forked > 0 && `, ${forked} tracking a fork`}</span><button type="button" disabled={loading || !!busy} onClick={() => void refresh()}>Refresh</button></div>
    </div>
  </div>;
}
