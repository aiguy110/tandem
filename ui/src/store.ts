// The client state store: a projection of daemon messages (docs/ui.md). The
// browser holds no authoritative state — every field here is derived from a
// ServerMsg. Built on zustand (lightweight, no framework). The WsClient is owned
// here so actions and the reducer share one socket.

import { create } from 'zustand';
import { WsClient, resolveToken, type ConnState } from './ws/client';
import type { AudioPosition } from './audio/engine';
import { getListeningAgentId, getState as getEngineState, setPlaylist as setEnginePlaylist } from './audio/engine';
import { ptyHub } from './terminal/ptyHub';
import { shellHub } from './terminal/shellHub';
import { browserHub } from './terminal/browserHub';
import type {
  SessionStatus,
  SessionSummary,
  AgentCatalog,
  Annotation,
  Approval,
  AutomationJob,
  AutomationRun,
  BrowserInputWire,
  BrowserSnapshot,
  Channel,
  ClosePreview,
  ClientMsg,
  GitRefInfo,
  FederationHost,
  Profile,
  RepoInfo,
  ResumableSession,
  ResumeCatalog,
  SessionSearchResult,
  ServerMsg,
  SessionConfigOption,
  SessionModeState,
  SystemNotification,
  SlashCommand,
  SpawnSpec,
  SpawnOptions,
  PromptBlock,
  QueuedPrompt,
  WireEvent,
  WorkspaceDiff,
  WorkspaceEntry,
} from './wire';

// The focus/bandwidth rules: browser frames stream only for the focused Browser
// pane, and raw PTY / worktree-shell scrollback streams only for the focused
// CLI or Terminal view. In particular, a page load must not replay every
// agent's terminal history before the user asks to see it.
const BASE_CHANNELS: Channel[] = ['transcript', 'terminals', 'status'];

// A pending session-initiated takeover (browser.request_takeover) for the rail.
export interface Takeover {
  reqId: string;
  reason: string;
}

// Severity of a completed-turn notification, in ascending order of urgency.
// success = the agent finished its turn cleanly (green); attention = it needs
// user input, e.g. a pending approval or browser takeover (yellow); failure =
// the turn errored out (red).
export type NotifSeverity = 'success' | 'attention' | 'failure';
export type ThreadAudioState = 'idle' | 'rendering' | 'ready' | 'error';

const SEVERITY_RANK: Record<NotifSeverity, number> = { success: 1, attention: 2, failure: 3 };

// Highest-urgency severity in the list, or null when the list is empty.
export function maxSeverity(severities: NotifSeverity[]): NotifSeverity | null {
  return severities.reduce<NotifSeverity | null>(
    (acc, s) => (acc && SEVERITY_RANK[acc] >= SEVERITY_RANK[s] ? acc : s),
    null,
  );
}

// Unread state is local to this browser; the underlying completed turn remains
// available in the durable transcript.
export interface TurnNotification {
  seq: number;
  createdAt: number;
  severity: NotifSeverity;
}

// Severity a status transition should raise as a completed-turn notification,
// or null when the transition is not worth notifying about. A pending approval
// or browser takeover surfaces its own actionable card and contributes
// 'attention' to the badges directly (see agentBadge), so 'blocked' itself is
// intentionally not notified here to avoid duplicate cards.
function turnNotificationSeverity(prev: SessionStatus, next: SessionStatus): NotifSeverity | null {
  if (next === 'error' && prev !== 'error') return 'failure';
  if (next === 'idle' && prev === 'working') return 'success';
  return null;
}

// Aggregate notification state for one agent's tab badge: the count of items
// living under it (unread turns + pending approvals + browser takeovers) and
// the highest severity among them, so the badge can be colored red > yellow >
// green.
export function agentBadge(agent: SessionView): { count: number; severity: NotifSeverity | null } {
  const severities = agent.turnNotifications.map((n) => n.severity);
  const attention = agent.pendingApprovals.length + agent.takeovers.length;
  for (let i = 0; i < attention; i++) severities.push('attention');
  return { count: severities.length, severity: maxSeverity(severities) };
}

// Chat = the agent conversation (ACP transcript or the agent's resumable CLI,
// toggled by the selected Chat tab's ACP/CLI switch). Shell = the user's escape-hatch shell
// in the agent's worktree (the Terminal tab).
export type PaneId = 'chat' | 'shell' | 'diff' | 'browser';
export const PANES: PaneId[] = ['chat', 'shell', 'diff', 'browser'];

export interface SessionView {
  id: string;
  name: string;
  agent?: string;
  hostId?: string;
  hostName?: string;
  profile?: {
    id?: string;
    model?: string;
    effort?: string;
    permission?: string;
    snapshot?: string;
  };
  workspace: {
    kind: 'worktree' | 'existing';
    repo: string;
    repoPath: string;
    branch: string;
    cwd: string;
    gitState?: 'dirty' | 'ahead' | 'behind' | 'diverged' | 'merged' | 'synced' | 'target_missing' | 'unknown';
    ahead?: number;
    behind?: number;
    targetRef?: string;
    targetKind?: 'local-branch' | 'remote-branch' | 'detached';
    startCommit?: string;
  };
  status: SessionStatus;
  events: { seq: number; event: WireEvent }[]; // transcript/terminals channel, seq-ordered
  lastSeq: number;
  pendingApprovals: Approval[];
  turnNotifications: TurnNotification[];
  hasPty: boolean; // any raw_pty seen → the agent CLI view has live content
  // User escape-hatch shell (Terminal tab). shellExited flips true when the
  // shell process ends (shell_exit) so the pane can offer a restart.
  shellExited: boolean;
  shellExitMessage: string | null;
  // Browser subsystem (Phase 5): whether a browser exists for this agent and who
  // holds the wheel, plus any pending session-initiated takeover requests.
  browserActive: boolean;
  browserOwner: 'agent' | 'user';
  // True while the user is satisfying an session-requested takeover. This keeps
  // the hand-back action visually distinct from an unsolicited manual grab.
  browserTakeoverHeld: boolean;
  takeovers: Takeover[];
  // Permission-mode + config-option (incl. model selector) state, from the last
  // session_config event. Null until the ACP agent reports it (or for pty sessions,
  // which never do) — the picker bar hides itself in that case.
  sessionConfig: { modes: SessionModeState | null; configOptions: SessionConfigOption[] } | null;
  usage: { used: number; size: number; cost?: { amount: number; currency: string } | null; updatedAt: number } | null;
  // The agent's slash-command menu (ACP available_commands_update), for the
  // fuzzy-find popup in PromptBar. Empty for pty sessions / until first reported.
  commands: SlashCommand[];
  // null until the adapter reports ACP prompt capabilities.
  imagePromptSupport: boolean | null;
  asideSupport: boolean | null;
  steeringSupport: boolean | null;
  // Daemon-owned FIFO entries waiting behind the active turn.
  queuedPrompts: QueuedPrompt[];
  controlMode: 'transcript' | 'switching' | 'terminal';
  // Stable adapter kind + whether the Chat tab offers the ACP/CLI switch.
  adapter: 'acp' | 'pty';
  canHandoff: boolean;
  // Daemon-owned preference and render state, replayed to every client.
  audioOnTurnEnd: boolean;
  audioState: ThreadAudioState;
  audioError: string | null;
  audioSeq: number | null;
  // Every persisted clip, supplied by daemon snapshots so a second device can
  // reconstruct each message player without re-rendering speech.
  audioReadySeqs: number[];
  // Known clip durations (ms) by seq, for spacing timeline tick marks
  // without downloading audio. A seq absent here (or a live ready event
  // without durationMs) means unknown duration.
  audioDurations: Record<number, number>;
  // Advances only for a live ready event. It lets the focused chat autoplay
  // newly completed clips without replaying historical audio on reconnect.
  audioReadyRevision: number;
  // Daemon-persisted "where was this chat's playback last" — the durable half
  // of position restore (ui/src/audio/engine.ts owns the browser-side
  // localStorage half), so the player resumes across a session switch or a
  // closed tab. null means nothing is stored: never played, or cleared by a
  // `seq: 0` write. See docs/ws-protocol.md for the wire contract.
  audioPosition: AudioPosition | null;
}

export type ModalKind = 'none' | 'spawn' | 'command' | 'resume' | 'automation';

export interface AckResult {
  sessionId?: string;
  error?: string;
  promptId?: string;
  disposition?: 'started' | 'queued' | 'steered';
  position?: number;
  cleared?: number;
}

