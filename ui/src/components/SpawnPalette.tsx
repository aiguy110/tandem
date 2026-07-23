import { useEffect, useMemo, useRef, useState } from 'react';
import { useStore } from '../store';
import { fuzzyFilter } from '../fuzzy';
import type { GitRefInfo, Profile, RepoInfo, SpawnOptions, SpawnSpec } from '../wire';

const RECENT_DIRS_KEY = 'tandem.recentDirs';
const RECENT_DIRS_MAX = 3;
const SPAWN_AGENT_KEY = 'tandem.spawnAgent.v1';
const BRANCH_CONTEXT_KEY = 'tandem.branchContext.v1';
const FALLBACK_HARNESSES = [
  { id: 'agent:claude', name: 'Claude', agent: 'claude', harness: undefined as string | undefined, hasAcp: true, hasTerminal: true },
  { id: 'agent:codex', name: 'Codex', agent: 'codex', harness: undefined as string | undefined, hasAcp: true, hasTerminal: true },
  { id: 'agent:pi', name: 'Pi', agent: 'pi', harness: undefined as string | undefined, hasAcp: true, hasTerminal: true },
];

function loadProjectAgent(project: string): string | undefined {
  try {
    const all = JSON.parse(localStorage.getItem(SPAWN_AGENT_KEY) || '{}') as Record<string, string>;
    return all[project] || undefined;
  } catch { return undefined; }
}

function saveProjectAgent(project: string, agent: string) {
  try {
    const all = JSON.parse(localStorage.getItem(SPAWN_AGENT_KEY) || '{}') as Record<string, string>;
    all[project] = agent;
    localStorage.setItem(SPAWN_AGENT_KEY, JSON.stringify(all));
  } catch { /* localStorage unavailable */ }
}

interface HarnessDefaults {
  model: string;
  effort: string;
  permission: string;
}

const EMPTY_HARNESS_DEFAULTS: HarnessDefaults = { model: '', effort: '', permission: '' };

