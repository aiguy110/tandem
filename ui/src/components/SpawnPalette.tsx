import { useEffect, useMemo, useRef, useState } from 'react';
import { useStore } from '../store';
import { fuzzyFilter } from '../fuzzy';
import type { RepoInfo, SpawnOptions, SpawnSpec } from '../wire';

const RECENT_DIRS_KEY = 'tandem.recentDirs';
const RECENT_DIRS_MAX = 3;
const SPAWN_SETTINGS_KEY = 'tandem.spawnSettings.v1';
const SPAWN_AGENT_KEY = 'tandem.spawnAgent.v1';
const AGENT_TYPES = ['claude', 'codex', 'pi'] as const;

type SavedSettings = { model?: string; effort?: string; permission?: string };

function settingsKey(agent: string, project: string) {
  return `${agent}\u0000${project}`;
}

function loadSpawnSettings(agent: string, project: string): SavedSettings {
  try {
    const all = JSON.parse(localStorage.getItem(SPAWN_SETTINGS_KEY) || '{}') as Record<string, SavedSettings>;
    return all[settingsKey(agent, project)] ?? {};
  } catch { return {}; }
}

function saveSpawnSettings(agent: string, project: string, value: SavedSettings) {
  try {
    const all = JSON.parse(localStorage.getItem(SPAWN_SETTINGS_KEY) || '{}') as Record<string, SavedSettings>;
    all[settingsKey(agent, project)] = value;
    localStorage.setItem(SPAWN_SETTINGS_KEY, JSON.stringify(all));
  } catch { /* localStorage unavailable */ }
}

function loadProjectAgent(project: string): string {
  try {
    const all = JSON.parse(localStorage.getItem(SPAWN_AGENT_KEY) || '{}') as Record<string, string>;
    return AGENT_TYPES.includes(all[project] as (typeof AGENT_TYPES)[number]) ? all[project] : 'claude';
  } catch { return 'claude'; }
}

function saveProjectAgent(project: string, agent: string) {
  try {
    const all = JSON.parse(localStorage.getItem(SPAWN_AGENT_KEY) || '{}') as Record<string, string>;
    all[project] = agent;
    localStorage.setItem(SPAWN_AGENT_KEY, JSON.stringify(all));
  } catch { /* localStorage unavailable */ }
}

function loadRecentDirs(): string[] {
  try {
    const raw = localStorage.getItem(RECENT_DIRS_KEY);
    return raw ? (JSON.parse(raw) as string[]) : [];
  } catch {
    return [];
  }
}

function recordRecentDir(path: string) {
  try {
    const existing = loadRecentDirs().filter((p) => p !== path);
    existing.unshift(path);
    localStorage.setItem(RECENT_DIRS_KEY, JSON.stringify(existing.slice(0, 20)));
  } catch {
    // localStorage unavailable — recency just won't persist
  }
}