interface StoreState {
  conn: ConnState;
  theme: 'dark' | 'light';
  sessions: Record<string, SessionView>;
  order: string[];
  focusedId: string | null;
  // The selected pane belongs to an agent/session, rather than to the focus
  // area. `pane` remains the currently focused agent's pane for consumers that
  // need a simple current-view value.
  panesBySession: Record<string, PaneId>;
  pane: PaneId;
  modal: ModalKind;
  // Agent the spawn palette should offer to hand off from ('' / null = none).
  // Set by the rail's "Hand off" action; cleared whenever the modal changes.
  spawnHandoffFrom: string | null;
  inspectorOpen: boolean;
  dirs: RepoInfo[];
  agentCatalog: AgentCatalog | null;
  // Federation data is keyed by host ID. `dirs` and `agentCatalog` remain the
  // local aliases so existing consumers and older daemons need no migration.
  hosts: FederationHost[];
  dirsByHost: Record<string, RepoInfo[]>;
  agentCatalogByHost: Record<string, AgentCatalog | null>;
  // Captured browser snapshots (seed states), refreshed on demand.
  snapshots: BrowserSnapshot[];
  automationJobs: AutomationJob[];
  automationRuns: AutomationRun[];
  automationLoading: boolean;
  automationError: string | null;
  // Resume palette: the resumable-session catalog (null until first fetched) and
  // a loading flag while the daemon probes sessions for external sessions.
  resumeCatalog: ResumeCatalog | null;
  resumeCatalogByHost: Record<string, ResumeCatalog>;
  resumePendingHostIds: Record<string, boolean>;
  resumeLoading: boolean;
  // Unsent prompt drafts, keyed by sessionId. Lives here (not in the pane's local
  // state) so a draft survives tab switches and agent switches, which remount the
  // TranscriptPane.
  drafts: Record<string, string>;
  // Pending transcript annotations (review tray), keyed by sessionId. Daemon-owned:
  // hydrated from `snapshot` and replaced wholesale by `annotations` broadcasts —
  // actions never mutate this locally, they only send the WS message and wait
  // for the echo (cross-device correctness).
  annotations: Record<string, Annotation[]>;
  // Which agent (if any) currently has its browser channel subscribed (i.e. the
  // focused agent with the Browser pane open) — drives the screencast focus rule.
  browserSubAgent: string | null;
  // Left/right rail collapse (mobile-friendly docking). Defaults from a
  // matchMedia breakpoint at boot, then user-toggleable regardless of width.
	sessionsRailCollapsed: boolean;
  approvalsRailCollapsed: boolean;
  // Daemon-owned operational notifications, including self-update actions.
  systemNotifications: SystemNotification[];

  // actions
  boot: () => void;
  submitToken: (t: string) => void;
  focus: (id: string) => void;
  reorderAgent: (id: string, targetId: string, after: boolean) => void;
  markAgentUnread: (id: string) => void;
  setPane: (p: PaneId) => void;
  toggleTheme: () => void;
  toggleSessionsRail: () => void;
  toggleApprovalsRail: () => void;
  setModal: (m: ModalKind) => void;
  // Open the spawn palette pre-selected to hand off from this agent.
  handOffAgent: (sessionId: string) => void;
  toggleInspector: () => void;
  toggleThreadAudio: (sessionId: string) => void;
  // The durable half of playback-position restore: ui/src/audio/engine.ts
  // calls this (via a sender it's handed at app root) on its throttled /
  // flush-on-teardown schedule. `seq: 0` means "no active section" and clears
  // the daemon's stored position. Throttling is the caller's responsibility.
  setAudioPosition: (sessionId: string, seq: number, positionMs: number) => void;
  refreshDirs: () => void;
  refreshHostDirs: (hostId: string) => void;
  refreshHosts: () => void;
  refreshAgentCatalog: (hostId: string) => void;
  refreshAgents: () => void;
  refreshSessions: (hostId?: string) => void;
  refreshAutomation: (repositoryId?: string) => Promise<void>;
  setAutomationEnabled: (id: string, enabled: boolean) => Promise<void>;
  searchSessions: (query: string, hostId?: string) => Promise<SessionSearchResult[]>;
  resumeSession: (s: ResumableSession) => Promise<AckResult>;
  enterTerminal: (sessionId: string, interrupt?: boolean) => Promise<AckResult>;
  leaveTerminal: (sessionId: string) => Promise<AckResult>;
  // User escape-hatch shell (Terminal tab).
  openShell: (sessionId: string, cols: number, rows: number) => Promise<AckResult>;
  restartShell: (sessionId: string, cols: number, rows: number) => Promise<AckResult>;
  spawn: (spec: SpawnSpec) => Promise<AckResult>;
  actOnSystemNotification: (notificationId: string, action: string) => Promise<AckResult>;
  getSpawnOptions: (agent: string, cwd: string, harness?: string, hostId?: string) => Promise<SpawnOptions>;
  listGitRefs: (repo: string, hostId?: string) => Promise<GitRefInfo[]>;
  listWorkspaceEntries: (sessionId: string, path: string) => Promise<WorkspaceEntry[]>;
  // Browser snapshots + agent profiles.
  captureSnapshot: (sessionId: string, name: string) => Promise<BrowserSnapshot[]>;
  listSnapshots: () => Promise<BrowserSnapshot[]>;
  deleteSnapshot: (id: string) => Promise<BrowserSnapshot[]>;
  listProfiles: (project?: string) => Promise<{ profiles: Profile[]; recent: string[] }>;
  renameProfile: (id: string, name: string, project?: string) => Promise<{ profiles: Profile[]; recent: string[] }>;
  renameAgent: (sessionId: string, name: string) => Promise<AckResult>;
  deleteProfile: (id: string, project?: string) => Promise<{ profiles: Profile[]; recent: string[] }>;
  prompt: (sessionId: string, input: string | PromptBlock[]) => Promise<AckResult>;
  steer: (sessionId: string, input: string | PromptBlock[]) => Promise<AckResult>;
  aside: (sessionId: string, question: string) => Promise<AckResult>;
  removeQueuedPrompt: (sessionId: string, promptId: string) => Promise<AckResult>;
  clearPromptQueue: (sessionId: string) => Promise<AckResult>;
  interruptAndClearQueue: (sessionId: string) => Promise<AckResult>;
  addAnnotation: (sessionId: string, anchor: { seq: number; role: string; quote: string }, comment: string) => Promise<AckResult>;
  updateAnnotation: (sessionId: string, id: string, comment: string) => Promise<AckResult>;
  removeAnnotation: (sessionId: string, id: string) => Promise<AckResult>;
  clearAnnotations: (sessionId: string) => Promise<AckResult>;
  setDraft: (sessionId: string, text: string) => void;
  interrupt: (sessionId: string) => void;
  respond: (sessionId: string, reqId: string, optionId: string) => void;
  setMode: (sessionId: string, modeId: string) => void;
  setConfigOption: (sessionId: string, configId: string, value: string | boolean) => void;
  getClosePreview: (sessionId: string) => Promise<ClosePreview>;
  getDiff: (sessionId: string) => Promise<WorkspaceDiff>;
  closeAgent: (sessionId: string, force?: boolean, deleteWorktree?: boolean, deinitSubmodules?: boolean) => Promise<AckResult>;
  send: (m: ClientMsg) => void;
  nav: (dir: 1 | -1) => void;
  // Browser pane control (Phase 5).
  setBrowserSub: (sessionId: string | null) => void;
  browserControl: (sessionId: string, action: 'grab' | 'release') => void;
  restartBrowser: (sessionId: string, snapshotId?: string) => Promise<AckResult>;
  restartHarness: (sessionId: string) => Promise<AckResult>;
  browserInput: (sessionId: string, event: BrowserInputWire) => void;
  toggleWheel: (sessionId: string) => void;
}

// ---- ack correlation (spawn/close want structured results) ----
let corrCounter = 0;
const nextCorr = () => `c${++corrCounter}`;
const pendingAcks = new Map<string, (r: AckResult) => void>();
const pendingSpawnOptions = new Map<string, { resolve: (options: SpawnOptions) => void; reject: (error: Error) => void }>();
const pendingGitRefs = new Map<string, { resolve: (refs: GitRefInfo[]) => void; reject: (error: Error) => void }>();
const pendingWorkspaceEntries = new Map<string, { resolve: (entries: WorkspaceEntry[]) => void; reject: (error: Error) => void }>();
const pendingClosePreviews = new Map<string, { resolve: (preview: ClosePreview) => void; reject: (error: Error) => void }>();
const pendingDiffs = new Map<string, { resolve: (diff: WorkspaceDiff) => void; reject: (error: Error) => void }>();
const pendingSnapshots = new Map<string, { resolve: (snaps: BrowserSnapshot[]) => void; reject: (error: Error) => void }>();
const pendingProfiles = new Map<string, { resolve: (r: { profiles: Profile[]; recent: string[] }) => void; reject: (error: Error) => void }>();
const pendingSessionSearches = new Map<string, { resolve: (results: SessionSearchResult[]) => void; reject: (error: Error) => void }>();
const pendingAutomation = new Map<string, { resolve: () => void; reject: (error: Error) => void }>();

let client: WsClient;
// Guards the one-time window 'hashchange' listener boot() installs (boot may run
// twice under React StrictMode in dev).
let hashListenerAttached = false;
let gitRefreshListenersAttached = false;
// The store's ServerMsg reducer (`apply`, defined inside the `create` factory
// below) captured here so tests can drive it directly without a real
// WebSocket — this is the ONLY place the daemon mutates agent/annotation
// state, so exercising it is how a "store reducer test" is possible at all.
// Not used by runtime code outside this module.
let applyServerMsg: (msg: ServerMsg) => void;
export function __testApplyServerMsg(msg: ServerMsg): void {
  applyServerMsg(msg);
}

const GIT_REFRESH_INTERVAL_MS = 15_000;
export const LOCAL_HOST_ID = 'local';

// Never put the synthesized local ID on the wire: an older daemon sees the
// exact commands it has always seen. Federation-aware masters may explicitly
// include their local host in the hosts list, so normalize that shape too.
export function isLocalHost(hostId: string | undefined): boolean {
  return !hostId || hostId === LOCAL_HOST_ID;
}

function normalizedHosts(hosts: FederationHost[]): FederationHost[] {
  const local: FederationHost = { id: LOCAL_HOST_ID, name: 'This host', status: 'connected', local: true };
  const explicitLocal = hosts.find((host) => host.local || host.id === LOCAL_HOST_ID);
  const remotes = hosts.filter((host) => host !== explicitLocal && host.id !== LOCAL_HOST_ID);
  return [explicitLocal ? { ...local, ...explicitLocal, id: LOCAL_HOST_ID, local: true } : local, ...remotes];
}