// A harness's model/effort/permission memory is no longer stored client-side:
// it is derived from the daemon-owned profiles list (globally ordered by
// lastUsedAt), so the most-recent profile matching a harness is its last-used
// settings. This keeps the profiles table the single source of truth.
function profileDefaults(profiles: Profile[], agent: string, harness: string): HarnessDefaults {
  const latest = profiles.find((p) => p.agent === agent && (p.harness ?? '') === harness);
  return latest
    ? { model: latest.model, effort: latest.effort, permission: latest.permission }
    : EMPTY_HARNESS_DEFAULTS;
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

function loadBranchContext(project: string): string | undefined {
  try {
    return (JSON.parse(localStorage.getItem(BRANCH_CONTEXT_KEY) || '{}') as Record<string, string>)[project];
  } catch { return undefined; }
}

function saveBranchContext(project: string, ref: string) {
  try {
    const all = JSON.parse(localStorage.getItem(BRANCH_CONTEXT_KEY) || '{}') as Record<string, string>;
    all[project] = ref;
    localStorage.setItem(BRANCH_CONTEXT_KEY, JSON.stringify(all));
  } catch { /* localStorage unavailable */ }
}

function proposedBranch(ref: GitRefInfo | undefined, name: string): string {
  let context = ref?.displayName ?? 'current';
  context = context.replace(/^origin\//, '').replace(/^feature\//, '').replace(/[^A-Za-z0-9._/-]+/g, '-');
  context = context.replace(/^[-./]+|[-./]+$/g, '').slice(0, 64) || 'detached';
  const agent = name ? name.replace(/[^A-Za-z0-9._-]+/g, '-').replace(/^[-.]+|[-.]+$/g, '') : '<agent-name>';
  return `tandem/${context}/${agent || 'agent'}`;
}

// Quick-spawn palette (D9): dir-first fuzzy modal backed by list_dirs.
//   Enter               → spawn worktree defaults + focus jumps
//   Tab → type task → Enter → spawn AND dispatch
//   ⌘/Ctrl+Enter        → reveal advanced (adapter / workspace mode / Git refs / name)
// Structured spawn errors (dir_occupied) offer worktree-instead / attach.
export function SpawnPalette() {
  const dirs = useStore((s) => s.dirs);
  const spawn = useStore((s) => s.spawn);
  const getSpawnOptions = useStore((s) => s.getSpawnOptions);
  const listGitRefs = useStore((s) => s.listGitRefs);
  const listProfiles = useStore((s) => s.listProfiles);
  const renameProfileAction = useStore((s) => s.renameProfile);
  const listSnapshots = useStore((s) => s.listSnapshots);
  const snapshots = useStore((s) => s.snapshots);
  const focus = useStore((s) => s.focus);
  const setModal = useStore((s) => s.setModal);
  const agents = useStore((s) => s.agents);
  const agentCatalog = useStore((s) => s.agentCatalog);
  const harnesses = useMemo(() => {
    if (!agentCatalog) return FALLBACK_HARNESSES;
    const agentsById = new Map(agentCatalog.agents.map((entry) => [entry.id, entry]));
    const configured = agentCatalog.harnesses.map((harness) => {
      const definition = agentsById.get(harness.agent);
      return {
        id: `harness:${harness.id}`,
        harness: harness.id,
        name: harness.name,
        agent: harness.agent,
        hasAcp: definition?.hasAcp ?? false,
        hasTerminal: definition?.hasTerminal ?? false,
      };
    });
    const implicit = agentCatalog.agents
      .map((entry) => ({ ...entry, id: `agent:${entry.id}`, agent: entry.id, harness: undefined as string | undefined }));
    return [...configured, ...implicit];
  }, [agentCatalog]);

  const [query, setQuery] = useState('');
  const [sel, setSel] = useState(0);
  const [taskMode, setTaskMode] = useState(false);
  const [task, setTask] = useState('');
  const [advanced, setAdvanced] = useState(false);
  const [adapter, setAdapter] = useState<'acp' | 'pty'>('acp');
  const [agent, setAgent] = useState<string>('agent:claude');
  const [terminalArgsText, setTerminalArgsText] = useState('');
  const [workspaceMode, setWorkspaceMode] = useState<'create' | 'attach' | 'existing'>('create');
  const [sourceRef, setSourceRef] = useState('');
  const [attachBranchRef, setAttachBranchRef] = useState('');
  const [agentBranch, setAgentBranch] = useState('');
  const [gitRefs, setGitRefs] = useState<GitRefInfo[]>([]);
  const [gitRefsBusy, setGitRefsBusy] = useState(false);
  const [gitRefsError, setGitRefsError] = useState('');
  const [gitRefsRefresh, setGitRefsRefresh] = useState(0);
  const [name, setName] = useState('');
  const [model, setModel] = useState('');
  const [effort, setEffort] = useState('');
  const [permission, setPermission] = useState('');
  const [snapshot, setSnapshot] = useState('');
  const [profiles, setProfiles] = useState<Profile[]>([]);
  const [profileRecent, setProfileRecent] = useState<string[]>([]);
  const [profilesByRepo, setProfilesByRepo] = useState<Record<string, { profiles: Profile[]; recent: string[] }>>({});
  const [renaming, setRenaming] = useState(false);
  const [renameText, setRenameText] = useState('');
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
  const selectedHarness = harnesses.find((harness) => harness.id === agent) ?? harnesses[0];
  const selectedGitRef = gitRefs.find((ref) => ref.ref === sourceRef);
  const selectedAttachRef = gitRefs.find((ref) => ref.ref === attachBranchRef);
  const agentSlug = selectedHarness?.agent ?? agent.replace(/^agent:/, '');
  const harnessForProject = (project: string) => {
    const saved = loadProjectAgent(project);
    const catalogDefault = agentCatalog?.defaultHarness
      ? `harness:${agentCatalog.defaultHarness}`
      : `agent:${agentCatalog?.defaultAgent ?? 'claude'}`;
    return (saved
      ? harnesses.find((harness) => harness.id === saved)
        ?? harnesses.find((harness) => !harness.harness && harness.agent === saved)
        ?? harnesses.find((harness) => harness.harness === saved)
      : undefined)
      ?? harnesses.find((harness) => harness.id === catalogDefault)
      ?? harnesses[0];
  };
  // Recall a harness's last-used model/effort/permission from the daemon-owned
  // profiles list (see profileDefaults) rather than any client-side store.
  const applyHarnessDefaults = (harnessId: string) => {
    const entry = harnesses.find((h) => h.id === harnessId);
    const defaults = profileDefaults(profiles, entry?.agent ?? harnessId.replace(/^agent:/, ''), entry?.harness ?? '');
    setModel(defaults.model);
    setEffort(defaults.effort);
    setPermission(defaults.permission);
  };
  // Resolve the harness entry a profile should launch under (its harness id, else
  // its agent).
  const harnessForProfile = (p: { agent: string; harness?: string }) =>
    (p.harness ? harnesses.find((h) => h.harness === p.harness) : undefined)
    ?? harnesses.find((h) => !h.harness && h.agent === p.agent)
    ?? harnesses.find((h) => h.agent === p.agent);
  // Apply a saved profile's settings onto the editable fields. The options-clamp
  // effect prunes any model/effort/permission the resolved harness doesn't offer.
  const applyProfile = (p: Profile) => {
    const entry = harnessForProfile(p);
    if (entry) setAgent(entry.id);
    setModel(p.model);
    setEffort(p.effort);
    setPermission(p.permission);
    setSnapshot(p.snapshotId);
    setRenaming(false);
  };
  const openAdvanced = (dir: RepoInfo, index: number) => {
    setSel(index);
    setAdvanced(true);
    appliedDirRef.current = '';
    const cached = profilesByRepo[dir.path];
    if (cached) {
      setProfiles(cached.profiles);
      setProfileRecent(cached.recent);
      const latest = cached.profiles.find((profile) => profile.id === cached.recent[0]);
      if (latest) {
        applyProfile(latest);
        appliedDirRef.current = dir.path;
      }
    }
  };
  // Seed the palette for the selected repo from the daemon's per-repo recency
  // (profile_recent): its most-recent profile is the durable default. Falls back
  // to the per-project harness hint + that harness's last settings only when the
  // repo has no recorded profile. Applied once per directory (appliedDirRef) so it
  // never clobbers edits, and re-runs as profilesByRepo loads in.
  useEffect(() => {
    if (!selectedDir) return;
    const cached = profilesByRepo[selectedDir.path];
    if (cached) {
      setProfiles(cached.profiles);
      setProfileRecent(cached.recent);
    }
    if (appliedDirRef.current === selectedDir.path) return;
    const latest = cached?.profiles.find((p) => p.id === cached.recent[0]);
    if (latest) {
      appliedDirRef.current = selectedDir.path;
      applyProfile(latest);
      return;
    }
    const matched = harnessForProject(selectedDir.path);
    setAgent(matched?.id ?? 'agent:claude');
    // Only lock in the harness-hint fallback once profiles have loaded, so a later
    // fetch that reveals a recent profile can still take precedence.
    if (cached) {
      appliedDirRef.current = selectedDir.path;
      const defaults = profileDefaults(cached.profiles, matched?.agent ?? '', matched?.harness ?? '');
      setModel(defaults.model);
      setEffort(defaults.effort);
      setPermission(defaults.permission);
    }
    // applyProfile/harnessForProject close over current harnesses; profilesByRepo drives re-runs.
  }, [selectedDir?.path, profilesByRepo, harnesses, agentCatalog]);
  useEffect(() => {
    if (adapter === 'acp' && selectedHarness && !selectedHarness.hasAcp && selectedHarness.hasTerminal) setAdapter('pty');
    if (adapter === 'pty' && selectedHarness && !selectedHarness.hasTerminal && selectedHarness.hasAcp) setAdapter('acp');
  }, [agent, adapter, selectedHarness]);
  // Load this repo's profiles + captured snapshots when the advanced panel opens,
  // and auto-apply the latest-used profile once per selected directory.
  const appliedDirRef = useRef<string>('');
  useEffect(() => {
    let cancelled = false;
    const missing = filtered.filter((dir) => !profilesByRepo[dir.path]);
    if (missing.length === 0) return;
    void Promise.all(missing.map(async (dir) => {
      try {
        const result = await listProfiles(dir.path);
        return [dir.path, result] as const;
      } catch {
        return null;
      }
    })).then((results) => {
      if (cancelled) return;
      setProfilesByRepo((current) => {
        const next = { ...current };
        for (const result of results) {
          if (result) next[result[0]] = result[1];
        }
        return next;
      });
    });
    return () => { cancelled = true; };
  }, [filtered, profilesByRepo, listProfiles]);
  // On advanced open, refresh this repo's profiles + snapshots so the picker
  // reflects any newly-created profiles. Feeding profilesByRepo lets the seed
  // effect above project the fresh list (and apply it if not yet applied).
  useEffect(() => {
    if (!advanced || !selectedDir) return;
    let cancelled = false;
    void listSnapshots().catch(() => {});
    void listProfiles(selectedDir.path).then(({ profiles: ps, recent }) => {
      if (cancelled) return;
      setProfilesByRepo((current) => ({ ...current, [selectedDir.path]: { profiles: ps, recent } }));
    }).catch(() => {});
    return () => { cancelled = true; };
  }, [advanced, selectedDir?.path, listProfiles, listSnapshots]);
  useEffect(() => {
    if (!advanced || adapter !== 'acp' || !selectedDir) {
      setSpawnOptions(null);
      setOptionsError('');
      return;
    }
    let cancelled = false;
    setOptionsBusy(true);
    setOptionsError('');
    setSpawnOptions(null);
    void getSpawnOptions(agentSlug, selectedDir.path, selectedHarness?.harness).then((options) => {
      if (cancelled) return;
      setSpawnOptions(options);
      // Clamp the current selections to what this harness actually offers; the
      // profile (or user) is the source of truth, options only prune invalids.
      const modelOption = options.configOptions.find((o) => o.category === 'model' && o.type === 'select');
      const effortOption = options.configOptions.find((o) => o.category === 'thought_level' && o.type === 'select');
      setModel((v) => (v && modelOption?.options?.some((o) => o.value === v) ? v : ''));
      setEffort((v) => (v && effortOption?.options?.some((o) => o.value === v) ? v : ''));
      setPermission((v) => (v && options.modes?.availableModes.some((m) => m.id === v) ? v : ''));
    }).catch((error: Error) => {
      if (!cancelled) {
        setSpawnOptions(null);
        setOptionsError(error.message);
      }
    }).finally(() => { if (!cancelled) setOptionsBusy(false); });
    return () => { cancelled = true; };
  }, [advanced, agent, agentSlug, adapter, selectedDir?.path, selectedHarness?.harness, getSpawnOptions]);
  useEffect(() => {
    if (!advanced || !selectedDir) {
      setGitRefs([]);
      setGitRefsError('');
      return;
    }
    let cancelled = false;
    setGitRefsBusy(true);
    setGitRefsError('');
    void listGitRefs(selectedDir.path).then((refs) => {
      if (cancelled) return;
      setGitRefs(refs);
      const remembered = loadBranchContext(selectedDir.path);
      const selected = refs.find((ref) => ref.ref === remembered)
        ?? refs.find((ref) => ref.isCurrent)
        ?? refs.find((ref) => ref.kind === 'local-branch')
        ?? refs[0];
      setSourceRef(selected?.ref ?? '');
      setAttachBranchRef(refs.find((ref) => ref.kind === 'local-branch' && ref.displayName.startsWith('tandem/'))?.ref ?? '');
    }).catch((error: Error) => {
      if (!cancelled) setGitRefsError(error.message);
    }).finally(() => { if (!cancelled) setGitRefsBusy(false); });
    return () => { cancelled = true; };
  }, [advanced, selectedDir?.path, listGitRefs, gitRefsRefresh]);
  useEffect(() => {
    if (taskMode) taskRef.current?.focus();
  }, [taskMode]);

  // The profile whose settings exactly match the current selection (if any), and
  // the auto-name a new profile would get — mirrors the daemon's naming.
  const currentHarness = selectedHarness?.harness ?? '';
  const matchedProfile = profiles.find((p) =>
    p.agent === agentSlug && (p.harness ?? '') === currentHarness &&
    p.model === model && p.effort === effort && p.permission === permission && (p.snapshotId ?? '') === snapshot);
  const snapshotLabel = snapshot ? (snapshots.find((s) => s.id === snapshot)?.name ?? 'snapshot') : 'Fresh';
  const autoName = [selectedHarness?.name ?? agentSlug, model, effort, permission, snapshotLabel]
    .filter((part) => part).join(' · ');
  // Profiles offered by default in the picker: this repo's most-recent 3.
  const recentProfiles = profileRecent.map((id) => profiles.find((p) => p.id === id)).filter((p): p is Profile => !!p).slice(0, 3);
  const commitRename = async () => {
    if (!matchedProfile || !renameText.trim()) return;
    const { profiles: ps, recent } = await renameProfileAction(matchedProfile.id, renameText.trim(), selectedDir?.path);
    setProfiles(ps);
    setProfileRecent(recent);
    setRenaming(false);
  };

  const doSpawn = async (dir: RepoInfo, forceWorktree = false, existingCwd?: string) => {
    setBusy(true);
    setError(null);
    let spawnHarness = advanced ? selectedHarness : undefined;
    let defaults: HarnessDefaults = advanced ? { model, effort, permission } : EMPTY_HARNESS_DEFAULTS;
    if (!advanced) {
      // Non-advanced quick-spawn launches this repo's durable default from
      // profile_recent (harness + settings), falling back to the per-project
      // harness hint and that harness's most-recent settings. The prefetch effect
      // usually has the list cached; fetch on a miss (row clicked before prefetch).
      const data = profilesByRepo[dir.path] ?? await listProfiles(dir.path).catch(() => ({ profiles: [], recent: [] }));
      const latest = data.profiles.find((p) => p.id === data.recent[0]);
      if (latest) {
        spawnHarness = harnessForProfile(latest);
        defaults = { model: latest.model, effort: latest.effort, permission: latest.permission };
      } else {
        spawnHarness = harnessForProject(dir.path);
        defaults = profileDefaults(data.profiles, spawnHarness?.agent ?? '', spawnHarness?.harness ?? '');
      }
    }
    const spawnAgent = spawnHarness?.agent ?? agentSlug;
    const spawnHarnessID = spawnHarness?.harness;
    const spawnAdapter = advanced ? adapter : (spawnHarness?.hasAcp ? 'acp' : 'pty');
    const mode = existingCwd ? 'existing' : forceWorktree ? 'create' : advanced ? workspaceMode : 'create';
    const cwd = existingCwd ?? dir.path;
    const workSource = advanced ? (mode === 'attach' ? selectedAttachRef : selectedGitRef) : undefined;
    let effectiveOptions = advanced ? spawnOptions : null;
    if (spawnAdapter === 'acp' && !effectiveOptions) {
      try {
        effectiveOptions = await getSpawnOptions(spawnAgent, dir.path, spawnHarnessID);
      } catch (cause) {
        setBusy(false);
        setError({ code: 'spawn_options', msg: cause instanceof Error ? cause.message : String(cause), dir });
        return;
      }
    }
    const modelOption = effectiveOptions?.configOptions.find((o) => o.category === 'model' && o.type === 'select');
    const effortOption = effectiveOptions?.configOptions.find((o) => o.category === 'thought_level' && o.type === 'select');
    const resolvedDefaults = spawnAdapter === 'acp' ? {
      model: defaults.model && modelOption?.options?.some((option) => option.value === defaults.model) ? defaults.model : '',
      effort: defaults.effort && effortOption?.options?.some((option) => option.value === defaults.effort) ? defaults.effort : '',
      permission: defaults.permission && effectiveOptions?.modes?.availableModes.some((mode) => mode.id === defaults.permission)
        ? defaults.permission : '',
    } : EMPTY_HARNESS_DEFAULTS;
    const spawnSnapshot = advanced ? snapshot : '';
    const spec: SpawnSpec = {
      adapter: spawnAdapter,
      agent: spawnAgent,
      harness: spawnHarnessID,
      terminalArgs: spawnAdapter === 'pty'
        ? (advanced ? terminalArgsText : '').split('\n').map((arg) => arg.endsWith('\r') ? arg.slice(0, -1) : arg).filter((arg) => arg.length > 0)
        : undefined,
      workspace: mode === 'existing'
        ? { kind: 'existing', cwd }
        : {
            kind: 'worktree',
            repo: dir.path,
            branchMode: mode,
            branch: mode === 'attach' ? selectedAttachRef?.displayName : agentBranch || undefined,
            source: workSource ? { ref: workSource.ref, commit: workSource.commit } : undefined,
            integration: advanced && selectedGitRef ? {
              kind: selectedGitRef.kind === 'local-branch' || selectedGitRef.kind === 'remote-branch' ? selectedGitRef.kind : 'detached',
              ref: selectedGitRef.ref,
            } : undefined,
          },
      name: advanced ? name || undefined : undefined,
      task: task.trim() || undefined,
      sessionConfig: spawnAdapter === 'acp' ? {
        modeId: resolvedDefaults.permission || undefined,
        configOptions: {
          ...(resolvedDefaults.model && modelOption ? { [modelOption.id]: resolvedDefaults.model } : {}),
          ...(resolvedDefaults.effort && effortOption ? { [effortOption.id]: resolvedDefaults.effort } : {}),
        },
      } : undefined,
      // Profile identity + browser snapshot seed. The daemon resolves-or-creates
      // the profile from these and records per-repo recency.
      profile: {
        ...(spawnAdapter === 'acp'
          ? {
              model: resolvedDefaults.model || undefined,
              effort: resolvedDefaults.effort || undefined,
              permission: resolvedDefaults.permission || undefined,
            }
          : {}),
        snapshot: spawnSnapshot || undefined,
      },
    };
    saveProjectAgent(dir.path, spawnHarness?.id ?? agent);
    if (advanced && selectedGitRef) saveBranchContext(dir.path, selectedGitRef.ref);
    const r = await spawn(spec);
    setBusy(false);
    if (r.error) {
      const code = r.error.split(':')[0];
      setError({ code, msg: r.error, dir: existingCwd ? { ...dir, path: existingCwd } : dir });
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
      const dir = filtered[sel];
      if (advanced && dir && !busy) void doSpawn(dir);
      else if (dir) openAdvanced(dir, sel);
      return;
    }
    if (advanced) return;
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
        {!advanced && (
          <>
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
              {filtered.map((d, i) => {
                const repoProfiles = profilesByRepo[d.path];
                const rowHarness = harnessForProject(d.path);
                // Quick-spawn launches this repo's most-recent profile (profile_recent).
                const defaultProfile = repoProfiles?.profiles.find((profile) => profile.id === repoProfiles.recent[0]);
                return (
                  <div key={d.path} className={`row${i === sel ? ' sel' : ''}`} onMouseEnter={() => setSel(i)} onClick={() => !busy && doSpawn(d)}>
                    <div className="repo-details">
                      <div className="primary">{d.name}</div>
                      <div className="sub">{d.path}</div>
                    </div>
                    <div className="meta">
                      <span>{d.currentBranch}</span>
                      {d.dirty && <span className="dirty">● dirty</span>}
                      {d.hasLiveAgent && <span className="occupied">◆ occupied</span>}
                    </div>
                    <button
                      className="btn profile-button"
                      type="button"
                      title="Open advanced launch settings"
                      disabled={busy}
                      onClick={(event) => {
                        event.stopPropagation();
                        openAdvanced(d, i);
                      }}
                    >
                      {defaultProfile?.name ?? `${rowHarness?.name ?? 'Agent'} defaults`}
                    </button>
                  </div>
                );
              })}
            </div>
          </>
        )}
        {advanced && (
          <>
            <div className="advanced-repo">
              <div>
                <div className="primary">{selectedDir?.name}</div>
                <div className="sub">{selectedDir?.path}</div>
              </div>
              <span>{selectedDir?.currentBranch}</span>
            </div>
            <div className="adv">
            <div className="adv-section" style={{ gridColumn: '1 / -1' }}>Agent profile</div>
            <label style={{ gridColumn: '1 / -1' }}>
              Profile <span className="sub">(latest 3 shown; fuzzy-find for more)</span>
              <ProfilePicker profiles={profiles} recent={recentProfiles} value={matchedProfile} onPick={applyProfile} />
              <div className="sub" style={{ marginTop: 4 }}>
                {matchedProfile ? (
                  renaming ? (
                    <span style={{ display: 'inline-flex', gap: 6, alignItems: 'center' }}>
                      <input value={renameText} autoFocus onChange={(e) => setRenameText(e.target.value)}
                        onKeyDown={(e) => { e.stopPropagation(); if (e.key === 'Enter') void commitRename(); if (e.key === 'Escape') setRenaming(false); }} />
                      <button type="button" className="btn" onClick={() => void commitRename()}>Save</button>
                      <button type="button" className="btn ghost" onClick={() => setRenaming(false)}>Cancel</button>
                    </span>
                  ) : (
                    <>Using <b>{matchedProfile.name}</b>{' '}
                      <button type="button" className="btn ghost" onClick={() => { setRenaming(true); setRenameText(matchedProfile.name); }}>Rename</button>
                    </>
                  )
                ) : (
                  <>New profile will be created: <b>{autoName}</b></>
                )}
              </div>
            </label>
            <label>
              Connection
              <select value={adapter} onChange={(e) => setAdapter(e.target.value as 'acp' | 'pty')}>
                <option value="acp" disabled={!selectedHarness?.hasAcp}>Transcript (ACP)</option>
                <option value="pty" disabled={!selectedHarness?.hasTerminal}>Direct Terminal</option>
              </select>
            </label>
            <label>
              Harness
              <select value={selectedHarness?.id ?? ''} onChange={(e) => {
                const next = e.target.value;
                setAgent(next);
                applyHarnessDefaults(next);
                if (selectedDir) saveProjectAgent(selectedDir.path, next);
              }}>
                {harnesses.map((harness) => <option key={harness.id} value={harness.id}>{harness.name}</option>)}
              </select>
            </label>
            {adapter === 'acp' && (
              <>
                <label>Model<select className="model-select" value={model} onChange={(e) => setModel(e.target.value)} disabled={optionsBusy || !spawnOptions}><option value="">Agent default</option>{spawnOptions?.configOptions.find((o) => o.category === 'model')?.options?.map((o) => <option key={o.value} value={o.value} title={o.name}>{o.name}</option>)}</select></label>
                <label>Effort<select value={effort} onChange={(e) => setEffort(e.target.value)} disabled={optionsBusy || !spawnOptions}><option value="">Agent default</option>{spawnOptions?.configOptions.find((o) => o.category === 'thought_level')?.options?.map((o) => <option key={o.value} value={o.value}>{o.name}</option>)}</select></label>
                <label>Permissions<select value={permission} onChange={(e) => setPermission(e.target.value)} disabled={optionsBusy || !spawnOptions}><option value="">Agent default</option>{spawnOptions?.modes?.availableModes.map((o) => <option key={o.id} value={o.id}>{o.name}</option>)}</select></label>
                {(optionsBusy || optionsError) && <div style={{ gridColumn: '1 / -1' }} className={optionsError ? 'modal-err' : 'sub'}>{optionsError || `Querying ${agent} ACP options…`}</div>}
              </>
            )}
            {adapter === 'pty' && (
              <label style={{ gridColumn: '1 / -1' }}>
                Additional terminal arguments (one argument per line)
                <textarea
                  value={terminalArgsText}
                  onChange={(e) => setTerminalArgsText(e.target.value)}
                  onKeyDown={(e) => e.stopPropagation()}
                  placeholder={'--model\nopenai/gpt-5'}
                  spellCheck={false}
                />
              </label>
            )}
            <label>
              Browser snapshot <span className="sub">(seed state)</span>
              <select value={snapshot} onChange={(e) => setSnapshot(e.target.value)}>
                <option value="">Fresh state</option>
                {snapshots.map((s) => <option key={s.id} value={s.id}>{s.name}</option>)}
              </select>
            </label>
            <div className="adv-section" style={{ gridColumn: '1 / -1' }}>Repo settings</div>
            <label>
              Name
              <input value={name} onChange={(e) => setName(e.target.value)} placeholder="auto: einstein-1…" />
            </label>
            <label>
              Git workspace
              <select value={workspaceMode} onChange={(e) => {
                const mode = e.target.value as 'create' | 'attach' | 'existing';
                setWorkspaceMode(mode);
                if (mode === 'attach') {
                  const local = gitRefs.find((ref) => ref.ref === attachBranchRef && ref.kind === 'local-branch')
                    ?? gitRefs.find((ref) => ref.kind === 'local-branch' && ref.displayName.startsWith('tandem/'));
                  setAttachBranchRef(local?.ref ?? '');
                }
              }}>
                <option value="create">New agent branch</option>
                <option value="attach">Continue existing branch</option>
                <option value="existing">Use existing checkout</option>
              </select>
            </label>
            {workspaceMode === 'attach' && (
              <label style={{ gridColumn: '1 / -1' }}>
                Existing agent branch
                <GitRefPicker
                  refs={gitRefs.filter((ref) => ref.kind === 'local-branch')}
                  value={attachBranchRef}
                  onChange={(refName) => {
                    setAttachBranchRef(refName);
                    const historical = gitRefs.find((ref) => ref.ref === refName)?.tandem;
                    if (historical?.integrationRef && gitRefs.some((ref) => ref.ref === historical.integrationRef)) setSourceRef(historical.integrationRef);
                  }}
                  busy={gitRefsBusy}
                />
              </label>
            )}
            {workspaceMode !== 'existing' && (
              <label style={{ gridColumn: '1 / -1' }}>
                {workspaceMode === 'attach' ? 'Merge target' : 'Start from / merge target'}
                <GitRefPicker refs={gitRefs} value={sourceRef} onChange={setSourceRef} busy={gitRefsBusy} />
              </label>
            )}
            {workspaceMode === 'create' && (
              <label style={{ gridColumn: '1 / -1' }}>
                Agent branch <span className="sub">(optional override)</span>
                <input value={agentBranch} onChange={(e) => setAgentBranch(e.target.value)} placeholder={proposedBranch(selectedGitRef, name)} />
              </label>
            )}
            {workspaceMode === 'existing' && (
              <div className="git-context-preview" style={{ gridColumn: '1 / -1' }}>
                The agent will use <code>{selectedDir?.path}</code> directly. No isolated branch is created.
              </div>
            )}
            {workspaceMode !== 'existing' && selectedGitRef && (
              <div className="git-context-preview" style={{ gridColumn: '1 / -1' }}>
                <div>
                  {workspaceMode === 'create' ? <>Starting at <b>{selectedGitRef.displayName}</b> @ <code>{selectedGitRef.commit.slice(0, 8)}</code></> : <>Attaching <b>{selectedAttachRef?.displayName ?? '—'}</b>; merge target <b>{selectedGitRef.displayName}</b></>}
                </div>
                {workspaceMode === 'create' && <div>Agent branch <code>{agentBranch || proposedBranch(selectedGitRef, name)}</code></div>}
                {selectedAttachRef?.checkedOutAt && workspaceMode === 'attach' && (
                  <div className="modal-err">
                    <div>This branch is already checked out at {selectedAttachRef.checkedOutAt}.</div>
                    <button
                      type="button"
                      className="btn"
                      style={{ marginTop: 8 }}
                      disabled={busy}
                      onClick={() => void doSpawn(selectedDir, false, selectedAttachRef.checkedOutAt)}
                    >
                      Use this checked-out worktree
                    </button>
                  </div>
                )}
                {selectedGitRef.checkedOutAt === selectedDir?.path && selectedDir?.dirty && workspaceMode === 'create' && (
                  <div className="branch-warning">Uncommitted changes in the existing checkout are not included; the agent starts from the committed revision above.</div>
                )}
              </div>
            )}
            {gitRefsError && workspaceMode !== 'existing' && <div className="modal-err" style={{ gridColumn: '1 / -1' }}>{gitRefsError}</div>}
            {workspaceMode !== 'existing' && (
              <div style={{ gridColumn: '1 / -1', display: 'flex', justifyContent: 'flex-end' }}>
                <button type="button" className="btn ghost" onClick={() => setGitRefsRefresh((value) => value + 1)} disabled={gitRefsBusy}>Refresh local refs</button>
              </div>
            )}
            </div>
          </>
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
        {advanced ? (
          <div className="foot advanced-actions">
            <button className="btn ghost" type="button" onClick={() => setAdvanced(false)}>Choose other repo</button>
            <span className="action-spacer" />
            <button className="btn ghost" type="button" onClick={() => setModal('none')}>Cancel</button>
            <button className="btn" type="button" disabled={busy || optionsBusy || !selectedDir} onClick={() => selectedDir && void doSpawn(selectedDir)}>
              {busy ? 'Launching…' : 'Launch'}
            </button>
          </div>
        ) : (
          <div className="foot">
            <span><span className="kbd">↵</span> spawn</span>
            <span><span className="kbd">⇥</span> add task</span>
            <span><span className="kbd">↑↓</span> select</span>
            <span><span className="kbd">Esc</span> close</span>
            {busy && <span style={{ marginLeft: 'auto' }}>spawning…</span>}
          </div>
        )}
      </div>
    </div>
  );
}

// Fuzzy profile picker: shows this repo's latest-used profiles by default, and
// fuzzy-searches all profiles by name while typing. Picking one applies its
// settings to the spawn form.
function ProfilePicker({
  profiles,
  recent,
  value,
  onPick,
}: {
  profiles: Profile[];
  recent: Profile[];
  value: Profile | undefined;
  onPick: (p: Profile) => void;
}) {
  const [query, setQuery] = useState('');
  const [open, setOpen] = useState(false);
  const [index, setIndex] = useState(0);
  const closeTimer = useRef<number>();
  const results = useMemo(() => {
    if (!query.trim()) return recent;
    return fuzzyFilter(query, profiles, (p) => p.name).slice(0, 8);
  }, [query, profiles, recent]);
  useEffect(() => setIndex(0), [query]);
  const choose = (p: Profile) => {
    onPick(p);
    setQuery('');
    setOpen(false);
  };
  return (
    <div className="git-ref-picker">
      <input
        value={query}
        placeholder={value ? value.name : 'Fresh — pick a saved profile…'}
        onFocus={() => setOpen(true)}
        onBlur={() => { closeTimer.current = window.setTimeout(() => setOpen(false), 120); }}
        onChange={(e) => { setQuery(e.target.value); setOpen(true); }}
        onKeyDown={(e) => {
          e.stopPropagation();
          if (e.key === 'ArrowDown') { e.preventDefault(); setOpen(true); setIndex((i) => Math.min(results.length - 1, i + 1)); }
          else if (e.key === 'ArrowUp') { e.preventDefault(); setIndex((i) => Math.max(0, i - 1)); }
          else if (e.key === 'Enter' && open && results[index]) { e.preventDefault(); choose(results[index]); }
          else if (e.key === 'Escape' && open) { e.preventDefault(); setOpen(false); setQuery(''); }
        }}
      />
      {open && (
        <div className="git-ref-results">
          {results.length === 0 && <div className="git-ref-empty">{profiles.length === 0 ? 'No saved profiles yet.' : 'No matching profiles.'}</div>}
          {results.map((p, row) => (
            <button
              type="button"
              key={p.id}
              className={`git-ref-row${row === index ? ' selected' : ''}`}
              onMouseDown={(e) => e.preventDefault()}
              onMouseEnter={() => setIndex(row)}
              onClick={() => { if (closeTimer.current) window.clearTimeout(closeTimer.current); choose(p); }}
            >
              <span className="git-ref-main">
                <strong>{p.name}</strong>
              </span>
              <span className="git-ref-meta">
                {value?.id === p.id && <em>current</em>}
                {!query.trim() && <span>recent</span>}
              </span>
            </button>
          ))}
        </div>
      )}
    </div>
  );
}

function GitRefPicker({
  refs,
  value,
  onChange,
  busy,
}: {
  refs: GitRefInfo[];
  value: string;
  onChange: (ref: string) => void;
  busy: boolean;
}) {
  const selected = refs.find((ref) => ref.ref === value);
  const [query, setQuery] = useState(selected?.displayName ?? '');
  const [open, setOpen] = useState(false);
  const [index, setIndex] = useState(0);
  const closeTimer = useRef<number>();
  useEffect(() => setQuery(selected?.displayName ?? ''), [selected?.displayName]);
  const filtered = useMemo(() => {
    const items = fuzzyFilter(query === selected?.displayName ? '' : query, refs, (ref) => `${ref.displayName} ${ref.kind} ${ref.subject ?? ''}`);
    return items.slice(0, 10);
  }, [query, refs, selected?.displayName]);
  useEffect(() => setIndex(0), [query, refs]);

  const choose = (ref: GitRefInfo) => {
    onChange(ref.ref);
    setQuery(ref.displayName);
    setOpen(false);
  };
  return (
    <div className="git-ref-picker">
      <input
        value={query}
        placeholder={busy ? 'Loading branches…' : 'Fuzzy-find a branch, remote, or tag…'}
        disabled={busy}
        onFocus={() => setOpen(true)}
        onBlur={() => { closeTimer.current = window.setTimeout(() => setOpen(false), 120); }}
        onChange={(event) => { setQuery(event.target.value); setOpen(true); }}
        onKeyDown={(event) => {
          event.stopPropagation();
          if (event.key === 'ArrowDown') {
            event.preventDefault();
            setOpen(true);
            setIndex((current) => Math.min(filtered.length - 1, current + 1));
          } else if (event.key === 'ArrowUp') {
            event.preventDefault();
            setIndex((current) => Math.max(0, current - 1));
          } else if (event.key === 'Enter' && open && filtered[index]) {
            event.preventDefault();
            choose(filtered[index]);
          } else if (event.key === 'Escape' && open) {
            event.preventDefault();
            setOpen(false);
            setQuery(selected?.displayName ?? '');
          }
        }}
      />
      {open && !busy && (
        <div className="git-ref-results">
          {filtered.length === 0 && <div className="git-ref-empty">No matching Git refs.</div>}
          {filtered.map((ref, row) => (
            <button
              type="button"
              key={ref.ref}
              className={`git-ref-row${row === index ? ' selected' : ''}`}
              onMouseDown={(event) => event.preventDefault()}
              onMouseEnter={() => setIndex(row)}
              onClick={() => {
                if (closeTimer.current) window.clearTimeout(closeTimer.current);
                choose(ref);
              }}
            >
              <span className="git-ref-main">
                <strong>{ref.displayName}</strong>
                <small>{ref.subject}</small>
              </span>
              <span className="git-ref-meta">
                {ref.isCurrent && <em>current</em>}
                {ref.checkedOutAt && <em>checked out</em>}
                {ref.tandem && <em>{ref.tandem.live ? 'live Tandem agent' : 'Tandem branch'}</em>}
                <span>{ref.kind.replace('-branch', '')}</span>
                {(ref.ahead || ref.behind) ? <span>+{ref.ahead ?? 0}/-{ref.behind ?? 0}</span> : null}
              </span>
            </button>
          ))}
        </div>
      )}
    </div>
  );
}