// Quick-spawn palette (D9): dir-first fuzzy modal backed by list_dirs.
//   Enter               → spawn worktree defaults + focus jumps
//   Tab → type task → Enter → spawn AND dispatch
//   ⌘/Ctrl+Enter        → reveal advanced (adapter / branch / baseRef / name / existing)
// Structured spawn errors (dir_occupied) offer worktree-instead / attach.
export function SpawnPalette() {
  const dirs = useStore((s) => s.dirs);
  const spawn = useStore((s) => s.spawn);
  const getSpawnOptions = useStore((s) => s.getSpawnOptions);
  const focus = useStore((s) => s.focus);
  const setModal = useStore((s) => s.setModal);
  const agents = useStore((s) => s.agents);

  const [query, setQuery] = useState('');
  const [sel, setSel] = useState(0);
  const [taskMode, setTaskMode] = useState(false);
  const [task, setTask] = useState('');
  const [advanced, setAdvanced] = useState(false);
  const [adapter, setAdapter] = useState<'acp' | 'pty'>('acp');
  const [agent, setAgent] = useState<string>('claude');
  const [branch, setBranch] = useState('');
  const [baseRef, setBaseRef] = useState('');
  const [name, setName] = useState('');
  const [useExisting, setUseExisting] = useState(false);
  const [model, setModel] = useState('');
  const [effort, setEffort] = useState('');
  const [permission, setPermission] = useState('');
  const [spawnOptions, setSpawnOptions] = useState<SpawnOptions | null>(null);
  const [optionsBusy, setOptionsBusy] = useState(false);
  const [optionsError, setOptionsError] = useState('');
  const [error, setError] = useState<{ code: string; msg: string; dir: RepoInfo } | null>(null);
  const [busy, setBusy] = useState(false);

  const taskRef = useRef<HTMLInputElement>(null);

  const filtered = useMemo(() => {
    const matched = fuzzyFilter(query, dirs, (d) => d.name + ' ' + d.path);
    if (query) return matched;
    const recentPaths = loadRecentDirs();
    const byPath = new Map(dirs.map((d) => [d.path, d]));
    const recent = recentPaths.map((p) => byPath.get(p)).filter((d): d is RepoInfo => !!d);
    return recent.length > 0 ? recent.slice(0, RECENT_DIRS_MAX) : matched.slice(0, RECENT_DIRS_MAX);
  }, [query, dirs]);
  useEffect(() => setSel(0), [query]);
  const selectedDir = filtered[sel];
  useEffect(() => {
    if (selectedDir) setAgent(loadProjectAgent(selectedDir.path));
  }, [selectedDir?.path]);
  useEffect(() => {
    if (!selectedDir || adapter !== 'acp') return;
    const saved = loadSpawnSettings(agent, selectedDir.path);
    setModel(saved.model ?? '');
    setEffort(saved.effort ?? '');
    setPermission(saved.permission ?? '');
  }, [agent, adapter, selectedDir?.path]);
  useEffect(() => {
    if (!advanced || adapter !== 'acp' || !selectedDir) {
      setSpawnOptions(null);
      setOptionsError('');
      return;
    }
    let cancelled = false;
    setOptionsBusy(true);
    setOptionsError('');
    void getSpawnOptions(agent, selectedDir.path).then((options) => {
      if (cancelled) return;
      setSpawnOptions(options);
      const saved = loadSpawnSettings(agent, selectedDir.path);
      const modelOption = options.configOptions.find((o) => o.category === 'model' && o.type === 'select');
      const effortOption = options.configOptions.find((o) => o.category === 'thought_level' && o.type === 'select');
      const valid = (value: string | undefined, values: { value: string }[]) => value && values.some((o) => o.value === value) ? value : '';
      setModel(valid(saved.model, modelOption?.options ?? []));
      setEffort(valid(saved.effort, effortOption?.options ?? []));
      setPermission(saved.permission && options.modes?.availableModes.some((m) => m.id === saved.permission) ? saved.permission : '');
    }).catch((error: Error) => {
      if (!cancelled) {
        setSpawnOptions(null);
        setOptionsError(error.message);
      }
    }).finally(() => { if (!cancelled) setOptionsBusy(false); });
    return () => { cancelled = true; };
  }, [advanced, agent, adapter, selectedDir?.path, getSpawnOptions]);
  useEffect(() => {
    if (taskMode) taskRef.current?.focus();
  }, [taskMode]);

  const doSpawn = async (dir: RepoInfo, forceWorktree = false) => {
    setBusy(true);
    setError(null);
    const spawnAdapter = advanced ? adapter : 'acp';
    const existing = useExisting && !forceWorktree;
    const modelOption = spawnOptions?.configOptions.find((o) => o.category === 'model' && o.type === 'select');
    const effortOption = spawnOptions?.configOptions.find((o) => o.category === 'thought_level' && o.type === 'select');
    const spec: SpawnSpec = {
      adapter: spawnAdapter,
      agent: spawnAdapter === 'acp' ? agent : undefined,
      workspace: existing
        ? { kind: 'existing', cwd: dir.path }
        : { kind: 'worktree', repo: dir.path, branch: branch || undefined, baseRef: baseRef || undefined },
      name: name || undefined,
      task: task.trim() || undefined,
      sessionConfig: spawnAdapter === 'acp' ? {
        modeId: permission || undefined,
        configOptions: {
          ...(model && modelOption ? { [modelOption.id]: model } : {}),
          ...(effort && effortOption ? { [effortOption.id]: effort } : {}),
        },
      } : undefined,
    };
    if (spawnAdapter === 'acp') {
      saveProjectAgent(dir.path, agent);
      saveSpawnSettings(agent, dir.path, { model: model || undefined, effort: effort || undefined, permission: permission || undefined });
    }
    const r = await spawn(spec);
    setBusy(false);
    if (r.error) {
      const code = r.error.split(':')[0];
      setError({ code, msg: r.error, dir });
    } else if (r.agentId) {
      recordRecentDir(dir.path);
      focus(r.agentId);
      // store.spawn already closes the modal on success
    }
  };

  const onKey = (e: React.KeyboardEvent) => {
    if (e.key === 'Escape') {
      setModal('none');
      return;
    }
    if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') {
      e.preventDefault();
      setAdvanced((a) => !a);
      return;
    }
    if (e.key === 'Tab' && !taskMode) {
      e.preventDefault();
      setTaskMode(true);
      return;
    }
    if (e.key === 'ArrowDown') {
      e.preventDefault();
      setSel((i) => Math.min(filtered.length - 1, i + 1));
      return;
    }
    if (e.key === 'ArrowUp') {
      e.preventDefault();
      setSel((i) => Math.max(0, i - 1));
      return;
    }
    if (e.key === 'Enter') {
      e.preventDefault();
      const dir = filtered[sel];
      if (dir && !busy) void doSpawn(dir);
    }
  };

  return (
    <div className="modal-scrim" onMouseDown={(e) => e.target === e.currentTarget && setModal('none')}>
      <div className="modal" onKeyDown={onKey}>
        <input
          className="q"
          autoFocus
          placeholder="Spawn in a directory…  (fuzzy; Enter = worktree + focus)"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
        />
        {taskMode && (
          <input
            ref={taskRef}
            className="task"
            placeholder="Task to dispatch on spawn (Enter to spawn + dispatch)…"
            value={task}
            onChange={(e) => setTask(e.target.value)}
          />
        )}
        <div className="rows">
          {filtered.length === 0 && <div className="empty">No git repos found under TANDEM_PROJECT_ROOTS.</div>}
          {filtered.map((d, i) => (
            <div key={d.path} className={`row${i === sel ? ' sel' : ''}`} onMouseEnter={() => !advanced && setSel(i)} onClick={() => !busy && doSpawn(d)}>
              <div>
                <div className="primary">{d.name}</div>
                <div className="sub">{d.path}</div>
              </div>
              <div className="meta">
                <span>{d.currentBranch}</span>
                {d.dirty && <span className="dirty">● dirty</span>}
                {d.hasLiveAgent && <span className="occupied">◆ occupied</span>}
              </div>
            </div>
          ))}
        </div>
        {advanced && (
          <div className="adv">
            <label>
              Adapter
              <select value={adapter} onChange={(e) => setAdapter(e.target.value as 'acp' | 'pty')}>
                <option value="acp">acp</option>
                <option value="pty">pty</option>
              </select>
            </label>
            {adapter === 'acp' && (
              <>
                <label>
                  Agent
                  <select value={agent} onChange={(e) => {
                    const next = e.target.value;
                    setAgent(next);
                    if (selectedDir) saveProjectAgent(selectedDir.path, next);
                  }}>
                    <option value="claude">claude</option>
                    <option value="codex">codex</option>
                    <option value="pi">pi</option>
                  </select>
                </label>
                <label>Model<select value={model} onChange={(e) => setModel(e.target.value)} disabled={optionsBusy || !spawnOptions}><option value="">Agent default</option>{spawnOptions?.configOptions.find((o) => o.category === 'model')?.options?.map((o) => <option key={o.value} value={o.value}>{o.name}</option>)}</select></label>
                <label>Effort<select value={effort} onChange={(e) => setEffort(e.target.value)} disabled={optionsBusy || !spawnOptions}><option value="">Agent default</option>{spawnOptions?.configOptions.find((o) => o.category === 'thought_level')?.options?.map((o) => <option key={o.value} value={o.value}>{o.name}</option>)}</select></label>
                <label>Permissions<select value={permission} onChange={(e) => setPermission(e.target.value)} disabled={optionsBusy || !spawnOptions}><option value="">Agent default</option>{spawnOptions?.modes?.availableModes.map((o) => <option key={o.id} value={o.id}>{o.name}</option>)}</select></label>
                {(optionsBusy || optionsError) && <div style={{ gridColumn: '1 / -1' }} className={optionsError ? 'modal-err' : 'sub'}>{optionsError || `Querying ${agent} ACP options…`}</div>}
              </>
            )}
            <label>
              Name
              <input value={name} onChange={(e) => setName(e.target.value)} placeholder="auto: web-1…" />
            </label>
            <label>
              Branch
              <input value={branch} onChange={(e) => setBranch(e.target.value)} placeholder="tandem/<name>" disabled={useExisting} />
            </label>
            <label>
              Base ref
              <input value={baseRef} onChange={(e) => setBaseRef(e.target.value)} placeholder="HEAD" disabled={useExisting} />
            </label>
            <label style={{ gridColumn: '1 / -1', flexDirection: 'row', alignItems: 'center', gap: 6 }}>
              <input type="checkbox" checked={useExisting} onChange={(e) => setUseExisting(e.target.checked)} />
              Reuse existing working tree (kind: existing) instead of a worktree
            </label>
          </div>
        )}
        {error && (
          <div className="modal-err">
            {error.msg}
            {error.code === 'dir_occupied' && (
              <div style={{ marginTop: 6, display: 'flex', gap: 8 }}>
                <button className="btn" onClick={() => void doSpawn(error.dir, true)}>
                  Open a worktree instead
                </button>
                {(() => {
                  const occ = Object.values(agents).find((a) => a.workspace.cwd === error.dir.path);
                  return occ ? (
                    <button className="btn" onClick={() => { focus(occ.id); setModal('none'); }}>
                      Attach to {occ.name}
                    </button>
                  ) : null;
                })()}
              </div>
            )}
          </div>
        )}
        <div className="foot">
          <span>
            <span className="kbd">↵</span> spawn
          </span>
          <span>
            <span className="kbd">⇥</span> add task
          </span>
          <span>
            <button className="btn" type="button" onClick={() => setAdvanced((a) => !a)} aria-expanded={advanced}>
              {advanced ? 'Hide advanced' : 'Advanced settings'}
            </button>
            <span className="kbd">⌘↵</span>
          </span>
          <span>
            <span className="kbd">↑↓</span> select
          </span>
          <span>
            <span className="kbd">Esc</span> close
          </span>
          {busy && <span style={{ marginLeft: 'auto' }}>spawning…</span>}
        </div>
      </div>
    </div>
  );
}