function catalogForHost(catalog: ResumeCatalog, hostId: string, hosts: FederationHost[]): ResumeCatalog {
  const host = hosts.find((entry) => entry.id === hostId);
  return {
    ...catalog,
    sessions: catalog.sessions.map((session) => isLocalHost(hostId)
      ? session
      : { ...session, hostId, hostName: host?.name ?? hostId }),
  };
}

function combinedCatalog(catalogs: Record<string, ResumeCatalog>): ResumeCatalog | null {
  const values = Object.values(catalogs);
  if (!values.length) return null;
  return {
    sessions: values.flatMap((catalog) => catalog.sessions),
    adapters: values.flatMap((catalog) => catalog.adapters),
  };
}

function rankSessions(sessions: Record<string, SessionView>, order: string[]): string[] {
  // `order` is explicitly arranged by the user via the Sessions rail. Filter
  // stale entries rather than applying a status-based sort over that order.
  return order.filter((id) => !!sessions[id]);
}

const initialTheme = (): 'dark' | 'light' => {
  const saved = localStorage.getItem('tandem.theme');
  return saved === 'light' ? 'light' : 'dark';
};

const SESSION_ORDER_STORAGE_KEY = 'tandem.sessionOrder';
const LEGACY_AGENT_ORDER_STORAGE_KEY = 'tandem.agentOrder';
const initialSessionOrder = (): string[] => {
	try {
		const saved: unknown = JSON.parse(localStorage.getItem(SESSION_ORDER_STORAGE_KEY) ?? localStorage.getItem(LEGACY_AGENT_ORDER_STORAGE_KEY) ?? '[]');
    return Array.isArray(saved) && saved.every((id) => typeof id === 'string') ? saved : [];
  } catch {
    return [];
  }
};
const saveSessionOrder = (order: string[]) => {
	try {
		localStorage.setItem(SESSION_ORDER_STORAGE_KEY, JSON.stringify(order));
  } catch {
    // Reordering still works when browser storage is unavailable.
  }
};

const FOCUSED_SESSION_STORAGE_KEY = 'tandem.focusedSession';
const LEGACY_FOCUSED_AGENT_STORAGE_KEY = 'tandem.focusedAgent';
const initialFocusedSession = (): string | null => {
	try {
		const saved = localStorage.getItem(FOCUSED_SESSION_STORAGE_KEY) ?? localStorage.getItem(LEGACY_FOCUSED_AGENT_STORAGE_KEY);
    return saved && saved.trim() ? saved : null;
  } catch {
    return null;
  }
};
const saveFocusedSession = (sessionId: string | null): void => {
	try {
		if (sessionId) localStorage.setItem(FOCUSED_SESSION_STORAGE_KEY, sessionId);
		else localStorage.removeItem(FOCUSED_SESSION_STORAGE_KEY);
  } catch {
    // Focusing still works when browser storage is unavailable.
  }
};

// Prompt drafts are deliberately browser-owned. Unlike queued or submitted
// prompts, they have not reached the daemon yet, so keeping them here also
// makes a daemon restart harmless to an in-progress composition.
const DRAFTS_STORAGE_KEY = 'tandem.promptDrafts';
const initialDrafts = (): Record<string, string> => {
  try {
    const saved: unknown = JSON.parse(localStorage.getItem(DRAFTS_STORAGE_KEY) ?? '{}');
    if (!saved || typeof saved !== 'object' || Array.isArray(saved)) return {};
    return Object.fromEntries(
      Object.entries(saved).filter((entry): entry is [string, string] =>
        typeof entry[0] === 'string' && typeof entry[1] === 'string'),
    );
  } catch {
    return {};
  }
};
const saveDrafts = (drafts: Record<string, string>): void => {
  try {
    localStorage.setItem(DRAFTS_STORAGE_KEY, JSON.stringify(drafts));
  } catch {
    // Drafting still works when browser storage is unavailable.
  }
};

const SESSION_PANES_STORAGE_KEY = 'tandem.sessionPanes';
const LEGACY_AGENT_PANES_STORAGE_KEY = 'tandem.agentPanes';
const initialSessionPanes = (): Record<string, PaneId> => {
  try {
		const saved: unknown = JSON.parse(localStorage.getItem(SESSION_PANES_STORAGE_KEY) ?? localStorage.getItem(LEGACY_AGENT_PANES_STORAGE_KEY) ?? '{}');
    if (!saved || typeof saved !== 'object' || Array.isArray(saved)) return {};
    return Object.fromEntries(
      Object.entries(saved).filter((entry): entry is [string, PaneId] =>
        typeof entry[0] === 'string' && PANES.includes(entry[1] as PaneId)),
    );
  } catch {
    return {};
  }
};
const saveSessionPanes = (panes: Record<string, PaneId>) => {
  try {
		localStorage.setItem(SESSION_PANES_STORAGE_KEY, JSON.stringify(panes));
  } catch {
    // Switching panes still works when browser storage is unavailable.
  }
};

// Rails collapse by default on narrow viewports (phones/small tablets), but the
// user can still toggle them open regardless of width.
const MOBILE_BREAKPOINT = '(max-width: 860px)';
const isNarrowViewport = (): boolean =>
  typeof window !== 'undefined' && !!window.matchMedia && window.matchMedia(MOBILE_BREAKPOINT).matches;

