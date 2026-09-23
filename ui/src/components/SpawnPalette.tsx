import { useEffect, useMemo, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import { LOCAL_HOST_ID, useStore } from '../store';
import { fuzzyFilter, fuzzyFilterFields } from '../fuzzy';
import type { BrowserSnapshot, GitRefInfo, Profile, RepoInfo, SpawnOptions, SpawnSpec } from '../wire';
import { usePresence, useValuePresence } from '../transitions';

const RECENT_DIRS_KEY = 'tandem.recentDirs';
const RECENT_DIRS_MAX = 3;
// Profiles listed under each repo, most recently used first.
const PROFILES_PER_REPO = 4;
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
  const agent = name ? name.replace(/[^A-Za-z0-9._-]+/g, '-').replace(/^[-.]+|[-.]+$/g, '') : '<session-name>';
  return `tandem/${context}/${agent || 'session'}`;
}

// A palette row is either a repo header or one of the profiles previously used
// in that repo (daemon-owned per-repo recency, newest first).
type PaletteRow =
  | { kind: 'repo'; dir: RepoInfo }
  | { kind: 'profile'; dir: RepoInfo; profile: Profile };

// Quick-spawn palette (D9): dir-first fuzzy modal backed by list_dirs. Each
// matching repo is a header followed by the profiles already used in it.
//   Enter on a profile  → spawn with that profile + focus jumps
//   Enter on a repo     → open the form to launch with a new profile
//   Tab → type task → Enter → spawn AND dispatch
//   ⌘/Ctrl+Enter        → customize the selected profile (or new profile on a repo)
//   right-click / long-press a profile → Customize / Forget
export function SpawnPalette() {
  const dirs = useStore((s) => s.dirs);
  const hosts = useStore((s) => s.hosts);
  const dirsByHost = useStore((s) => s.dirsByHost);
  const agentCatalogByHost = useStore((s) => s.agentCatalogByHost);
  const refreshHostDirs = useStore((s) => s.refreshHostDirs);
  const refreshAgentCatalog = useStore((s) => s.refreshAgentCatalog);
  const spawn = useStore((s) => s.spawn);
  const getSpawnOptions = useStore((s) => s.getSpawnOptions);
  const listGitRefs = useStore((s) => s.listGitRefs);
  const listProfiles = useStore((s) => s.listProfiles);
  const forgetProfile = useStore((s) => s.forgetProfile);
  const listSnapshots = useStore((s) => s.listSnapshots);
  const localSnapshots = useStore((s) => s.snapshots);
  // The target host's own snapshots: a profile's snapshot id only means
  // something on the daemon that captured it.
  const [remoteSnapshots, setRemoteSnapshots] = useState<BrowserSnapshot[]>([]);
  const setModal = useStore((s) => s.setModal);
  const agents = useStore((s) => s.sessions);
  const spawnHandoffFrom = useStore((s) => s.spawnHandoffFrom);
  const spawnHostId = useStore((s) => s.spawnHostId);
  const agentCatalog = useStore((s) => s.agentCatalog);
  const [hostId, setHostId] = useState(() => spawnHostId ?? LOCAL_HOST_ID);
  const selectedHost = hosts.find((host) => host.id === hostId) ?? hosts[0];
  const remote = !!selectedHost && !selectedHost.local && selectedHost.id !== LOCAL_HOST_ID;
  const scopedDirs = remote ? (dirsByHost[hostId] ?? []) : dirs;
  const snapshots = remote ? remoteSnapshots : localSnapshots;
  const scopedCatalog = remote ? (agentCatalogByHost[hostId] ?? null) : agentCatalog;
  const harnesses = useMemo(() => {
    if (!scopedCatalog) return FALLBACK_HARNESSES;
    const agentsById = new Map(scopedCatalog.agents.map((entry) => [entry.id, entry]));
    const configured = scopedCatalog.harnesses.map((harness) => {
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
    const implicit = scopedCatalog.agents
      .map((entry) => ({ ...entry, id: `agent:${entry.id}`, agent: entry.id, harness: undefined as string | undefined }));
    return [...configured, ...implicit];
  }, [scopedCatalog]);

  const [query, setQuery] = useState('');
  // null = not moved by the user since the query changed; the first profile row
  // (the repo's latest profile) is then selected so Enter quick-spawns it.
  const [sel, setSel] = useState<number | null>(null);
  const [taskMode, setTaskMode] = useState(false);
  const [task, setTask] = useState('');
  const [advanced, setAdvanced] = useState(false);
  const [adapter, setAdapter] = useState<'acp' | 'pty'>('acp');
  const [agent, setAgent] = useState<string>('agent:claude');
  const [terminalArgsText, setTerminalArgsText] = useState('');
  const [workspaceMode, setWorkspaceMode] = useState<'create' | 'attach' | 'existing' | 'join'>('create');
  // Agent whose transcript seeds the new agent's first message ('' = none, the
  // default). Pre-set when the rail's "Hand off" action opened this palette.
  const [parentSession, setParentSession] = useState(spawnHandoffFrom ?? '');
  // The two things a parent session offers — its transcript and its worktree —
  // are independent; either can be taken without the other.
  const [includeTranscript, setIncludeTranscript] = useState(true);
  const [handoffMode, setHandoffMode] = useState<'full' | 'brief'>('full');
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
  const [profilesByRepo, setProfilesByRepo] = useState<Record<string, { profiles: Profile[]; recent: string[] }>>({});
  // The repo the advanced form launches into, pinned when the form opens so
  // profile-list refreshes cannot shift it to another row.
  const [chosenDir, setChosenDir] = useState<RepoInfo | null>(null);
  // Profile being customized. Launching with its settings unchanged (and no new
  // name) reuses it; any change yields a new profile, never an edit of this one.
  const [baseProfile, setBaseProfile] = useState<Profile | undefined>();
  const [profileName, setProfileName] = useState('');
  const [menu, setMenu] = useState<{ x: number; y: number; dir: RepoInfo; profile: Profile } | null>(null);
  const { rendered: renderedMenu, closing: menuClosing } = useValuePresence(menu);
  const rowsRef = useRef<HTMLDivElement>(null);
  const [spawnOptions, setSpawnOptions] = useState<SpawnOptions | null>(null);
  const [optionsBusy, setOptionsBusy] = useState(false);
  const [optionsError, setOptionsError] = useState('');
  const [error, setError] = useState<{ code: string; msg: string; dir: RepoInfo } | null>(null);
  const [busy, setBusy] = useState(false);

  const taskRef = useRef<HTMLInputElement>(null);
  const launchRef = useRef<HTMLButtonElement>(null);

  // A rail-initiated hand-off should be created where its source actually
  // lives. Remote summaries carry a master-qualified id plus this hostId;
  // choosing local here makes both the checkout and transcript unavailable.
  useEffect(() => {
    if (!spawnHandoffFrom) return;
    const source = agents[spawnHandoffFrom];
    if (source) setHostId(source.hostId ?? LOCAL_HOST_ID);
  }, [spawnHandoffFrom, agents]);

  useEffect(() => {
    if (spawnHostId) setHostId(spawnHostId);
  }, [spawnHostId]);

  // Host discovery is intentionally lazy: opening the palette continues to
  // work against an older daemon, while choosing a connected slave asks the
  // master for that host's own repositories and launch catalog.
  useEffect(() => {
    refreshHostDirs(hostId);
    refreshAgentCatalog(hostId);
    setQuery('');
    setSel(null);
    setAdvanced(false);
    setChosenDir(null);
    // Profiles and browser snapshots belong to the host that recorded them, so
    // switching hosts drops the previous host's lists.
    setProfilesByRepo({});
    setRemoteSnapshots([]);
    setBaseProfile(undefined);
    setSnapshot('');
    setAgent('agent:claude');
    setModel('');
    setEffort('');
    setPermission('');
  }, [hostId, refreshHostDirs, refreshAgentCatalog]);

  const filtered = useMemo(() => {
    // Name first: the path's shared ~/Projects prefix otherwise matches every
    // repo just as well as a repo's own name does.
    const matched = fuzzyFilterFields(query, scopedDirs, (d) => [[d.name, 2], [d.path, 1]]);
    if (query) return matched;
    const recentPaths = loadRecentDirs();
    const byPath = new Map(scopedDirs.map((d) => [d.path, d]));
    const recent = recentPaths.map((p) => byPath.get(p)).filter((d): d is RepoInfo => !!d);
    return recent.length > 0 ? recent.slice(0, RECENT_DIRS_MAX) : matched.slice(0, RECENT_DIRS_MAX);
  }, [query, scopedDirs, hostId]);
  const rows = useMemo(() => filtered.flatMap((dir): PaletteRow[] => {
    const data = profilesByRepo[dir.path];
    const used = data
      ? data.recent.map((id) => data.profiles.find((p) => p.id === id)).filter((p): p is Profile => !!p).slice(0, PROFILES_PER_REPO)
      : [];
    return [{ kind: 'repo', dir }, ...used.map((profile): PaletteRow => ({ kind: 'profile', dir, profile }))];
  }), [filtered, profilesByRepo]);
  useEffect(() => setSel(null), [query]);
  const selIndex = Math.min(sel ?? (rows[1]?.kind === 'profile' ? 1 : 0), Math.max(0, rows.length - 1));
  const selectedRow = rows[selIndex];
  const selectedDir = advanced && chosenDir ? chosenDir : selectedRow?.dir;
  useEffect(() => {
    rowsRef.current?.querySelector('.row.sel')?.scrollIntoView({ block: 'nearest' });
  }, [selIndex]);
  // Profiles are global (the picker searches them all); any repo's cached list
  // is the same set, so prefer the selected repo's freshest copy.
  const profiles = (selectedDir && profilesByRepo[selectedDir.path]?.profiles) || Object.values(profilesByRepo)[0]?.profiles || [];
  const selectedHarness = harnesses.find((harness) => harness.id === agent) ?? harnesses[0];
  const selectedGitRef = gitRefs.find((ref) => ref.ref === sourceRef);
  const selectedAttachRef = gitRefs.find((ref) => ref.ref === attachBranchRef);
  const agentSlug = selectedHarness?.agent ?? agent.replace(/^agent:/, '');
  const parentAgent = parentSession ? agents[parentSession] : undefined;
  // Sessions offered as a hand-off source, newest names last is unhelpful here,
  // so order them the way the rail does: alphabetically by display name.
  const parentCandidates = useMemo(
    () => Object.values(agents)
      .filter((candidate) => (candidate.hostId ?? LOCAL_HOST_ID) === hostId)
      .sort((a, b) => a.name.localeCompare(b.name)),
    [agents, hostId],
  );
  const joinCwd = parentAgent?.workspace.cwd ?? '';
  // Agents already working in whichever directory this spawn would land in.
  // Sharing is allowed (that is the point of a hand-off), but it is never
  // implicit, so the form says who else is there.
  const targetCwd = workspaceMode === 'join' ? joinCwd : workspaceMode === 'existing' ? (selectedDir?.path ?? '') : '';
  // "existing" and "join" both adopt a checkout as-is, so the ref pickers (start
  // point, merge target, agent branch) only apply to the two provisioning modes.
  const usesGitRefs = workspaceMode === 'create' || workspaceMode === 'attach';
  const cohabitants = targetCwd
    ? Object.values(agents).filter((a) => a.workspace.cwd === targetCwd).map((a) => a.name).sort()
    : [];
  // The rail's "Hand off" action opens this palette with a source already
  // chosen. A hand-off is never a quick-spawn — it needs the source session and
  // usually its worktree — so jump straight to the advanced form on that repo.
  const handoffSeeded = useRef(false);
  useEffect(() => {
    if (handoffSeeded.current || !spawnHandoffFrom) return;
    const source = agents[spawnHandoffFrom];
    if (!source || (source.hostId ?? LOCAL_HOST_ID) !== hostId || scopedDirs.length === 0) return;
    handoffSeeded.current = true;
    setParentSession(spawnHandoffFrom);
    const dir = scopedDirs.find((d) => d.path === source.workspace.repoPath);
    if (dir) {
      setQuery(dir.path);
      openForm(dir);
    }
    setAdvanced(true);
    if (source.workspace.cwd) setWorkspaceMode('join');
  }, [spawnHandoffFrom, agents, hostId, scopedDirs]);
  // "Continue in the source's worktree" stops meaning anything once the source
  // is cleared; fall back to the ordinary isolated worktree.
  useEffect(() => {
    if (workspaceMode === 'join' && !joinCwd) setWorkspaceMode('create');
  }, [workspaceMode, joinCwd]);

  const harnessForProject = (project: string) => {
    const saved = loadProjectAgent(project);
    const catalogDefault = scopedCatalog?.defaultHarness
      ? `harness:${scopedCatalog.defaultHarness}`
      : `agent:${scopedCatalog?.defaultAgent ?? 'claude'}`;
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
  };
  // Open the launch form for a repo. With `base` it customizes that profile;
  // without, it starts a new profile seeded from the repo's latest one (or the
  // per-project harness hint + that harness's last settings when it has none).
  const openForm = (dir: RepoInfo, base?: Profile) => {
    setChosenDir(dir);
    setAdvanced(true);
    setMenu(null);
    setProfileName('');
    setBaseProfile(base);
    const data = profilesByRepo[dir.path];
    const seed = base ?? data?.profiles.find((p) => p.id === data.recent[0]);
    if (seed) {
      applyProfile(seed);
      return;
    }
    const matched = harnessForProject(dir.path);
    const defaults = profileDefaults(data?.profiles ?? [], matched?.agent ?? '', matched?.harness ?? '');
    setAgent(matched?.id ?? 'agent:claude');
    setModel(defaults.model);
    setEffort(defaults.effort);
    setPermission(defaults.permission);
    setSnapshot('');
  };
  const forget = async (dir: RepoInfo, profile: Profile) => {
    setMenu(null);
    try {
      const result = await forgetProfile(profile.id, dir.path, hostId);
      setProfilesByRepo((current) => ({ ...current, [dir.path]: result }));
    } catch (cause) {
      setError({ code: 'forget_profile', msg: cause instanceof Error ? cause.message : String(cause), dir });
    }
  };
  const openMenu = (origin: { x: number; y: number }, dir: RepoInfo, profile: Profile) => {
    const width = 180;
    const height = 90;
    setMenu({
      x: Math.max(8, Math.min(origin.x, window.innerWidth - width - 8)),
      y: Math.max(8, Math.min(origin.y, window.innerHeight - height - 8)),
      dir,
      profile,
    });
  };
  useEffect(() => {
    if (!menu) return;
    const dismiss = () => setMenu(null);
    window.addEventListener('pointerdown', dismiss);
    return () => window.removeEventListener('pointerdown', dismiss);
  }, [menu]);
  useEffect(() => {
    if (adapter === 'acp' && selectedHarness && !selectedHarness.hasAcp && selectedHarness.hasTerminal) setAdapter('pty');
    if (adapter === 'pty' && selectedHarness && !selectedHarness.hasTerminal && selectedHarness.hasAcp) setAdapter('acp');
  }, [agent, adapter, selectedHarness]);
  // Prefetch the used-profile lists for every visible repo.
  useEffect(() => {
    let cancelled = false;
    const missing = filtered.filter((dir) => !profilesByRepo[dir.path]);
    if (missing.length === 0) return;
    void Promise.all(missing.map(async (dir) => {
      try {
        const result = await listProfiles(dir.path, hostId);
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
  }, [filtered, profilesByRepo, listProfiles, hostId]);
  // On advanced open, refresh this repo's profiles + snapshots so the picker
  // reflects any newly-created profiles.
  useEffect(() => {
    if (!advanced || !selectedDir) return;
    let cancelled = false;
    void listSnapshots(hostId).then((list) => {
      if (!cancelled && remote) setRemoteSnapshots(list);
    }).catch(() => {});
    void listProfiles(selectedDir.path, hostId).then(({ profiles: ps, recent }) => {
      if (cancelled) return;
      setProfilesByRepo((current) => ({ ...current, [selectedDir.path]: { profiles: ps, recent } }));
    }).catch(() => {});
    return () => { cancelled = true; };
  }, [advanced, selectedDir?.path, listProfiles, listSnapshots, remote, hostId]);
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
    void getSpawnOptions(agentSlug, selectedDir.path, selectedHarness?.harness, hostId).then((options) => {
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
  }, [advanced, agent, agentSlug, adapter, selectedDir?.path, selectedHarness?.harness, getSpawnOptions, hostId]);
  useEffect(() => {
    if (!advanced || !selectedDir) {
      setGitRefs([]);
      setGitRefsError('');
      return;
    }
    let cancelled = false;
    setGitRefsBusy(true);
    setGitRefsError('');
    void listGitRefs(selectedDir.path, hostId).then((refs) => {
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
  }, [advanced, selectedDir?.path, listGitRefs, gitRefsRefresh, hostId]);
  useEffect(() => {
    if (taskMode) taskRef.current?.focus();
  }, [taskMode]);

  // The profile the daemon will record for the current form (mirroring its
  // resolution rules), and the auto-name a new profile would get.
  const currentHarness = selectedHarness?.harness ?? '';
  const tuple = adapter === 'acp' ? { model, effort, permission } : EMPTY_HARNESS_DEFAULTS;
  const matchesForm = (p: Profile) =>
    p.agent === agentSlug && (p.harness ?? '') === currentHarness &&
    p.model === tuple.model && p.effort === tuple.effort && p.permission === tuple.permission && (p.snapshotId ?? '') === snapshot;
  const trimmedProfileName = profileName.trim();
  const baseUnchanged = !!baseProfile && !trimmedProfileName && matchesForm(baseProfile);
  const resolvedProfile = trimmedProfileName
    ? profiles.find((p) => !p.autoNamed && p.name === trimmedProfileName && matchesForm(p))
    : baseUnchanged ? baseProfile : profiles.find((p) => p.autoNamed && matchesForm(p));
  const snapshotLabel = snapshot ? (snapshots.find((s) => s.id === snapshot)?.name ?? 'snapshot') : 'Fresh';
  const autoName = [selectedHarness?.name ?? agentSlug, tuple.model, tuple.effort, tuple.permission, snapshotLabel]
    .filter((part) => part).join(' · ');
  // Profiles offered by default in the picker: this repo's most-recent 3.
  const repoRecent = (selectedDir && profilesByRepo[selectedDir.path]?.recent) || [];
  const recentProfiles = repoRecent.map((id) => profiles.find((p) => p.id === id)).filter((p): p is Profile => !!p).slice(0, 3);

  // Spawn in `dir`. `quick` launches that saved profile as-is (a profile row);
  // otherwise the advanced form's current settings are used.
  const doSpawn = async (dir: RepoInfo, { quick, existingCwd }: { quick?: Profile; existingCwd?: string } = {}) => {
    setBusy(true);
    setError(null);
    const spawnHarness = quick ? harnessForProfile(quick) : selectedHarness;
    const defaults: HarnessDefaults = quick
      ? { model: quick.model, effort: quick.effort, permission: quick.permission }
      : { model, effort, permission };
    const spawnAgent = spawnHarness?.agent ?? quick?.agent ?? agentSlug;
    const spawnHarnessID = spawnHarness?.harness ?? (quick ? quick.harness || undefined : undefined);
    const spawnAdapter = quick ? (spawnHarness?.hasAcp === false ? 'pty' : 'acp') : adapter;
    const mode = existingCwd ? 'existing' : quick ? 'create' : workspaceMode;
    // "join" resolves to the source agent's checkout: the daemon recognizes an
    // already-occupied directory and lets the new agent share that workspace
    // (branch, merge target and all) instead of provisioning its own.
    const cwd = existingCwd ?? (mode === 'join' ? joinCwd : dir.path);
    const workSource = quick ? undefined : (mode === 'attach' ? selectedAttachRef : selectedGitRef);
    let effectiveOptions = quick ? null : spawnOptions;
    if (spawnAdapter === 'acp' && !effectiveOptions) {
      try {
        effectiveOptions = await getSpawnOptions(spawnAgent, dir.path, spawnHarnessID, hostId);
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
    const spawnSnapshot = quick ? quick.snapshotId : snapshot;
    const spec: SpawnSpec = {
      ...(remote ? { hostId } : {}),
      adapter: spawnAdapter,
      agent: spawnAgent,
      harness: spawnHarnessID,
      terminalArgs: spawnAdapter === 'pty'
        ? (quick ? '' : terminalArgsText).split('\n').map((arg) => arg.endsWith('\r') ? arg.slice(0, -1) : arg).filter((arg) => arg.length > 0)
        : undefined,
      workspace: mode === 'existing' || mode === 'join'
        ? { kind: 'existing', cwd }
        : {
            kind: 'worktree',
            repo: dir.path,
            branchMode: mode,
            branch: mode === 'attach' ? selectedAttachRef?.displayName : agentBranch || undefined,
            source: workSource ? { ref: workSource.ref, commit: workSource.commit } : undefined,
            integration: !quick && selectedGitRef ? {
              kind: selectedGitRef.kind === 'local-branch' || selectedGitRef.kind === 'remote-branch' ? selectedGitRef.kind : 'detached',
              ref: selectedGitRef.ref,
            } : undefined,
          },
      name: quick ? undefined : name || undefined,
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
        id: quick?.id ?? (trimmedProfileName ? undefined : baseProfile?.id),
        name: quick ? undefined : trimmedProfileName || undefined,
        ...(spawnAdapter === 'acp'
          ? {
              model: resolvedDefaults.model || undefined,
              effort: resolvedDefaults.effort || undefined,
              permission: resolvedDefaults.permission || undefined,
            }
          : {}),
        snapshot: spawnSnapshot || undefined,
      },
      handoffFrom: parentSession && includeTranscript ? parentSession : undefined,
      handoffMode: parentSession && includeTranscript ? handoffMode : undefined,
    };
    saveProjectAgent(dir.path, spawnHarness?.id ?? agent);
    if (!quick && selectedGitRef) saveBranchContext(dir.path, selectedGitRef.ref);
    // store.spawn closes the palette immediately; the focus area shows spawn
    // progress (and any error) and focuses the new session once it is up.
    const r = await spawn(spec);
    if (!r.error) recordRecentDir(dir.path);
  };

  // Activate a list row: a profile spawns with it, a repo opens the form for a
  // new profile. `customize` opens the form seeded from the profile instead.
  const activate = (row: PaletteRow | undefined, customize = false) => {
    if (!row || busy) return;
    if (row.kind === 'repo') openForm(row.dir);
    else if (customize) openForm(row.dir, row.profile);
    else void doSpawn(row.dir, { quick: row.profile });
  };

  const onKey = (e: React.KeyboardEvent) => {
    if (e.key === 'Escape') {
      if (menu) setMenu(null);
      else setModal('none');
      return;
    }
    if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') {
      e.preventDefault();
      if (advanced) {
        if (selectedDir && !busy) void doSpawn(selectedDir);
      } else activate(selectedRow, true);
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
      setSel(Math.min(rows.length - 1, selIndex + 1));
      return;
    }
    if (e.key === 'ArrowUp') {
      e.preventDefault();
      setSel(Math.max(0, selIndex - 1));
      return;
    }
    if (e.key === 'Enter') {
      e.preventDefault();
      activate(selectedRow);
    }
  };

  return (
    <div className="modal-scrim" onMouseDown={(e) => e.target === e.currentTarget && setModal('none')}>
      <div className="modal" onKeyDown={onKey}>
        {!advanced && (
          <>
            {hosts.length > 1 && (
              <label className="spawn-host">
                Host
                <select value={hostId} onChange={(e) => setHostId(e.target.value)}>
                  {hosts.map((host) => (
                    <option key={host.id} value={host.id} disabled={!host.local && host.status !== 'connected' && host.status !== 'accepted'}>
                      {host.name || host.id}{host.local ? ' (local)' : host.status && host.status !== 'connected' && host.status !== 'accepted' ? ` (${host.status})` : ''}
                    </option>
                  ))}
                </select>
              </label>
            )}
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
            <div className="rows" ref={rowsRef}>
              {filtered.length === 0 && <div className="empty">No git repos found under TANDEM_PROJECT_ROOTS.</div>}
              {rows.map((row, i) => row.kind === 'repo' ? (
                <div
                  key={row.dir.path}
                  className={`row repo-row${i === selIndex ? ' sel' : ''}`}
                  title="Launch with a new profile"
                  onMouseEnter={() => setSel(i)}
                  onClick={() => activate(row)}
                >
                  <div className="repo-details">
                    <div className="primary">{row.dir.name}</div>
                    <div className="sub">{row.dir.path}</div>
                  </div>
                  <div className="meta">
                    <span>{row.dir.currentBranch}</span>
                    {row.dir.dirty && <span className="dirty">● dirty</span>}
                    {row.dir.hasLiveAgent && <span className="occupied">◆ occupied</span>}
                    <span className="new-profile">+ new profile</span>
                  </div>
                </div>
              ) : (
                <ProfileRow
                  key={`${row.dir.path}::${row.profile.id}`}
                  profile={row.profile}
                  selected={i === selIndex}
                  onHover={() => setSel(i)}
                  onActivate={() => activate(row)}
                  onMenu={(origin) => { setSel(i); openMenu(origin, row.dir, row.profile); }}
                />
              ))}
            </div>
            {renderedMenu && createPortal(
              <div
                className={`session-context-menu spawn-profile-menu${menuClosing ? ' closing' : ''}`}
                style={{ left: renderedMenu.x, top: renderedMenu.y }}
                role="menu"
                aria-label={`Actions for ${renderedMenu.profile.name}`}
                onPointerDown={(event) => event.stopPropagation()}
                onMouseDown={(event) => event.stopPropagation()}
              >
                <button type="button" role="menuitem" onClick={() => openForm(renderedMenu.dir, renderedMenu.profile)}>Customize…</button>
                <button type="button" role="menuitem" className="danger" onClick={() => void forget(renderedMenu.dir, renderedMenu.profile)}>Forget for this repo</button>
              </div>,
              document.body,
            )}
          </>
        )}
        {advanced && (
          <>
            <div className="advanced-repo">
              <div>
                <div className="primary">{selectedDir?.name}</div>
                <div className="sub">{selectedHost?.name || selectedHost?.id}{remote ? ' · remote host' : ''} · {selectedDir?.path}</div>
              </div>
              <span>{selectedDir?.currentBranch}</span>
            </div>
            <div className="adv">
            <div className="adv-section" style={{ gridColumn: '1 / -1' }}>Agent profile</div>
            <label style={{ gridColumn: '1 / -1' }}>
              Start from <span className="sub">(latest 3 shown; fuzzy-find for more)</span>
              <ProfilePicker profiles={profiles} recent={recentProfiles} value={baseProfile} onPick={(p) => {
                setBaseProfile(p);
                applyProfile(p);
                // Move focus to Launch so a subsequent Enter spawns with the picked profile.
                requestAnimationFrame(() => launchRef.current?.focus());
              }} />
            </label>
            <label style={{ gridColumn: '1 / -1' }}>
              Profile name <span className="sub">(optional)</span>
              <input value={profileName} onChange={(e) => setProfileName(e.target.value)} placeholder={baseUnchanged && baseProfile ? baseProfile.name : `auto: ${autoName}`} />
              <div className="sub" style={{ marginTop: 4 }}>
                {resolvedProfile
                  ? <>Launches with profile <b>{resolvedProfile.name}</b></>
                  : <>Creates a new profile: <b>{trimmedProfileName || autoName}</b></>}
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
            <>
              <div className="adv-section" style={{ gridColumn: '1 / -1' }}>Hand-off</div>
              <label style={{ gridColumn: '1 / -1' }}>
                Continue from session <span className="sub">(optional)</span>
                <select value={parentSession} onChange={(e) => setParentSession(e.target.value)}>
                  <option value="">None — start a fresh conversation</option>
                  {parentCandidates.map((candidate) => (
                    <option key={candidate.id} value={candidate.id}>
                      {candidate.name}{candidate.workspace.branch ? ` · ${candidate.workspace.branch}` : ''}
                    </option>
                  ))}
                </select>
              </label>
              {parentAgent && (
                <>
                  <label className="delete-worktree-option" style={{ gridColumn: '1 / -1' }}>
                    <input type="checkbox" checked={includeTranscript} onChange={(e) => setIncludeTranscript(e.target.checked)} />
                    Start with a hand-off transcript of {parentAgent.name}'s conversation
                  </label>
                  {includeTranscript && (
                    <>
                      <label style={{ gridColumn: '1 / -1' }}>
                        Transcript detail
                        <select value={handoffMode} onChange={(e) => setHandoffMode(e.target.value as 'full' | 'brief')}>
                          <option value="full">Full — every message, each tool call summarized</option>
                          <option value="brief">Brief — turn-end messages only, tool calls counted</option>
                        </select>
                      </label>
                      <div className="git-context-preview" style={{ gridColumn: '1 / -1' }}>
                        The new session's first message will be a Hand-off Transcript assembled from <b>{parentAgent.name}</b>'s
                        conversation.{' '}
                        {handoffMode === 'full'
                          ? "Every user and assistant message is carried over verbatim, and each intervening tool call becomes one summary line naming the file it touched."
                          : "Every user message is carried over verbatim, but only each turn's closing message is — the work in between appears as a count of tool calls."}
                        {' '}It is built mechanically from the event log, so no model is asked to summarize anything.
                      </div>
                    </>
                  )}
                </>
              )}
              </>
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
                <option value="create">New session branch</option>
                <option value="attach">Continue existing branch</option>
                <option value="existing">Use existing checkout</option>
                <option value="join" disabled={!joinCwd}>
                    {parentAgent ? `Continue in ${parentAgent.name}'s worktree` : "Continue in the parent session's worktree"}
                </option>
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
            {usesGitRefs && (
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
            {workspaceMode === 'join' && (
              <div className="git-context-preview" style={{ gridColumn: '1 / -1' }}>
                The agent will work in <code>{joinCwd}</code>, the checkout {parentAgent?.name} is already using —
                including any uncommitted changes left there. No new worktree or branch is created.
              </div>
            )}
            {cohabitants.length > 0 && (
              <div className="branch-warning" style={{ gridColumn: '1 / -1' }}>
                This worktree is already occupied by {cohabitants.length === 1 ? 'session' : 'sessions'}{' '}
                <b>{cohabitants.join(', ')}</b>. Sessions sharing a checkout can overwrite each other's edits, and the
                worktree will not be removed until the last of them is deleted.
              </div>
            )}
            {usesGitRefs && selectedGitRef && (
              <div className="git-context-preview" style={{ gridColumn: '1 / -1' }}>
                <div>
                  {workspaceMode === 'create' ? <>Starting at <b>{selectedGitRef.displayName}</b> @ <code>{selectedGitRef.commit.slice(0, 8)}</code></> : <>Attaching <b>{selectedAttachRef?.displayName ?? '—'}</b>; merge target <b>{selectedGitRef.displayName}</b></>}
                </div>
                {workspaceMode === 'create' && <div>Session branch <code>{agentBranch || proposedBranch(selectedGitRef, name)}</code></div>}
                {selectedAttachRef?.checkedOutAt && workspaceMode === 'attach' && (
                  <div className="modal-err">
                    <div>This branch is already checked out at {selectedAttachRef.checkedOutAt}.</div>
                    <button
                      type="button"
                      className="btn"
                      style={{ marginTop: 8 }}
                      disabled={busy}
                      onClick={() => void doSpawn(selectedDir, { existingCwd: selectedAttachRef.checkedOutAt })}
                    >
                      Use this checked-out worktree
                    </button>
                  </div>
                )}
                {selectedGitRef.checkedOutAt === selectedDir?.path && selectedDir?.dirty && workspaceMode === 'create' && (
                  <div className="branch-warning">Uncommitted changes in the existing checkout are not included; the session starts from the committed revision above.</div>
                )}
              </div>
            )}
            {gitRefsError && usesGitRefs && <div className="modal-err" style={{ gridColumn: '1 / -1' }}>{gitRefsError}</div>}
            {usesGitRefs && (
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
          </div>
        )}
        {advanced ? (
          <div className="foot advanced-actions">
            <button className="btn ghost" type="button" onClick={() => setAdvanced(false)}>Choose other repo</button>
            <span className="action-spacer" />
            <button className="btn ghost" type="button" onClick={() => setModal('none')}>Cancel</button>
            <button ref={launchRef} className="btn" type="button" disabled={busy || optionsBusy || !selectedDir} onClick={() => selectedDir && void doSpawn(selectedDir)}>
              {busy ? 'Launching…' : 'Launch'}
            </button>
          </div>
        ) : (
          <div className="foot">
            <span><span className="kbd">↵</span> spawn</span>
            <span><span className="kbd">⌘↵</span> customize</span>
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

// One previously-used profile listed under its repo. Click/Enter spawns with it;
// right-click or a touch long-press opens its Customize / Forget menu.
function ProfileRow({
  profile,
  selected,
  onHover,
  onActivate,
  onMenu,
}: {
  profile: Profile;
  selected: boolean;
  onHover: () => void;
  onActivate: () => void;
  onMenu: (origin: { x: number; y: number }) => void;
}) {
  // Touch long-press is recognized directly: a browser `contextmenu` for it is
  // inconsistently delivered (iOS never sends one).
  const longPress = useRef<{ pointerId: number; x: number; y: number; timer: number } | null>(null);
  const longPressOpened = useRef(false);
  const cancelLongPress = () => {
    if (!longPress.current) return;
    window.clearTimeout(longPress.current.timer);
    longPress.current = null;
  };
  useEffect(() => cancelLongPress, []);
  return (
    <div
      className={`row profile-row${selected ? ' sel' : ''}`}
      title="Launch with this profile (right-click or long-press for more)"
      onMouseEnter={onHover}
      onClick={() => {
        if (longPressOpened.current) {
          longPressOpened.current = false;
          return;
        }
        onActivate();
      }}
      onContextMenu={(event) => {
        event.preventDefault();
        cancelLongPress();
        onMenu({ x: event.clientX, y: event.clientY });
      }}
      onPointerDown={(event) => {
        if (event.pointerType !== 'touch') return;
        cancelLongPress();
        longPressOpened.current = false;
        const { pointerId, clientX: x, clientY: y } = event;
        const timer = window.setTimeout(() => {
          longPress.current = null;
          longPressOpened.current = true;
          onMenu({ x, y });
        }, 550);
        longPress.current = { pointerId, x, y, timer };
      }}
      onPointerMove={(event) => {
        const gesture = longPress.current;
        if (!gesture || event.pointerId !== gesture.pointerId) return;
        // Movement beyond natural finger drift is a scroll, not a long-press.
        if (Math.hypot(event.clientX - gesture.x, event.clientY - gesture.y) > 12) cancelLongPress();
      }}
      onPointerUp={(event) => {
        if (event.pointerId === longPress.current?.pointerId) cancelLongPress();
      }}
      onPointerCancel={cancelLongPress}
    >
      <span className="profile-name">{profile.name}</span>
      {!profile.autoNamed && <span className="profile-kind">named</span>}
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
  const { mounted: resultsOpen, closing: resultsClosing } = usePresence(open);
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
      {resultsOpen && (
        <div className={`git-ref-results${resultsClosing ? ' closing' : ''}`}>
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
  const { mounted: resultsOpen, closing: resultsClosing } = usePresence(open);

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
      {resultsOpen && !busy && (
        <div className={`git-ref-results${resultsClosing ? ' closing' : ''}`}>
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
                {ref.tandem && <em>{ref.tandem.live ? 'live Tandem session' : 'Tandem branch'}</em>}
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
