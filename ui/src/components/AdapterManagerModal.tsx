import { useEffect, useState } from 'react';
import { isLocalHost, useStore } from '../store';
import type { ForkTracking, ManagedAdapter, Profile } from '../wire';

/**
 * A fork-tracked row does not get a version picker. Its running code is a git
 * commit, not a published version, so offering the npm dropdown would present a
 * choice the daemon deliberately refuses (installing over a tracked fork would
 * silently drop the change the fork carries). The row instead states what the
 * fork is, whether upstream has moved, and the two ways out: rebase it, or
 * retire it once the change is upstreamed.
 */
function ForkRow({ row, fork, canSpawn, hostId }: { row: ManagedAdapter; fork: ForkTracking; canSpawn: boolean; hostId?: string }) {
  const behind = Boolean(fork.latestUpstreamVersion);
  const act = useStore((s) => s.actOnSystemNotification);
  const setModal = useStore((s) => s.setModal);
  const [spawning, setSpawning] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // The rebase offer is the updater's notification for this agent; acting on it
  // spawns (or reopens) the rebase agent exactly as the notification button does.
  const spawnRebase = async () => {
    setSpawning(true); setError(null);
    const local = `agent-update:${row.agent}`;
    const result = await act(!isLocalHost(hostId) ? `fed~${hostId}~${local}` : local, 'rebase');
    setSpawning(false);
    if (result.error) setError(result.error);
    else if (result.sessionId) setModal('none');
  };
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
          {fork.packageDir && (
            <>
              {' · package '}
              <code>{fork.packageDir}</code>
            </>
          )}
        </div>
        {fork.reason && <div className="adapter-fork-reason">{fork.reason}</div>}
        <div className="adapter-installed">
          {behind
            ? 'A rebase agent checks whether the change was upstreamed, then either retires the fork or rebases it.'
            : 'Tandem will offer a rebase when a newer upstream release appears.'}
          {fork.follow === 'off'
            ? ` Trees other hosts publish on ${fork.ref} are ignored.`
            : fork.follow === 'auto'
              ? ` Trees other hosts publish on ${fork.ref} are adopted automatically.`
              : ` Trees other hosts publish on ${fork.ref} are offered for adoption.`}
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
        {error && <div className="modal-err">{error}</div>}
      </div>
      <div className="adapter-controls adapter-controls-fork">
        {behind && (
          <button
            type="button"
            className="adapter-latest"
            disabled={spawning || !canSpawn}
            title={canSpawn ? 'Start (or open) the agent that rebases or retires this fork' : 'Choose a fork rebase agent profile above first'}
            onClick={() => void spawnRebase()}
          >
            {spawning ? 'Spawning…' : 'Spawn rebase agent'}
          </button>
        )}
        <code className="adapter-fork-cmd" title="Run in a terminal, or let a rebase agent do it">
          tandem acp status
        </code>
      </div>
    </div>
  );
}

/** The configured value may be a profile ID (what the picker stores) or a name. */
function matchProfile(profiles: Profile[], ref: string): Profile | undefined {
  if (!ref) return undefined;
  return profiles.find((p) => p.id === ref) ?? profiles.find((p) => p.name.toLowerCase() === ref.toLowerCase());
}

/**
 * Picks the agent profile Tandem spawns when a tracked fork falls behind
 * upstream. A profile can stop working on its own (the agent drops a model it
 * names), which is why a failed rebase notification links here.
 */
function RebaseProfileRow({ current, profiles, hostId, onSaved }: { current: string; profiles: Profile[]; hostId?: string; onSaved: () => Promise<void> }) {
  const save = useStore((s) => s.setAcpForkRebaseProfile);
  const matched = matchProfile(profiles, current);
  const [choice, setChoice] = useState(matched?.id ?? '');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => { setChoice(matchProfile(profiles, current)?.id ?? ''); }, [current, profiles]);
  const sorted = [...profiles].sort((a, b) => a.name.localeCompare(b.name));
  const apply = async () => {
    setBusy(true); setError(null);
    try { await save(choice, hostId); await onSaved(); }
    catch (err) { setError(err instanceof Error ? err.message : String(err)); }
    finally { setBusy(false); }
  };
  return <div className="adapter-row adapter-row-rebase-profile">
    <div className="adapter-main">
      <div className="adapter-heading"><span className="primary">Fork rebase agent</span>
        {current && !matched && <span className="adapter-fork-behind" title={current}>configured profile not found</span>}
      </div>
      <div className="adapter-installed">
        {current
          ? matched ? `Spawns ${matched.name} when a tracked fork falls behind upstream.` : `“${current}” no longer matches a saved profile; choose another.`
          : 'No profile chosen: fork rebase notifications are informational only.'}
      </div>
      {error && <div className="modal-err">{error}</div>}
    </div>
    <div className="adapter-controls">
      <select aria-label="Fork rebase agent profile" value={choice} disabled={busy} onChange={(e) => setChoice(e.target.value)}>
        <option value="">None</option>
        {sorted.map((p) => <option value={p.id} key={p.id}>{p.name}</option>)}
      </select>
      <button type="button" disabled={busy || choice === (matched?.id ?? '') && !(current && !matched)} onClick={() => void apply()}>{busy ? 'Saving…' : 'Save'}</button>
    </div>
  </div>;
}