export const useStore = create<StoreState>((set, get) => {
  let audioFocusAgent: string | null = null;
  function syncAudioFocus(): void {
    const state = get();
    // Normally focus tracks "chat pane, document visible" — but a hidden
    // document is exactly the phone-screen-off listening case this exists
    // for, so don't drop focus (and thus tell the daemon to stop
    // pre-rendering speech) while the audio engine is actually playing or
    // armed mid-section for some chat. Only fall back to "no focus" when the
    // engine is genuinely not listening (no playlist, idle) — see
    // getListeningAgentId's doc in engine.ts.
    const visible = document.visibilityState === 'visible' && state.pane === 'chat';
    const next = visible ? state.focusedId : getListeningAgentId();
    if (next === audioFocusAgent) return;
    if (audioFocusAgent) client.send({ t: 'set_audio_focus', sessionId: audioFocusAgent, focused: false });
    audioFocusAgent = next;
    if (next) client.send({ t: 'set_audio_focus', sessionId: next, focused: true });
  }
  // Channels to subscribe for an agent: base always; browser only for the
  // focused Browser pane; and terminal bytes only when the user is looking at
  // the agent CLI or worktree Terminal. raw_pty and shell_pty share the daemon
  // `pty` channel, so visiting either terminal surface enables it.
  const wantsPty = (id: string): boolean => {
    const state = get();
    if (state.focusedId !== id) return false;
    if (state.pane === 'shell') return true;
    const agent = state.sessions[id];
    return state.pane === 'chat' && !!agent && (agent.adapter === 'pty' || agent.controlMode === 'terminal');
  };
  const channelsFor = (id: string): Channel[] => {
    const channels = [...BASE_CHANNELS];
    if (wantsPty(id)) channels.push('pty');
    if (get().browserSubAgent === id) channels.push('browser');
    return channels;
  };
  function subscribeAgent(id: string, replayPty = false): void {
    // Terminal output was deliberately skipped while this agent was in the
    // background. Request its full channel history only on the first visit to
    // a terminal surface; normal re-subscriptions continue incrementally.
    const sinceSeq = replayPty ? 0 : get().sessions[id]?.lastSeq ?? 0;
    client.send({ t: 'subscribe', sessionId: id, channels: channelsFor(id), sinceSeq });
  }
  function replayPtyFor(id: string): void {
    if (!get().sessions[id]) return;
    // A full terminal replay replaces prior terminal state. Clear the client
    // buffers first so re-visiting a pane never appends duplicate scrollback.
    ptyHub.clear(id);
    shellHub.clear(id);
    subscribeAgent(id, true);
  }

  // Apply one server message. This is the ONLY place agent state is mutated by
  // the daemon (actions mutate only local UI concerns like focus/pane/theme).
  const apply = (msg: ServerMsg): void => {
    switch (msg.t) {
		case 'agents': {
        const newlyDiscovered = msg.sessions.filter((s) => !get().sessions[s.id]).map((s) => s.id);
        set((st) => {
          const sessions = { ...st.sessions };
          // Retain a saved order only for sessions that still exist locally or
          // were included by the daemon; this also discards old browser state.
          const live = new Set(msg.sessions.map((a) => a.id));
          const order = st.order.filter((id) => live.has(id) || !!sessions[id]);
          for (const s of msg.sessions) {
            const prev = sessions[s.id];
            sessions[s.id] = mergeSummary(prev, s);
            if (!order.includes(s.id)) order.push(s.id);
          }
          // Drop any local agent the daemon no longer reports (e.g. closed elsewhere).
          for (const id of order.slice()) {
            if (!live.has(id) && sessions[id] && sessions[id].events.length === 0) {
              delete sessions[id];
              order.splice(order.indexOf(id), 1);
            }
          }
          const focusedId = st.focusedId && sessions[st.focusedId] ? st.focusedId : order[0] ?? null;
          const pane = focusedId ? st.panesBySession[focusedId] ?? 'chat' : 'chat';
          if (focusedId !== st.focusedId) saveFocusedSession(focusedId);
          return { sessions, order, focusedId, pane };
        });
        // The reconnect path already re-subscribes tracked sessions. Summary
        // refreshes only need to subscribe sessions discovered for the first time.
        for (const id of newlyDiscovered) subscribeAgent(id);
        return;
      }
      case 'dirs':
        set((st) => {
          const hostId = msg.hostId ?? LOCAL_HOST_ID;
          return {
            dirs: isLocalHost(hostId) ? msg.dirs : st.dirs,
            dirsByHost: { ...st.dirsByHost, [hostId]: msg.dirs },
          };
        });
        return;
      case 'git_refs': {
        const pending = msg.corrId ? pendingGitRefs.get(msg.corrId) : undefined;
        if (pending && msg.corrId) {
          pendingGitRefs.delete(msg.corrId);
          if (msg.error || !msg.refs) pending.reject(new Error(msg.error ?? 'Git refs unavailable'));
          else pending.resolve(msg.refs);
        }
        return;
      }
      case 'workspace_entries': {
        const pending = msg.corrId ? pendingWorkspaceEntries.get(msg.corrId) : undefined;
        if (pending && msg.corrId) {
          pendingWorkspaceEntries.delete(msg.corrId);
          if (msg.error || !msg.entries) pending.reject(new Error(msg.error ?? 'Workspace entries unavailable'));
          else pending.resolve(msg.entries);
        }
        return;
      }
      case 'agent_catalog':
        set((st) => {
          const hostId = msg.hostId ?? LOCAL_HOST_ID;
          return {
            agentCatalog: isLocalHost(hostId) ? msg.catalog : st.agentCatalog,
            agentCatalogByHost: { ...st.agentCatalogByHost, [hostId]: msg.catalog },
          };
        });
        return;
      case 'hosts':
        set({ hosts: normalizedHosts(msg.hosts) });
        return;
      case 'spawn_options': {
        const pending = msg.corrId ? pendingSpawnOptions.get(msg.corrId) : undefined;
        if (pending) {
          pendingSpawnOptions.delete(msg.corrId!);
          if (msg.error || !msg.options) pending.reject(new Error(msg.error ?? 'ACP server returned no spawn options'));
          else pending.resolve(msg.options);
        }
        return;
      }
      case 'snapshots': {
        const pending = msg.corrId ? pendingSnapshots.get(msg.corrId) : undefined;
        const snaps = msg.snapshots ?? [];
        if (!msg.error && msg.snapshots) set({ snapshots: snaps });
        if (pending && msg.corrId) {
          pendingSnapshots.delete(msg.corrId);
          if (msg.error) pending.reject(new Error(msg.error));
          else pending.resolve(snaps);
        }
        break;
      }
      case 'system_notifications':
        set({ systemNotifications: msg.notifications });
        return;
      case 'profiles': {
        const pending = msg.corrId ? pendingProfiles.get(msg.corrId) : undefined;
        if (pending && msg.corrId) {
          pendingProfiles.delete(msg.corrId);
          if (msg.error || !msg.profiles) pending.reject(new Error(msg.error ?? 'profiles unavailable'));
          else pending.resolve({ profiles: msg.profiles, recent: msg.recent ?? [] });
        }
        break;
      }
      case 'sessions':
        set((st) => {
          const hostId = msg.hostId ?? LOCAL_HOST_ID;
          const tagged = catalogForHost(msg.catalog, hostId, st.hosts);
          const resumeCatalogByHost = { ...st.resumeCatalogByHost, [hostId]: tagged };
          const resumePendingHostIds = { ...st.resumePendingHostIds };
          delete resumePendingHostIds[hostId];
          return {
            resumeCatalogByHost,
            resumeCatalog: combinedCatalog(resumeCatalogByHost),
            resumePendingHostIds,
            resumeLoading: Object.keys(resumePendingHostIds).length > 0,
          };
        });
        return;
      case 'automation': {
        if (msg.error) {
          set({ automationLoading: false, automationError: msg.error });
        } else {
          set({
            automationJobs: msg.jobs ?? [],
            automationRuns: msg.runs ?? [],
            automationLoading: false,
            automationError: null,
          });
        }
        const pending = msg.corrId ? pendingAutomation.get(msg.corrId) : undefined;
        if (pending && msg.corrId) {
          pendingAutomation.delete(msg.corrId);
          if (msg.error) pending.reject(new Error(msg.error));
          else pending.resolve();
        }
        return;
      }
      case 'session_search': {
        const pending = msg.corrId ? pendingSessionSearches.get(msg.corrId) : undefined;
        if (pending && msg.corrId) {
          pendingSessionSearches.delete(msg.corrId);
          if (msg.error) pending.reject(new Error(msg.error));
          else pending.resolve(msg.results ?? []);
        }
        return;
      }
      case 'close_preview': {
        const pending = msg.corrId ? pendingClosePreviews.get(msg.corrId) : undefined;
        if (pending && msg.corrId) {
          pendingClosePreviews.delete(msg.corrId);
          if (msg.error || !msg.preview) pending.reject(new Error(msg.error || 'close preview unavailable'));
          else pending.resolve(msg.preview);
        }
        return;
      }
      case 'diff': {
        const pending = msg.corrId ? pendingDiffs.get(msg.corrId) : undefined;
        if (pending && msg.corrId) {
          pendingDiffs.delete(msg.corrId);
          if (msg.error || !msg.diff) pending.reject(new Error(msg.error || 'diff unavailable'));
          else pending.resolve(msg.diff);
        }
        return;
      }
      case 'browser_frame':
        // Frames bypass the reactive store (browserHub) to avoid re-render storms.
        browserHub.push(msg.sessionId, { dataB64: msg.dataB64, meta: msg.meta });
        return;
      case 'browser_state': {
        set((st) => {
          const a = st.sessions[msg.sessionId];
          if (!a) return st;
          // Only an actual grab acknowledges the attention item. Re-subscribing
          // while changing panes can refresh agent ownership and must not dismiss
          // a takeover the user has not acted on. The daemon keeps the tool call
          // pending until release, when takeover_resolved clears it durably.
          const userJustGrabbed = msg.controlOwner === 'user' && a.browserOwner !== 'user';
          const takeoverGrab = userJustGrabbed && a.takeovers.length > 0;
          const takeovers = takeoverGrab ? [] : a.takeovers;
          const browserTakeoverHeld = msg.controlOwner === 'user' && (a.browserTakeoverHeld || takeoverGrab);
          return {
            sessions: {
              ...st.sessions,
              [msg.sessionId]: { ...a, browserActive: msg.active, browserOwner: msg.controlOwner, browserTakeoverHeld, takeovers },
            },
          };
        });
        return;
      }
      case 'ack': {
        if (msg.corrId && pendingAcks.has(msg.corrId)) {
          pendingAcks.get(msg.corrId)!({
            sessionId: msg.sessionId,
            error: msg.error,
            promptId: msg.promptId,
            disposition: msg.disposition,
            position: msg.position,
            cleared: msg.cleared,
          });
          pendingAcks.delete(msg.corrId);
        }
        return;
      }
      case 'agent_closed': {
        set((st) => {
          if (!st.sessions[msg.sessionId]) return st;
          const sessions = { ...st.sessions };
          delete sessions[msg.sessionId];
          const order = st.order.filter((id) => id !== msg.sessionId);
          const focusedId = st.focusedId === msg.sessionId ? order[0] ?? null : st.focusedId;
          const pane = focusedId ? st.panesBySession[focusedId] ?? 'chat' : 'chat';
          if (focusedId !== st.focusedId) saveFocusedSession(focusedId);
          const annotations = { ...st.annotations };
          delete annotations[msg.sessionId];
          return { sessions, order, focusedId, pane, annotations };
        });
        ptyHub.clear(msg.sessionId);
        shellHub.clear(msg.sessionId);
        browserHub.clear(msg.sessionId);
        // Tear the engine's playlist down if it was this (now-closed) chat's
        // — otherwise a silence keepalive (or pinned audio focus) could keep
        // running for a chat that no longer exists.
        if (getEngineState().sessionId === msg.sessionId) setEnginePlaylist(msg.sessionId, []);
        return;
      }
      case 'snapshot': {
        set((st) => {
          const sessions = { ...st.sessions };
          const prev = sessions[msg.sessionId] ?? shell(msg.sessionId);
          // raw_pty (agent CLI) and shell_pty/shell_exit (user Terminal shell)
          // are byte/lifecycle streams routed to their hubs, not the transcript.
          const transcript = msg.transcript.filter(
            (e) => e.event.kind !== 'raw_pty' && e.event.kind !== 'shell_pty' && e.event.kind !== 'shell_exit',
          );
          // Feed any pty frames in the snapshot into the terminal hubs (rehydrate).
          for (const e of msg.transcript) {
            if (e.event.kind === 'raw_pty') ptyHub.push(msg.sessionId, e.event.dataB64);
            else if (e.event.kind === 'shell_pty') shellHub.push(msg.sessionId, e.event.dataB64);
          }
          // The shell is exited iff its last lifecycle event is shell_exit (a
          // restart appends fresh shell_pty after it).
          const lastShell = [...msg.transcript].reverse().find((e) => e.event.kind === 'shell_pty' || e.event.kind === 'shell_exit');
          const shellExited = !!lastShell && lastShell.event.kind === 'shell_exit';
          const shellExitMessage = shellExited && lastShell.event.kind === 'shell_exit' ? lastShell.event.message : null;
          // session_config/available_commands aren't top-level snapshot fields
          // (unlike status/pendingApprovals) — fold the latest one out of the
          // replayed transcript, mirroring the live 'event' path's applyEventToView.
          const lastConfig = [...transcript].reverse().find((e) => e.event.kind === 'session_config');
          const lastCommands = [...transcript].reverse().find((e) => e.event.kind === 'available_commands');
          const lastPromptCapabilities = [...transcript].reverse().find((e) => e.event.kind === 'prompt_capabilities');
          const lastAsideCapabilities = [...transcript].reverse().find((e) => e.event.kind === 'aside_capabilities');
          const lastSteeringCapabilities = [...transcript].reverse().find((e) => e.event.kind === 'steering_capabilities');
          const lastUsage = [...transcript].reverse().find((e) => e.event.kind === 'usage');
          const audioEvents = transcript.filter((e) => e.event.kind === 'audio_preference' || e.event.kind === 'audio_state');
          let audioOnTurnEnd = prev.audioOnTurnEnd;
          let audioState = prev.audioState;
          let audioError = prev.audioError;
          let audioSeq = prev.audioSeq;
          for (const entry of audioEvents) {
            if (entry.event.kind === 'audio_preference') audioOnTurnEnd = entry.event.enabled;
            if (entry.event.kind === 'audio_state') {
              audioState = entry.event.state;
              audioError = entry.event.state === 'error' ? entry.event.message ?? 'Speech rendering failed' : null;
              audioSeq = entry.event.seq;
            }
          }
          // Takeovers are daemon-owned durable events rather than part of the
          // session snapshot fields. Rebuild the pending set from the complete
          // transcript so reconnecting cannot erase a prompt while its MCP call
          // remains blocked.
          const replayedTakeovers: Takeover[] = [];
          for (const entry of transcript) {
            const event = entry.event;
            if (event.kind === 'takeover_request' && !replayedTakeovers.some((t) => t.reqId === event.reqId)) {
              replayedTakeovers.push({ reqId: event.reqId, reason: event.reason });
            } else if (event.kind === 'takeover_resolved') {
              const index = replayedTakeovers.findIndex((t) => t.reqId === event.reqId);
              if (index !== -1) replayedTakeovers.splice(index, 1);
            }
          }
          sessions[msg.sessionId] = {
            ...prev,
            status: msg.status,
            controlMode: msg.controlMode,
            pendingApprovals: msg.pendingApprovals,
            queuedPrompts: msg.queuedPrompts,
            events: transcript,
            lastSeq: msg.seq,
            sessionConfig:
              lastConfig && lastConfig.event.kind === 'session_config'
                ? { modes: lastConfig.event.modes, configOptions: lastConfig.event.configOptions }
                : prev.sessionConfig,
            commands: lastCommands && lastCommands.event.kind === 'available_commands' ? lastCommands.event.commands : prev.commands,
            imagePromptSupport:
              lastPromptCapabilities && lastPromptCapabilities.event.kind === 'prompt_capabilities'
                ? lastPromptCapabilities.event.image
                : prev.imagePromptSupport,
            asideSupport:
              lastAsideCapabilities && lastAsideCapabilities.event.kind === 'aside_capabilities'
                ? lastAsideCapabilities.event.fork
                : prev.asideSupport,
            steeringSupport:
              lastSteeringCapabilities && lastSteeringCapabilities.event.kind === 'steering_capabilities'
                ? lastSteeringCapabilities.event.supported
                : prev.steeringSupport,
            usage:
              lastUsage && lastUsage.event.kind === 'usage'
                ? { used: lastUsage.event.used, size: lastUsage.event.size, cost: lastUsage.event.cost, updatedAt: usageUpdatedAt(lastUsage.event) }
                : prev.usage,
            // Pane-change subscriptions replay the still-pending request while
            // the user holds the wheel. Keep it acknowledged in that case.
            takeovers: prev.browserOwner === 'user' && prev.browserTakeoverHeld ? [] : replayedTakeovers,
            hasPty: prev.hasPty || msg.transcript.some((e) => e.event.kind === 'raw_pty'),
            shellExited,
            shellExitMessage,
            audioOnTurnEnd,
            audioState,
            audioError,
            audioSeq,
            audioReadySeqs: msg.audioReadySeqs ?? [],
            audioDurations: Object.fromEntries((msg.audioReady ?? []).map((clip) => [clip.seq, clip.durationMs])),
            audioPosition: msg.audioPosition ?? null,
          };
          const order = st.order.includes(msg.sessionId) ? st.order : [...st.order, msg.sessionId];
          return {
            sessions,
            order,
            focusedId: st.focusedId ?? msg.sessionId,
            annotations: { ...st.annotations, [msg.sessionId]: msg.annotations ?? [] },
          };
        });
        syncAudioFocus();
        return;
      }
      case 'event': {
        const { sessionId, seq, event } = msg;
        if (event.kind === 'raw_pty') {
          ptyHub.push(sessionId, event.dataB64);
          set((st) => {
            const a = st.sessions[sessionId];
            if (!a || (a.hasPty && seq <= a.lastSeq)) return st;
            return { sessions: { ...st.sessions, [sessionId]: { ...a, hasPty: true, lastSeq: Math.max(a.lastSeq, seq) } } };
          });
          return;
        }
        if (event.kind === 'shell_pty') {
          shellHub.push(sessionId, event.dataB64);
          set((st) => {
            const a = st.sessions[sessionId];
            if (!a) return st;
            // Live output implies a running shell; clear any stale exited flag.
            if (!a.shellExited && seq <= a.lastSeq) return st;
            return { sessions: { ...st.sessions, [sessionId]: { ...a, shellExited: false, shellExitMessage: null, lastSeq: Math.max(a.lastSeq, seq) } } };
          });
          return;
        }
        if (event.kind === 'shell_exit') {
          const message = event.message;
          set((st) => {
            const a = st.sessions[sessionId];
            if (!a) return st;
            return { sessions: { ...st.sessions, [sessionId]: { ...a, shellExited: true, shellExitMessage: message, lastSeq: Math.max(a.lastSeq, seq) } } };
          });
          return;
        }
        set((st) => {
          const a = st.sessions[sessionId] ?? shell(sessionId);
          if (seq <= a.lastSeq && st.sessions[sessionId]) return st; // already applied (dedupe)
          const prevStatus = a.status;
          const next: SessionView = { ...a, events: [...a.events, { seq, event }], lastSeq: Math.max(a.lastSeq, seq) };
          applyEventToView(next, event);
          if (event.kind === 'audio_state' && event.state === 'ready') {
            next.audioReadySeqs = [...new Set([...next.audioReadySeqs, event.seq])].sort((a, b) => a - b);
            if (event.durationMs != null) {
              next.audioDurations = { ...next.audioDurations, [event.seq]: event.durationMs };
            }
            next.audioReadyRevision++;
          }
          // Raise at most one completed-turn notification for each background
          // agent. Only live events create these browser-local notifications;
          // snapshot replay never resurrects ones the user has already read.
          // The focused thread is already visible, so it does not need a card.
          const severity = turnNotificationSeverity(prevStatus, next.status);
          if (severity && st.focusedId !== sessionId) {
            next.turnNotifications = [{ seq, createdAt: Date.now(), severity }];
          }
          const sessions = { ...st.sessions, [sessionId]: next };
          const order = st.order.includes(sessionId) ? st.order : [...st.order, sessionId];
          return { sessions, order };
        });
        return;
      }
      case 'prompt_queue':
        set((st) => {
          const agent = st.sessions[msg.sessionId];
          if (!agent) return st;
          return { sessions: { ...st.sessions, [msg.sessionId]: { ...agent, queuedPrompts: msg.queuedPrompts } } };
        });
        return;
      case 'annotations':
        set((st) => ({ annotations: { ...st.annotations, [msg.sessionId]: msg.annotations } }));
        return;
      case 'audio_position':
        // Cross-device sync only: the daemon never echoes this back to the
        // connection that sent set_audio_position, so this only ever reflects
        // another client's playback moving the shared position.
        set((st) => {
          const agent = st.sessions[msg.sessionId];
          if (!agent) return st;
          const audioPosition = msg.seq === 0 ? null : { seq: msg.seq, positionMs: msg.positionMs, updatedAt: msg.updatedAt };
          return { sessions: { ...st.sessions, [msg.sessionId]: { ...agent, audioPosition } } };
        });
        return;
    }
  };
  applyServerMsg = apply;

  client = new WsClient({
    onMessage: apply,
    onState: (conn) => set({ conn }),
    onOpen: () => {
      // The daemon drops connection-scoped audio focus on a socket close, so
      // always re-announce the active chat after reconnecting.
      audioFocusAgent = null;
      syncAudioFocus();
      // Rediscover sessions (and their metadata) and re-subscribe with sinceSeq.
      client.send({ t: 'list_agents' });
      client.send({ t: 'list_agent_catalog' });
      client.send({ t: 'list_hosts' });
      client.send({ t: 'list_system_notifications' });
      // Also re-subscribe to anything we already track, immediately (idempotent).
      for (const id of get().order) subscribeAgent(id);
    },
  });

  return {
    conn: 'connecting',
    theme: initialTheme(),
    sessions: {},
    order: initialSessionOrder(),
    focusedId: initialFocusedSession(),
    panesBySession: initialSessionPanes(),
    pane: 'chat',
    modal: 'none',
    spawnHandoffFrom: null,
    inspectorOpen: false,
    dirs: [],
    agentCatalog: null,
    hosts: normalizedHosts([]),
    dirsByHost: {},
    agentCatalogByHost: {},
    snapshots: [],
    automationJobs: [],
    automationRuns: [],
    automationLoading: false,
    automationError: null,
    resumeCatalog: null,
    resumeCatalogByHost: {},
    resumePendingHostIds: {},
    resumeLoading: false,
    drafts: initialDrafts(),
    annotations: {},
    browserSubAgent: null,
	sessionsRailCollapsed: isNarrowViewport(),
    approvalsRailCollapsed: isNarrowViewport(),
    systemNotifications: [],

    boot: () => {
      const tok = resolveToken();
      client.start(tok);
      // Adopt a token pasted into the URL fragment AFTER boot — e.g. opening a
      // fresh bootstrap URL (#t=…) in the already-open tab after the daemon
      // restarted with a new token. That's a same-document hash change, so the
      // SPA never re-runs boot(); without this the tab stays stuck on the old
      // (now-rejected) token until a manual reload.
      if (!hashListenerAttached) {
        hashListenerAttached = true;
        window.addEventListener('hashchange', () => {
          const next = resolveToken();
          if (next) client.setToken(next);
        });
      }
      if (!gitRefreshListenersAttached) {
        gitRefreshListenersAttached = true;
        const refreshVisibleAgents = () => {
          if (document.visibilityState === 'visible' && get().conn === 'connected') {
            client.send({ t: 'list_agents' });
          }
        };
        document.addEventListener('visibilitychange', refreshVisibleAgents);
        document.addEventListener('visibilitychange', syncAudioFocus);
        window.addEventListener('focus', refreshVisibleAgents);
        setInterval(refreshVisibleAgents, GIT_REFRESH_INTERVAL_MS);
        // A frozen (screen-off) page's WS can silently die with the client
        // unaware — refreshVisibleAgents above only re-fetches state on a
        // healthy connection, it doesn't detect/fix a dead one. Force a fast
        // reconnect on both signals the platform gives us for "the page just
        // came back": visibilitychange -> visible, and pageshow (notably
        // fired on iOS's back-forward-cache restore, which visibilitychange
        // alone can miss).
        const wake = () => { if (document.visibilityState === 'visible') client.wake(); };
        document.addEventListener('visibilitychange', wake);
        window.addEventListener('pageshow', wake);
      }
    },
    submitToken: (t) => client.setToken(t.trim()),
    // Focusing an agent acknowledges its completed-turn notifications, whether
    // the user came from the left rail or the notifications rail.
    focus: (id) => {
      const previous = get().focusedId;
      const previousHadPty = previous ? wantsPty(previous) : false;
      set((st) => {
        const agent = st.sessions[id];
        const pane = st.panesBySession[id] ?? 'chat';
        if (!agent || agent.turnNotifications.length === 0) return { focusedId: id, pane };
        return { focusedId: id, pane, sessions: { ...st.sessions, [id]: { ...agent, turnNotifications: [] } } };
      });
      if (get().sessions[id]) saveFocusedSession(id);
      syncAudioFocus();
      if (previous && previous !== id && previousHadPty) subscribeAgent(previous);
      if (wantsPty(id)) replayPtyFor(id);
    },
    reorderAgent: (id, targetId, after) => {
      const order = [...get().order];
      const from = order.indexOf(id);
      const target = order.indexOf(targetId);
      if (from < 0 || target < 0 || from === target) return;
      order.splice(from, 1);
      const nextTarget = order.indexOf(targetId);
      order.splice(nextTarget + (after ? 1 : 0), 0, id);
      saveSessionOrder(order);
      set({ order });
    },
    // A user can restore the completed-turn badge after acknowledging it. This
    // is deliberately browser-local, like notifications created from live
    // status transitions, and is idempotent while the agent is already unread.
    markAgentUnread: (id) => set((st) => {
      const agent = st.sessions[id];
      if (!agent || agent.turnNotifications.length > 0) return st;
      const notification: TurnNotification = {
        seq: agent.lastSeq,
        createdAt: Date.now(),
        severity: 'success',
      };
      return {
        sessions: {
          ...st.sessions,
          [id]: { ...agent, turnNotifications: [...agent.turnNotifications, notification] },
        },
      };
    }),
    // Selecting Terminal is view-only until its shroud's explicit Take control
    // action calls enterTerminal. Even an idle ACP session must never be swapped
    // merely because the user inspected the tab.
    setPane: (p) => {
      if (get().pane === p) return;
      const id = get().focusedId;
      const hadPty = id ? wantsPty(id) : false;
      if (id) {
        set((st) => {
          const panesBySession = { ...st.panesBySession, [id]: p };
          saveSessionPanes(panesBySession);
          return { pane: p, panesBySession };
        });
      } else {
        set({ pane: p });
      }
      syncAudioFocus();
      if (!id) return;
      if (wantsPty(id) && !hadPty) replayPtyFor(id);
      else if (hadPty && !wantsPty(id)) subscribeAgent(id);
    },
    toggleTheme: () =>
      set((st) => {
        const theme = st.theme === 'dark' ? 'light' : 'dark';
        localStorage.setItem('tandem.theme', theme);
        return { theme };
      }),
	toggleSessionsRail: () => set((st) => ({ sessionsRailCollapsed: !st.sessionsRailCollapsed })),
    toggleApprovalsRail: () => set((st) => ({ approvalsRailCollapsed: !st.approvalsRailCollapsed })),
    setModal: (m) => {
      if (m === 'spawn') {
        get().refreshDirs();
        client.send({ t: 'list_agent_catalog' });
        get().refreshHosts();
      }
      if (m === 'resume') get().refreshSessions();
      if (m === 'automation') void get().refreshAutomation().catch(() => undefined);
      set({ modal: m, spawnHandoffFrom: null });
    },
    handOffAgent: (sessionId) => {
      get().setModal('spawn');
      set({ spawnHandoffFrom: sessionId });
    },
    toggleInspector: () => set((st) => ({ inspectorOpen: !st.inspectorOpen })),
    toggleThreadAudio: (sessionId) => {
      const agent = get().sessions[sessionId];
      if (agent) client.send({ t: 'set_audio_enabled', sessionId, enabled: !agent.audioOnTurnEnd });
    },
    setAudioPosition: (sessionId, seq, positionMs) => client.send({ t: 'set_audio_position', sessionId, seq, positionMs }),
    refreshDirs: () => client.send({ t: 'list_dirs' }),
    refreshHostDirs: (hostId) => client.send(isLocalHost(hostId) ? { t: 'list_dirs' } : { t: 'list_dirs', hostId }),
    refreshHosts: () => client.send({ t: 'list_hosts' }),
    refreshAgentCatalog: (hostId) => client.send(isLocalHost(hostId) ? { t: 'list_agent_catalog' } : { t: 'list_agent_catalog', hostId }),
    refreshAgents: () => client.send({ t: 'list_agents' }),
    refreshSessions: (hostId) => {
      const targets = get().hosts.filter((host) => (hostId ? host.id === hostId : host.local || host.status === 'connected' || host.status === 'accepted'));
      set({
        resumeLoading: targets.length > 0,
        resumeCatalogByHost: {},
        resumeCatalog: null,
        resumePendingHostIds: Object.fromEntries(targets.map((host) => [host.id, true])),
      });
      for (const host of targets) client.send(isLocalHost(host.id) ? { t: 'list_sessions' } : { t: 'list_sessions', hostId: host.id });
    },
    refreshAutomation: (repositoryId) =>
      new Promise<void>((resolve, reject) => {
        const corrId = nextCorr();
        pendingAutomation.set(corrId, { resolve, reject });
        set({ automationLoading: true, automationError: null });
        client.send({ t: 'list_automation', repositoryId, corrId });
      }),
    setAutomationEnabled: (id, enabled) =>
      new Promise<void>((resolve, reject) => {
        const corrId = nextCorr();
        pendingAutomation.set(corrId, { resolve, reject });
        set({ automationLoading: true, automationError: null });
        client.send({ t: 'set_automation_enabled', id, enabled, corrId });
      }),
    searchSessions: (query, hostId) => {
      const targets = get().hosts.filter((host) => (hostId ? host.id === hostId : host.local || host.status === 'connected' || host.status === 'accepted'));
      return Promise.all(targets.map((host) => new Promise<SessionSearchResult[]>((resolve, reject) => {
        const corrId = nextCorr();
        pendingSessionSearches.set(corrId, {
          resolve: (results) => resolve(isLocalHost(host.id) ? results : results.map((result) => ({
            ...result,
            session: { ...result.session, hostId: host.id, hostName: host.name ?? host.id },
          }))),
          reject,
        });
        client.send(isLocalHost(host.id)
          ? { t: 'search_sessions', query, limit: 30, maxHitsPerSession: 3, corrId }
          : { t: 'search_sessions', query, limit: 30, maxHitsPerSession: 3, hostId: host.id, corrId });
      }))).then((groups) => groups.flat());
    },
    resumeSession: (session) =>
      new Promise<AckResult>((resolve) => {
        const corrId = nextCorr();
        pendingAcks.set(corrId, (r) => {
          if (r.sessionId && !r.error) {
            get().refreshAgents();
            set({ focusedId: r.sessionId, modal: 'none', pane: 'chat' });
          }
          resolve(r);
        });
        client.send({
          t: 'resume_session',
		  externalSessionId: session.externalSessionId,
          source: session.source,
          agent: session.agent,
          cwd: session.cwd || undefined,
          ...(isLocalHost(session.hostId) ? {} : { hostId: session.hostId }),
          corrId,
        });
      }),
    enterTerminal: (sessionId, interrupt = false) =>
      new Promise<AckResult>((resolve) => {
        const corrId = nextCorr();
        pendingAcks.set(corrId, (result) => {
          // The CLI switch is an explicit request to view terminal output. It
          // may emit control_state before its ack, so opt into PTY replay here
          // rather than waiting for a later pane change.
          if (!result.error) replayPtyFor(sessionId);
          resolve(result);
        });
        client.send({ t: 'enter_terminal', sessionId, interrupt, corrId });
      }),
    leaveTerminal: (sessionId) =>
      new Promise<AckResult>((resolve) => {
        const corrId = nextCorr();
        pendingAcks.set(corrId, (result) => {
          if (!result.error && !wantsPty(sessionId)) subscribeAgent(sessionId);
          resolve(result);
        });
        client.send({ t: 'leave_terminal', sessionId, corrId });
      }),
    openShell: (sessionId, cols, rows) =>
      new Promise<AckResult>((resolve) => {
        const corrId = nextCorr();
        pendingAcks.set(corrId, resolve);
        client.send({ t: 'shell_open', sessionId, cols, rows, corrId });
      }),
    restartShell: (sessionId, cols, rows) => {
      // Clear the exited flag + prior scrollback so the fresh shell starts clean.
      shellHub.reset(sessionId);
      set((st) => {
        const a = st.sessions[sessionId];
        if (!a) return st;
        return { sessions: { ...st.sessions, [sessionId]: { ...a, shellExited: false, shellExitMessage: null } } };
      });
      return new Promise<AckResult>((resolve) => {
        const corrId = nextCorr();
        pendingAcks.set(corrId, resolve);
        client.send({ t: 'shell_open', sessionId, cols, rows, corrId });
      });
    },
    spawn: (spec) =>
      new Promise<AckResult>((resolve) => {
        const corrId = nextCorr();
        pendingAcks.set(corrId, (r) => {
          if (r.sessionId && !r.error) {
            get().refreshAgents();
            set({ focusedId: r.sessionId, modal: 'none', pane: 'chat' });
          }
          resolve(r);
        });
        client.send({ t: 'spawn_agent', spec, corrId });
      }),
    actOnSystemNotification: (notificationId, action) =>
      new Promise<AckResult>((resolve) => {
        const corrId = nextCorr();
        pendingAcks.set(corrId, (result) => {
          if (result.sessionId && !result.error) {
            get().refreshAgents();
            set({ focusedId: result.sessionId, pane: 'chat' });
          }
          resolve(result);
        });
        client.send({ t: 'system_notification_action', notificationId, action, corrId });
      }),
    getSpawnOptions: (agent, cwd, harness, hostId) =>
      new Promise<SpawnOptions>((resolve, reject) => {
        const corrId = nextCorr();
        pendingSpawnOptions.set(corrId, { resolve, reject });
        client.send(isLocalHost(hostId)
          ? { t: 'get_spawn_options', agent, harness, cwd, corrId }
          : { t: 'get_spawn_options', agent, harness, cwd, hostId, corrId });
      }),
    listGitRefs: (repo, hostId) =>
      new Promise<GitRefInfo[]>((resolve, reject) => {
        const corrId = nextCorr();
        pendingGitRefs.set(corrId, { resolve, reject });
        client.send(isLocalHost(hostId)
          ? { t: 'list_git_refs', repo, corrId }
          : { t: 'list_git_refs', repo, hostId, corrId });
      }),
    listWorkspaceEntries: (sessionId, path) =>
      new Promise<WorkspaceEntry[]>((resolve, reject) => {
        const corrId = nextCorr();
        pendingWorkspaceEntries.set(corrId, { resolve, reject });
        client.send({ t: 'list_workspace_entries', sessionId, path, corrId });
      }),
    captureSnapshot: (sessionId, name) =>
      new Promise<BrowserSnapshot[]>((resolve, reject) => {
        const corrId = nextCorr();
        pendingSnapshots.set(corrId, { resolve, reject });
        client.send({ t: 'capture_snapshot', sessionId, name, corrId });
      }),
    listSnapshots: () =>
      new Promise<BrowserSnapshot[]>((resolve, reject) => {
        const corrId = nextCorr();
        pendingSnapshots.set(corrId, { resolve, reject });
        client.send({ t: 'list_snapshots', corrId });
      }),
    deleteSnapshot: (id) =>
      new Promise<BrowserSnapshot[]>((resolve, reject) => {
        const corrId = nextCorr();
        pendingSnapshots.set(corrId, { resolve, reject });
        client.send({ t: 'delete_snapshot', id, corrId });
      }),
    listProfiles: (project) =>
      new Promise<{ profiles: Profile[]; recent: string[] }>((resolve, reject) => {
        const corrId = nextCorr();
        pendingProfiles.set(corrId, { resolve, reject });
        client.send({ t: 'list_profiles', project, corrId });
      }),
    renameProfile: (id, name, project) =>
      new Promise<{ profiles: Profile[]; recent: string[] }>((resolve, reject) => {
        const corrId = nextCorr();
        pendingProfiles.set(corrId, { resolve, reject });
        client.send({ t: 'rename_profile', id, name, project, corrId });
      }),
    renameAgent: (sessionId, name) =>
      new Promise<AckResult>((resolve) => {
        const corrId = nextCorr();
        pendingAcks.set(corrId, (result) => {
          if (!result.error) get().refreshAgents();
          resolve(result);
        });
        client.send({ t: 'rename_agent', sessionId, name, corrId });
      }),
    deleteProfile: (id, project) =>
      new Promise<{ profiles: Profile[]; recent: string[] }>((resolve, reject) => {
        const corrId = nextCorr();
        pendingProfiles.set(corrId, { resolve, reject });
        client.send({ t: 'delete_profile', id, project, corrId });
      }),
    prompt: (sessionId, input) =>
      new Promise<AckResult>((resolve) => {
        const corrId = nextCorr();
        pendingAcks.set(corrId, (result) => {
          if (!result.error) set((st) => ({ drafts: { ...st.drafts, [sessionId]: '' } }));
          resolve(result);
        });
        client.send(typeof input === 'string'
          ? { t: 'prompt', sessionId, text: input, corrId }
          : { t: 'prompt', sessionId, blocks: input, corrId });
      }),
    steer: (sessionId, input) =>
      new Promise<AckResult>((resolve) => {
        const corrId = nextCorr();
        pendingAcks.set(corrId, (result) => {
          if (!result.error) set((st) => ({ drafts: { ...st.drafts, [sessionId]: '' } }));
          resolve(result);
        });
        client.send(typeof input === 'string'
          ? { t: 'steer', sessionId, text: input, corrId }
          : { t: 'steer', sessionId, blocks: input, corrId });
      }),
    aside: (sessionId, question) =>
      new Promise<AckResult>((resolve) => {
        const corrId = nextCorr();
        pendingAcks.set(corrId, (result) => {
          if (!result.error) set((st) => ({ drafts: { ...st.drafts, [sessionId]: '' } }));
          resolve(result);
        });
        client.send({ t: 'aside', sessionId, text: question, corrId });
      }),
    removeQueuedPrompt: (sessionId, promptId) =>
      new Promise<AckResult>((resolve) => {
        const corrId = nextCorr();
        pendingAcks.set(corrId, resolve);
        client.send({ t: 'remove_queued_prompt', sessionId, promptId, corrId });
      }),
    clearPromptQueue: (sessionId) =>
      new Promise<AckResult>((resolve) => {
        const corrId = nextCorr();
        pendingAcks.set(corrId, resolve);
        client.send({ t: 'clear_prompt_queue', sessionId, corrId });
      }),
    interruptAndClearQueue: (sessionId) =>
      new Promise<AckResult>((resolve) => {
        const corrId = nextCorr();
        pendingAcks.set(corrId, resolve);
        client.send({ t: 'interrupt_and_clear_queue', sessionId, corrId });
      }),
    // Annotations are daemon-authoritative: these actions only send the WS
    // message and resolve the ack. Local `annotations` state updates only via
    // the `annotations` broadcast (and `snapshot` hydration) above.
    addAnnotation: (sessionId, anchor, comment) =>
      new Promise<AckResult>((resolve) => {
        const corrId = nextCorr();
        pendingAcks.set(corrId, resolve);
        client.send({ t: 'add_annotation', sessionId, seq: anchor.seq, role: anchor.role, quote: anchor.quote, comment, corrId });
      }),
    updateAnnotation: (sessionId, id, comment) =>
      new Promise<AckResult>((resolve) => {
        const corrId = nextCorr();
        pendingAcks.set(corrId, resolve);
        client.send({ t: 'update_annotation', sessionId, id, comment, corrId });
      }),
    removeAnnotation: (sessionId, id) =>
      new Promise<AckResult>((resolve) => {
        const corrId = nextCorr();
        pendingAcks.set(corrId, resolve);
        client.send({ t: 'delete_annotation', sessionId, id, corrId });
      }),
    clearAnnotations: (sessionId) =>
      new Promise<AckResult>((resolve) => {
        const corrId = nextCorr();
        pendingAcks.set(corrId, resolve);
        client.send({ t: 'clear_annotations', sessionId, corrId });
      }),
    setDraft: (sessionId, text) => set((st) => ({ drafts: { ...st.drafts, [sessionId]: text } })),
    interrupt: (sessionId) => client.send({ t: 'interrupt', sessionId }),
    respond: (sessionId, reqId, optionId) => {
      // Optimistically drop the approval so the rail feels instant; the daemon
      // confirms via a status/event stream.
      set((st) => {
        const a = st.sessions[sessionId];
        if (!a) return st;
        return { sessions: { ...st.sessions, [sessionId]: { ...a, pendingApprovals: a.pendingApprovals.filter((p) => p.reqId !== reqId) } } };
      });
      client.send({ t: 'permission_response', sessionId, reqId, optionId });
    },
    setMode: (sessionId, modeId) => client.send({ t: 'set_mode', sessionId, modeId }),
    setConfigOption: (sessionId, configId, value) => client.send({ t: 'set_config_option', sessionId, configId, value }),
    getClosePreview: (sessionId) =>
      new Promise<ClosePreview>((resolve, reject) => {
        const corrId = nextCorr();
        pendingClosePreviews.set(corrId, { resolve, reject });
        client.send({ t: 'get_close_preview', sessionId, corrId });
      }),
    getDiff: (sessionId) =>
      new Promise<WorkspaceDiff>((resolve, reject) => {
        const corrId = nextCorr();
        pendingDiffs.set(corrId, { resolve, reject });
        client.send({ t: 'get_diff', sessionId, corrId });
      }),
    closeAgent: (sessionId, force, deleteWorktree, deinitSubmodules) =>
      new Promise<AckResult>((resolve) => {
        const corrId = nextCorr();
        pendingAcks.set(corrId, resolve);
        client.send({ t: 'close_agent', sessionId, force, deleteWorktree, deinitSubmodules, corrId });
      }),
    send: (m) => client.send(m),
    // Opt an agent's browser channel in/out (screencast focus rule). Re-subscribe
    // both the previously- and newly-viewing sessions so the daemon starts/stops the
    // screencast accordingly.
    setBrowserSub: (sessionId) => {
      const prev = get().browserSubAgent;
      if (prev === sessionId) return;
      set({ browserSubAgent: sessionId });
      if (prev && get().sessions[prev]) subscribeAgent(prev);
      if (sessionId && get().sessions[sessionId]) subscribeAgent(sessionId);
    },
    browserControl: (sessionId, action) => client.send({ t: 'browser_control', sessionId, action }),
    restartBrowser: (sessionId, snapshotId) =>
      new Promise<AckResult>((resolve) => {
        const corrId = nextCorr();
        pendingAcks.set(corrId, resolve);
        client.send({ t: 'restart_browser', sessionId, snapshotId, corrId });
      }),
    restartHarness: (sessionId) =>
      new Promise<AckResult>((resolve) => {
        const corrId = nextCorr();
        pendingAcks.set(corrId, (result) => {
          if (!result.error) get().refreshAgents();
          resolve(result);
        });
        client.send({ t: 'restart_harness', sessionId, corrId });
      }),
    browserInput: (sessionId, event) => client.send({ t: 'browser_input', sessionId, event }),
    toggleWheel: (sessionId) => {
      const a = get().sessions[sessionId];
      if (!a || !a.browserActive) return;
      client.send({ t: 'browser_control', sessionId, action: a.browserOwner === 'user' ? 'release' : 'grab' });
    },
    nav: (dir) => {
      const previous = get().focusedId;
      const previousHadPty = previous ? wantsPty(previous) : false;
      set((st) => {
        const ranked = rankSessions(st.sessions, st.order);
        if (ranked.length === 0) return st;
        const i = st.focusedId ? ranked.indexOf(st.focusedId) : -1;
        const next = ranked[(i + dir + ranked.length) % ranked.length];
        return { focusedId: next };
      });
      const next = get().focusedId;
      if (previous && previous !== next && previousHadPty) subscribeAgent(previous);
      if (next && wantsPty(next)) replayPtyFor(next);
      syncAudioFocus();
    },
  };
});

// Some focus changes are part of higher-level actions (spawn, resume, and
// keyboard navigation) rather than `focus()`. Subscribe once at the store
// boundary so persistence cannot depend on which action made the mutation.
// Do the same for drafts: every keystroke reaches localStorage synchronously,
// before a refresh or daemon restart can discard the browser state.
useStore.subscribe((state, previous) => {
  if (state.focusedId !== previous.focusedId) saveFocusedSession(state.focusedId);
  if (state.drafts !== previous.drafts) saveDrafts(state.drafts);
});

// Derived selector: rail order with blocked/error floated to the top.
export function rankedOrder(st: StoreState): string[] {
  return rankSessions(st.sessions, st.order);
}
// All pending approvals across all sessions (for the right rail), urgent first.
export function allApprovals(st: StoreState): { sessionId: string; approval: Approval }[] {
  const out: { sessionId: string; approval: Approval }[] = [];
  for (const id of rankSessions(st.sessions, st.order)) {
    const a = st.sessions[id];
    for (const ap of a.pendingApprovals) out.push({ sessionId: id, approval: ap });
  }
  return out;
}

// Completed turns waiting to be read, newest first within each agent.
export function allTurnNotifications(st: StoreState): { sessionId: string; notification: TurnNotification }[] {
  const out: { sessionId: string; notification: TurnNotification }[] = [];
  for (const id of rankSessions(st.sessions, st.order)) {
    const agent = st.sessions[id];
    for (const notification of agent.turnNotifications) out.push({ sessionId: id, notification });
  }
  return out.sort((a, b) => b.notification.createdAt - a.notification.createdAt);
}