export function AdapterManagerModal() {
  const setModal = useStore((s) => s.setModal);
  const list = useStore((s) => s.listAgentDistributions);
  const install = useStore((s) => s.installAgentDistribution);
  const listProfiles = useStore((s) => s.listProfiles);
  const hostId = useStore((s) => s.adaptersHostId) ?? undefined;
  const hostName = useStore((s) => (hostId ? s.hosts.find((h) => h.id === hostId)?.name ?? hostId : null));
  const [adapters, setAdapters] = useState<ManagedAdapter[]>([]);
  const [rebaseProfile, setRebaseProfile] = useState<string | undefined>(undefined);
  const [profiles, setProfiles] = useState<Profile[]>([]);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<Record<string, string>>({});
  const refresh = async () => {
    setLoading(true); setError(null);
    try {
      const { adapters: rows, rebaseProfile: profile } = await list(hostId);
      setAdapters(rows); setRebaseProfile(profile);
      setSelected(Object.fromEntries(rows.map((row) => [row.agent, row.currentVersion || row.availableVersions[0] || ''])));
      // Only a daemon that reports the setting can change it.
      if (profile !== undefined) setProfiles((await listProfiles(undefined, hostId)).profiles);
    }
    catch (err) { setError(err instanceof Error ? err.message : String(err)); }
    finally { setLoading(false); }
  };
  useEffect(() => { void refresh(); }, [hostId]);
  const choose = async (agent: string, version: string) => {
    setBusy(agent); setError(null);
    try { await install(agent, version, hostId); await refresh(); }
    catch (err) { setError(err instanceof Error ? err.message : String(err)); }
    finally { setBusy(null); }
  };
  const forked = adapters.filter((row) => row.fork).length;
  return <div className="modal-scrim" onMouseDown={(e) => e.target === e.currentTarget && setModal('none')}>
    <div className="modal adapter-modal" role="dialog" aria-modal="true" aria-labelledby="adapter-title" onKeyDown={(e) => e.key === 'Escape' && setModal('none')}>
      <div className="automation-header"><div><div className="primary" id="adapter-title">Managed ACP adapters{hostName && <span className="notification-host-badge">{hostName}</span>}</div><div className="sub">Choose the version used by new sessions. Existing sessions remain pinned.</div></div><button type="button" className="automation-close" onClick={() => setModal('none')} aria-label="Close adapter manager">×</button></div>
      <div className="rows adapter-rows">
        {loading && adapters.length === 0 && <div className="empty">Checking adapter versions…</div>}
        {rebaseProfile !== undefined && <RebaseProfileRow current={rebaseProfile} profiles={profiles} hostId={hostId} onSaved={refresh} />}
        {adapters.map((row) => {
          if (row.fork) return <ForkRow row={row} fork={row.fork} canSpawn={rebaseProfile !== ''} hostId={hostId} key={row.agent} />;
          // Deliberately not availableVersions[0]: that can be a prerelease.
          const latest = row.latestVersion ?? '';
          const choice = selected[row.agent] ?? row.currentVersion;
          const changing = busy === row.agent;
          return <div className="adapter-row" key={row.agent}>
            <div className="adapter-main">
              <div className="adapter-heading"><span className="primary">{row.agent}</span><span className="adapter-current">current {row.currentVersion || 'not installed'}</span></div>
              <div className="automation-path">{row.package}</div>
              <div className="adapter-installed">Installed: {row.installedVersions.length ? row.installedVersions.join(', ') : 'none'}</div>
            </div>
            <div className="adapter-controls">
              <select aria-label={`${row.agent} version`} value={choice} disabled={changing} onChange={(e) => setSelected((all) => ({ ...all, [row.agent]: e.target.value }))}>
                {row.availableVersions.map((version) => <option value={version} key={version}>{version}{version === row.currentVersion ? ' (current)' : row.installedVersions.includes(version) ? ' (installed)' : ''}</option>)}
              </select>
              <button type="button" disabled={changing || !choice || choice === row.currentVersion} onClick={() => void choose(row.agent, choice)}>{changing ? 'Installing…' : row.installedVersions.includes(choice) ? 'Roll back' : 'Use version'}</button>
              {latest && latest !== row.currentVersion && <button type="button" className="adapter-latest" disabled={changing} onClick={() => void choose(row.agent, latest)}>Update to latest</button>}
            </div>
          </div>;
        })}
      </div>
      {error && <div className="modal-err">{error}</div>}
      <div className="foot automation-foot"><span>{adapters.length} managed {adapters.length === 1 ? 'adapter' : 'adapters'}{forked > 0 && `, ${forked} tracking a fork`}</span><button type="button" disabled={loading || !!busy} onClick={() => void refresh()}>Refresh</button></div>
    </div>
  </div>;
}