// Notifications-panel badge summary: total items across every agent (unread
// turns + approvals + takeovers) and the highest severity among them.
export function notificationsSummary(st: StoreState): { total: number; severity: NotifSeverity | null } {
  const severities: NotifSeverity[] = st.systemNotifications.map((n) => n.severity);
  let total = st.systemNotifications.length;
  for (const id of Object.keys(st.sessions)) {
    const badge = agentBadge(st.sessions[id]);
    total += badge.count;
    if (badge.severity) severities.push(badge.severity);
  }
  return { total, severity: maxSeverity(severities) };
}

function shell(id: string): SessionView {
  return {
    id,
    name: id,
    workspace: { kind: 'worktree', repo: '', repoPath: '', branch: '', cwd: '' },
    status: 'idle',
    events: [],
    lastSeq: 0,
    pendingApprovals: [],
    turnNotifications: [],
    hasPty: false,
    shellExited: false,
    shellExitMessage: null,
    browserActive: false,
    browserOwner: 'agent',
    browserTakeoverHeld: false,
    takeovers: [],
    sessionConfig: null,
    usage: null,
    commands: [],
    imagePromptSupport: null,
    asideSupport: null,
    steeringSupport: null,
    queuedPrompts: [],
    controlMode: 'transcript',
    adapter: 'acp',
    canHandoff: false,
    audioOnTurnEnd: false,
    audioState: 'idle',
    audioError: null,
    audioSeq: null,
    audioReadySeqs: [],
    audioDurations: {},
    audioReadyRevision: 0,
    audioPosition: null,
  };
}

function mergeSummary(prev: SessionView | undefined, s: SessionSummary): SessionView {
  const base = prev ?? shell(s.id);
  return { ...base, name: s.name, agent: s.agent, hostId: s.hostId, hostName: s.hostName, profile: s.profile, workspace: s.workspace, status: s.status, controlMode: s.controlMode, adapter: s.adapter, canHandoff: s.canHandoff };
}

// Fold status/permission side effects of an event into the view (mirrors the
// daemon's session-side handling so the rail stays correct between snapshots).
// The daemon stamps usage events with the moment the reported totals last
// changed and persists that with the event, so age survives reconnects, page
// loads, and other browsers. Pre-updatedAt events fall back to arrival time.
function usageUpdatedAt(event: Extract<WireEvent, { kind: 'usage' }>): number {
  return typeof event.updatedAt === 'number' && event.updatedAt > 0 ? event.updatedAt : Date.now();
}

function applyEventToView(v: SessionView, event: WireEvent): void {
  if (event.kind === 'status') v.status = event.status;
  if (event.kind === 'permission_request') {
    v.status = 'blocked';
    if (!v.pendingApprovals.some((p) => p.reqId === event.reqId)) {
      v.pendingApprovals = [...v.pendingApprovals, { reqId: event.reqId, toolCallId: event.toolCallId, title: event.title, options: event.options }];
    }
  }
  if (event.kind === 'error') v.status = 'error';
  if (event.kind === 'takeover_request') {
    v.status = 'blocked';
    if (!v.takeovers.some((t) => t.reqId === event.reqId)) v.takeovers = [...v.takeovers, { reqId: event.reqId, reason: event.reason }];
  }
  if (event.kind === 'takeover_resolved') {
    v.takeovers = v.takeovers.filter((t) => t.reqId !== event.reqId);
  }
  if (event.kind === 'session_config') v.sessionConfig = { modes: event.modes, configOptions: event.configOptions };
  if (event.kind === 'available_commands') v.commands = event.commands;
  if (event.kind === 'prompt_capabilities') v.imagePromptSupport = event.image;
  if (event.kind === 'aside_capabilities') v.asideSupport = event.fork;
  if (event.kind === 'steering_capabilities') v.steeringSupport = event.supported;
  if (event.kind === 'usage') v.usage = { used: event.used, size: event.size, cost: event.cost, updatedAt: usageUpdatedAt(event) };
  if (event.kind === 'control_state') v.controlMode = event.mode;
  if (event.kind === 'audio_preference') {
    v.audioOnTurnEnd = event.enabled;
    if (!event.enabled) { v.audioState = 'idle'; v.audioError = null; v.audioSeq = null; }
  }
  if (event.kind === 'audio_state') {
    v.audioState = event.state;
    v.audioError = event.state === 'error' ? event.message ?? 'Speech rendering failed' : null;
    v.audioSeq = event.seq;
  }
  if (event.kind === 'prompt_queued' && !v.queuedPrompts.some((prompt) => prompt.id === event.promptId)) {
    v.queuedPrompts = [...v.queuedPrompts, { id: event.promptId, blocks: event.blocks, queuedAt: event.queuedAt }];
  }
  if (event.kind === 'prompt_started' || event.kind === 'prompt_removed') {
    v.queuedPrompts = v.queuedPrompts.filter((prompt) => prompt.id !== event.promptId);
  }
}

// All pending browser takeovers across sessions, for the attention rail.
export function allTakeovers(st: StoreState): { sessionId: string; takeover: Takeover }[] {
  const out: { sessionId: string; takeover: Takeover }[] = [];
  for (const id of rankSessions(st.sessions, st.order)) {
    const a = st.sessions[id];
    for (const t of a.takeovers) out.push({ sessionId: id, takeover: t });
  }
  return out;
}
