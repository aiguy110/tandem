// The client state store: a projection of daemon messages (docs/ui.md). The
// browser holds no authoritative state — every field here is derived from a
// ServerMsg. Built on zustand (lightweight, no framework). The WsClient is owned
// here so actions and the reducer share one socket.

import { create } from 'zustand';
import { WsClient, resolveToken, type ConnState } from './ws/client';
import { ptyHub } from './terminal/ptyHub';
import { shellHub } from './terminal/shellHub';
import { browserHub } from './terminal/browserHub';
import type {
  AgentStatus,
  AgentSummary,
  AgentCatalog,
  Approval,
  BrowserInputWire,
  BrowserSnapshot,
  Channel,
  ClosePreview,
  ClientMsg,
  GitRefInfo,
  Profile,
  RepoInfo,
  ResumableSession,
  ResumeCatalog,
  ServerMsg,
  SessionConfigOption,
  SessionModeState,
  SlashCommand,
  SpawnSpec,
  SpawnOptions,
  PromptBlock,
  QueuedPrompt,
  WireEvent,
  WorkspaceDiff,
} from './wire';

// The focus/bandwidth rule (docs/browser.md): only the focused, browser-viewing
// client streams the screencast. Base subscription omits 'browser'; the mounted
// BrowserPane opts its agent in via setBrowserSub.
const BASE_CHANNELS: Channel[] = ['transcript', 'pty', 'terminals', 'status'];

// A pending agent-initiated takeover (browser.request_takeover) for the rail.
export interface Takeover {
  reqId: string;
  reason: string;
}

// Chat = the agent conversation (ACP transcript or the agent's resumable CLI,
// toggled by the selected Chat tab's ACP/CLI switch). Shell = the user's escape-hatch shell
// in the agent's worktree (the Terminal tab).
export type PaneId = 'chat' | 'shell' | 'diff' | 'browser';
export const PANES: PaneId[] = ['chat', 'shell', 'diff', 'browser'];

export interface AgentView {
  id: string;
  name: string;
  agent?: string;
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
  status: AgentStatus;
  events: { seq: number; event: WireEvent }[]; // transcript/terminals channel, seq-ordered
  lastSeq: number;
  pendingApprovals: Approval[];
  hasPty: boolean; // any raw_pty seen → the agent CLI view has live content
  // User escape-hatch shell (Terminal tab). shellExited flips true when the
  // shell process ends (shell_exit) so the pane can offer a restart.
  shellExited: boolean;
  shellExitMessage: string | null;
  // Browser subsystem (Phase 5): whether a browser exists for this agent and who
  // holds the wheel, plus any pending agent-initiated takeover requests.
  browserActive: boolean;
  browserOwner: 'agent' | 'user';
  // True while the user is satisfying an agent-requested takeover. This keeps
  // the hand-back action visually distinct from an unsolicited manual grab.
  browserTakeoverHeld: boolean;
  takeovers: Takeover[];
  // Permission-mode + config-option (incl. model selector) state, from the last
  // session_config event. Null until the ACP agent reports it (or for pty agents,
  // which never do) — the picker bar hides itself in that case.
  sessionConfig: { modes: SessionModeState | null; configOptions: SessionConfigOption[] } | null;
  usage: { used: number; size: number; cost?: { amount: number; currency: string } | null; updatedAt: number } | null;
  // The agent's slash-command menu (ACP available_commands_update), for the
  // fuzzy-find popup in PromptBar. Empty for pty agents / until first reported.
  commands: SlashCommand[];
  // null until the adapter reports ACP prompt capabilities.
  imagePromptSupport: boolean | null;
  // Daemon-owned FIFO entries waiting behind the active turn.
  queuedPrompts: QueuedPrompt[];
  controlMode: 'transcript' | 'switching' | 'terminal';
  // Stable adapter kind + whether the Chat tab offers the ACP/CLI switch.
  adapter: 'acp' | 'pty';
  canHandoff: boolean;
}

export type ModalKind = 'none' | 'spawn' | 'command' | 'resume';

export interface AckResult {
  agentId?: string;
  error?: string;
  promptId?: string;
  disposition?: 'started' | 'queued';
  position?: number;
  cleared?: number;
}

interface StoreState {
  conn: ConnState;
  theme: 'dark' | 'light';
  agents: Record<string, AgentView>;
  order: string[];
  focusedId: string | null;
  pane: PaneId;
  modal: ModalKind;
  inspectorOpen: boolean;
  dirs: RepoInfo[];
  agentCatalog: AgentCatalog | null;
  // Captured browser snapshots (seed states), refreshed on demand.
  snapshots: BrowserSnapshot[];
  // Resume picker: the resumable-session catalog (null until first fetched) and a
  // loading flag while the daemon probes agents for external sessions.
  resumeCatalog: ResumeCatalog | null;
  resumeLoading: boolean;
  // Unsent prompt drafts, keyed by agentId. Lives here (not in the pane's local
  // state) so a draft survives tab switches and agent switches, which remount the
  // TranscriptPane.
  drafts: Record<string, string>;
  // Which agent (if any) currently has its browser channel subscribed (i.e. the
  // focused agent with the Browser pane open) — drives the screencast focus rule.
  browserSubAgent: string | null;
  // Left/right rail collapse (mobile-friendly docking). Defaults from a
  // matchMedia breakpoint at boot, then user-toggleable regardless of width.
  agentsRailCollapsed: boolean;
  approvalsRailCollapsed: boolean;

  // actions
  boot: () => void;
  submitToken: (t: string) => void;
  focus: (id: string) => void;
  setPane: (p: PaneId) => void;
  toggleTheme: () => void;
  toggleAgentsRail: () => void;
  toggleApprovalsRail: () => void;
  setModal: (m: ModalKind) => void;
  toggleInspector: () => void;
  refreshDirs: () => void;
  refreshAgents: () => void;
  refreshSessions: () => void;
  resumeSession: (s: ResumableSession) => Promise<AckResult>;
  enterTerminal: (agentId: string, interrupt?: boolean) => Promise<AckResult>;
  leaveTerminal: (agentId: string) => Promise<AckResult>;
  // User escape-hatch shell (Terminal tab).
  openShell: (agentId: string, cols: number, rows: number) => Promise<AckResult>;
  restartShell: (agentId: string, cols: number, rows: number) => Promise<AckResult>;
  spawn: (spec: SpawnSpec) => Promise<AckResult>;
  getSpawnOptions: (agent: string, cwd: string, harness?: string) => Promise<SpawnOptions>;
  listGitRefs: (repo: string) => Promise<GitRefInfo[]>;
  // Browser snapshots + agent profiles.
  captureSnapshot: (agentId: string, name: string) => Promise<BrowserSnapshot[]>;
  listSnapshots: () => Promise<BrowserSnapshot[]>;
  deleteSnapshot: (id: string) => Promise<BrowserSnapshot[]>;
  listProfiles: (project?: string) => Promise<{ profiles: Profile[]; recent: string[] }>;
  renameProfile: (id: string, name: string, project?: string) => Promise<{ profiles: Profile[]; recent: string[] }>;
  renameAgent: (agentId: string, name: string) => Promise<AckResult>;
  deleteProfile: (id: string, project?: string) => Promise<{ profiles: Profile[]; recent: string[] }>;
  prompt: (agentId: string, input: string | PromptBlock[]) => Promise<AckResult>;
  removeQueuedPrompt: (agentId: string, promptId: string) => Promise<AckResult>;
  clearPromptQueue: (agentId: string) => Promise<AckResult>;
  interruptAndClearQueue: (agentId: string) => Promise<AckResult>;
  setDraft: (agentId: string, text: string) => void;
  interrupt: (agentId: string) => void;
  respond: (agentId: string, reqId: string, optionId: string) => void;
  setMode: (agentId: string, modeId: string) => void;
  setConfigOption: (agentId: string, configId: string, value: string | boolean) => void;
  getClosePreview: (agentId: string) => Promise<ClosePreview>;
  getDiff: (agentId: string) => Promise<WorkspaceDiff>;
  closeAgent: (agentId: string, force?: boolean, deleteWorktree?: boolean) => Promise<AckResult>;
  send: (m: ClientMsg) => void;
  nav: (dir: 1 | -1) => void;
  // Browser pane control (Phase 5).
  setBrowserSub: (agentId: string | null) => void;
  browserControl: (agentId: string, action: 'grab' | 'release') => void;
  restartBrowser: (agentId: string, snapshotId?: string) => Promise<AckResult>;
  browserInput: (agentId: string, event: BrowserInputWire) => void;
  toggleWheel: (agentId: string) => void;
}

// ---- ack correlation (spawn/close want structured results) ----
let corrCounter = 0;
const nextCorr = () => `c${++corrCounter}`;
const pendingAcks = new Map<string, (r: AckResult) => void>();
const pendingSpawnOptions = new Map<string, { resolve: (options: SpawnOptions) => void; reject: (error: Error) => void }>();
const pendingGitRefs = new Map<string, { resolve: (refs: GitRefInfo[]) => void; reject: (error: Error) => void }>();
const pendingClosePreviews = new Map<string, { resolve: (preview: ClosePreview) => void; reject: (error: Error) => void }>();
const pendingDiffs = new Map<string, { resolve: (diff: WorkspaceDiff) => void; reject: (error: Error) => void }>();
const pendingSnapshots = new Map<string, { resolve: (snaps: BrowserSnapshot[]) => void; reject: (error: Error) => void }>();
const pendingProfiles = new Map<string, { resolve: (r: { profiles: Profile[]; recent: string[] }) => void; reject: (error: Error) => void }>();

let client: WsClient;
// Guards the one-time window 'hashchange' listener boot() installs (boot may run
// twice under React StrictMode in dev).
let hashListenerAttached = false;
let gitRefreshListenersAttached = false;

const GIT_REFRESH_INTERVAL_MS = 15_000;

function rankAgents(agents: Record<string, AgentView>, order: string[]): string[] {
  // Blocked / error float to the top (docs/ui.md), otherwise insertion order.
  const weight = (s: AgentStatus) => (s === 'error' ? 0 : s === 'blocked' ? 1 : 2);
  return [...order].sort((a, b) => {
    const av = agents[a];
    const bv = agents[b];
    if (!av || !bv) return 0;
    const d = weight(av.status) - weight(bv.status);
    return d !== 0 ? d : order.indexOf(a) - order.indexOf(b);
  });
}

const initialTheme = (): 'dark' | 'light' => {
  const saved = localStorage.getItem('tandem.theme');
  return saved === 'light' ? 'light' : 'dark';
};

const USAGE_STORAGE_KEY = 'tandem.agentUsage';
type StoredUsage = NonNullable<AgentView['usage']>;

function readStoredUsage(agentId: string): StoredUsage | null {
  try {
    const all = JSON.parse(localStorage.getItem(USAGE_STORAGE_KEY) || '{}') as Record<string, StoredUsage>;
    const usage = all[agentId];
    return usage && usage.used >= 0 && usage.size > 0 && Number.isFinite(usage.updatedAt) ? usage : null;
  } catch {
    return null;
  }
}

function writeStoredUsage(agentId: string, usage: StoredUsage | null): void {
  try {
    const all = JSON.parse(localStorage.getItem(USAGE_STORAGE_KEY) || '{}') as Record<string, StoredUsage>;
    if (usage) all[agentId] = usage;
    else delete all[agentId];
    localStorage.setItem(USAGE_STORAGE_KEY, JSON.stringify(all));
  } catch {
    // Persistence is best-effort when storage is unavailable or malformed.
  }
}

// Rails collapse by default on narrow viewports (phones/small tablets), but the
// user can still toggle them open regardless of width.
const MOBILE_BREAKPOINT = '(max-width: 860px)';
const isNarrowViewport = (): boolean =>
  typeof window !== 'undefined' && !!window.matchMedia && window.matchMedia(MOBILE_BREAKPOINT).matches;

export const useStore = create<StoreState>((set, get) => {
  // Channels to subscribe for an agent: base always, plus 'browser' only for the
  // agent whose Browser pane is open (the screencast focus rule).
  const channelsFor = (id: string): Channel[] => (get().browserSubAgent === id ? [...BASE_CHANNELS, 'browser'] : BASE_CHANNELS);
  function subscribeAgent(id: string): void {
    client.send({ t: 'subscribe', agentId: id, channels: channelsFor(id), sinceSeq: get().agents[id]?.lastSeq ?? 0 });
  }

  // Apply one server message. This is the ONLY place agent state is mutated by
  // the daemon (actions mutate only local UI concerns like focus/pane/theme).
  const apply = (msg: ServerMsg): void => {
    switch (msg.t) {
      case 'agents': {
        const newlyDiscovered = msg.agents.filter((s) => !get().agents[s.id]).map((s) => s.id);
        set((st) => {
          const agents = { ...st.agents };
          const order = [...st.order];
          for (const s of msg.agents) {
            const prev = agents[s.id];
            agents[s.id] = mergeSummary(prev, s);
            if (!order.includes(s.id)) order.push(s.id);
          }
          // Drop any local agent the daemon no longer reports (e.g. closed elsewhere).
          const live = new Set(msg.agents.map((a) => a.id));
          for (const id of order.slice()) {
            if (!live.has(id) && agents[id] && agents[id].events.length === 0) {
              delete agents[id];
              order.splice(order.indexOf(id), 1);
            }
          }
          const focusedId = st.focusedId && agents[st.focusedId] ? st.focusedId : order[0] ?? null;
          return { agents, order, focusedId };
        });
        // The reconnect path already re-subscribes tracked agents. Summary
        // refreshes only need to subscribe agents discovered for the first time.
        for (const id of newlyDiscovered) subscribeAgent(id);
        return;
      }
      case 'dirs':
        set({ dirs: msg.dirs });
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
      case 'agent_catalog':
        set({ agentCatalog: msg.catalog });
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
        set({ resumeCatalog: msg.catalog, resumeLoading: false });
        return;
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
        browserHub.push(msg.agentId, { dataB64: msg.dataB64, meta: msg.meta });
        return;
      case 'browser_state': {
        set((st) => {
          const a = st.agents[msg.agentId];
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
            agents: {
              ...st.agents,
              [msg.agentId]: { ...a, browserActive: msg.active, browserOwner: msg.controlOwner, browserTakeoverHeld, takeovers },
            },
          };
        });
        return;
      }
      case 'ack': {
        if (msg.corrId && pendingAcks.has(msg.corrId)) {
          pendingAcks.get(msg.corrId)!({
            agentId: msg.agentId,
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
          if (!st.agents[msg.agentId]) return st;
          const agents = { ...st.agents };
          delete agents[msg.agentId];
          const order = st.order.filter((id) => id !== msg.agentId);
          const focusedId = st.focusedId === msg.agentId ? order[0] ?? null : st.focusedId;
          return { agents, order, focusedId };
        });
        ptyHub.clear(msg.agentId);
        shellHub.clear(msg.agentId);
        browserHub.clear(msg.agentId);
        writeStoredUsage(msg.agentId, null);
        return;
      }
      case 'snapshot': {
        set((st) => {
          const agents = { ...st.agents };
          const prev = agents[msg.agentId] ?? shell(msg.agentId);
          // raw_pty (agent CLI) and shell_pty/shell_exit (user Terminal shell)
          // are byte/lifecycle streams routed to their hubs, not the transcript.
          const transcript = msg.transcript.filter(
            (e) => e.event.kind !== 'raw_pty' && e.event.kind !== 'shell_pty' && e.event.kind !== 'shell_exit',
          );
          // Feed any pty frames in the snapshot into the terminal hubs (rehydrate).
          for (const e of msg.transcript) {
            if (e.event.kind === 'raw_pty') ptyHub.push(msg.agentId, e.event.dataB64);
            else if (e.event.kind === 'shell_pty') shellHub.push(msg.agentId, e.event.dataB64);
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
          agents[msg.agentId] = {
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
            // Pane-change subscriptions replay the still-pending request while
            // the user holds the wheel. Keep it acknowledged in that case.
            takeovers: prev.browserOwner === 'user' && prev.browserTakeoverHeld ? [] : replayedTakeovers,
            hasPty: prev.hasPty || msg.transcript.some((e) => e.event.kind === 'raw_pty'),
            shellExited,
            shellExitMessage,
          };
          const order = st.order.includes(msg.agentId) ? st.order : [...st.order, msg.agentId];
          return { agents, order, focusedId: st.focusedId ?? msg.agentId };
        });
        return;
      }
      case 'event': {
        const { agentId, seq, event } = msg;
        if (event.kind === 'raw_pty') {
          ptyHub.push(agentId, event.dataB64);
          set((st) => {
            const a = st.agents[agentId];
            if (!a || (a.hasPty && seq <= a.lastSeq)) return st;
            return { agents: { ...st.agents, [agentId]: { ...a, hasPty: true, lastSeq: Math.max(a.lastSeq, seq) } } };
          });
          return;
        }
        if (event.kind === 'shell_pty') {
          shellHub.push(agentId, event.dataB64);
          set((st) => {
            const a = st.agents[agentId];
            if (!a) return st;
            // Live output implies a running shell; clear any stale exited flag.
            if (!a.shellExited && seq <= a.lastSeq) return st;
            return { agents: { ...st.agents, [agentId]: { ...a, shellExited: false, shellExitMessage: null, lastSeq: Math.max(a.lastSeq, seq) } } };
          });
          return;
        }
        if (event.kind === 'shell_exit') {
          const message = event.message;
          set((st) => {
            const a = st.agents[agentId];
            if (!a) return st;
            return { agents: { ...st.agents, [agentId]: { ...a, shellExited: true, shellExitMessage: message, lastSeq: Math.max(a.lastSeq, seq) } } };
          });
          return;
        }
        const usageReceivedAt = event.kind === 'usage' ? Date.now() : undefined;
        set((st) => {
          const a = st.agents[agentId] ?? shell(agentId);
          if (seq <= a.lastSeq && st.agents[agentId]) return st; // already applied (dedupe)
          const next: AgentView = { ...a, events: [...a.events, { seq, event }], lastSeq: Math.max(a.lastSeq, seq) };
          applyEventToView(next, event, usageReceivedAt);
          if (next.usage && usageReceivedAt) writeStoredUsage(agentId, next.usage);
          const agents = { ...st.agents, [agentId]: next };
          const order = st.order.includes(agentId) ? st.order : [...st.order, agentId];
          return { agents, order };
        });
        return;
      }
      case 'prompt_queue':
        set((st) => {
          const agent = st.agents[msg.agentId];
          if (!agent) return st;
          return { agents: { ...st.agents, [msg.agentId]: { ...agent, queuedPrompts: msg.queuedPrompts } } };
        });
        return;
    }
  };

  client = new WsClient({
    onMessage: apply,
    onState: (conn) => set({ conn }),
    onOpen: () => {
      // Rediscover agents (and their metadata) and re-subscribe with sinceSeq.
      client.send({ t: 'list_agents' });
      client.send({ t: 'list_agent_catalog' });
      // Also re-subscribe to anything we already track, immediately (idempotent).
      for (const id of get().order) subscribeAgent(id);
    },
  });

  return {
    conn: 'connecting',
    theme: initialTheme(),
    agents: {},
    order: [],
    focusedId: null,
    pane: 'chat',
    modal: 'none',
    inspectorOpen: false,
    dirs: [],
    agentCatalog: null,
    snapshots: [],
    resumeCatalog: null,
    resumeLoading: false,
    drafts: {},
    browserSubAgent: null,
    agentsRailCollapsed: isNarrowViewport(),
    approvalsRailCollapsed: isNarrowViewport(),

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
        window.addEventListener('focus', refreshVisibleAgents);
        setInterval(refreshVisibleAgents, GIT_REFRESH_INTERVAL_MS);
      }
    },
    submitToken: (t) => client.setToken(t.trim()),
    focus: (id) => set({ focusedId: id }),
    // Selecting Terminal is view-only until its shroud's explicit Take control
    // action calls enterTerminal. Even an idle ACP session must never be swapped
    // merely because the user inspected the tab.
    setPane: (p) => set({ pane: p }),
    toggleTheme: () =>
      set((st) => {
        const theme = st.theme === 'dark' ? 'light' : 'dark';
        localStorage.setItem('tandem.theme', theme);
        return { theme };
      }),
    toggleAgentsRail: () => set((st) => ({ agentsRailCollapsed: !st.agentsRailCollapsed })),
    toggleApprovalsRail: () => set((st) => ({ approvalsRailCollapsed: !st.approvalsRailCollapsed })),
    setModal: (m) => {
      if (m === 'spawn') {
        get().refreshDirs();
        client.send({ t: 'list_agent_catalog' });
      }
      if (m === 'resume') get().refreshSessions();
      set({ modal: m });
    },
    toggleInspector: () => set((st) => ({ inspectorOpen: !st.inspectorOpen })),
    refreshDirs: () => client.send({ t: 'list_dirs' }),
    refreshAgents: () => client.send({ t: 'list_agents' }),
    refreshSessions: () => {
      set({ resumeLoading: true });
      client.send({ t: 'list_sessions' });
    },
    resumeSession: (session) =>
      new Promise<AckResult>((resolve) => {
        const corrId = nextCorr();
        pendingAcks.set(corrId, (r) => {
          if (r.agentId && !r.error) {
            get().refreshAgents();
            set({ focusedId: r.agentId, modal: 'none', pane: 'chat' });
          }
          resolve(r);
        });
        client.send({
          t: 'resume_session',
          sessionId: session.sessionId,
          agent: session.source === 'external' ? session.agent : undefined,
          cwd: session.source === 'external' ? session.cwd : undefined,
          corrId,
        });
      }),
    enterTerminal: (agentId, interrupt = false) =>
      new Promise<AckResult>((resolve) => {
        const corrId = nextCorr();
        pendingAcks.set(corrId, resolve);
        client.send({ t: 'enter_terminal', agentId, interrupt, corrId });
      }),
    leaveTerminal: (agentId) =>
      new Promise<AckResult>((resolve) => {
        const corrId = nextCorr();
        pendingAcks.set(corrId, resolve);
        client.send({ t: 'leave_terminal', agentId, corrId });
      }),
    openShell: (agentId, cols, rows) =>
      new Promise<AckResult>((resolve) => {
        const corrId = nextCorr();
        pendingAcks.set(corrId, resolve);
        client.send({ t: 'shell_open', agentId, cols, rows, corrId });
      }),
    restartShell: (agentId, cols, rows) => {
      // Clear the exited flag + prior scrollback so the fresh shell starts clean.
      shellHub.reset(agentId);
      set((st) => {
        const a = st.agents[agentId];
        if (!a) return st;
        return { agents: { ...st.agents, [agentId]: { ...a, shellExited: false, shellExitMessage: null } } };
      });
      return new Promise<AckResult>((resolve) => {
        const corrId = nextCorr();
        pendingAcks.set(corrId, resolve);
        client.send({ t: 'shell_open', agentId, cols, rows, corrId });
      });
    },
    spawn: (spec) =>
      new Promise<AckResult>((resolve) => {
        const corrId = nextCorr();
        pendingAcks.set(corrId, (r) => {
          if (r.agentId && !r.error) {
            get().refreshAgents();
            set({ focusedId: r.agentId, modal: 'none', pane: 'chat' });
          }
          resolve(r);
        });
        client.send({ t: 'spawn_agent', spec, corrId });
      }),
    getSpawnOptions: (agent, cwd, harness) =>
      new Promise<SpawnOptions>((resolve, reject) => {
        const corrId = nextCorr();
        pendingSpawnOptions.set(corrId, { resolve, reject });
        client.send({ t: 'get_spawn_options', agent, harness, cwd, corrId });
      }),
    listGitRefs: (repo) =>
      new Promise<GitRefInfo[]>((resolve, reject) => {
        const corrId = nextCorr();
        pendingGitRefs.set(corrId, { resolve, reject });
        client.send({ t: 'list_git_refs', repo, corrId });
      }),
    captureSnapshot: (agentId, name) =>
      new Promise<BrowserSnapshot[]>((resolve, reject) => {
        const corrId = nextCorr();
        pendingSnapshots.set(corrId, { resolve, reject });
        client.send({ t: 'capture_snapshot', agentId, name, corrId });
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
    renameAgent: (agentId, name) =>
      new Promise<AckResult>((resolve) => {
        const corrId = nextCorr();
        pendingAcks.set(corrId, (result) => {
          if (!result.error) get().refreshAgents();
          resolve(result);
        });
        client.send({ t: 'rename_agent', agentId, name, corrId });
      }),
    deleteProfile: (id, project) =>
      new Promise<{ profiles: Profile[]; recent: string[] }>((resolve, reject) => {
        const corrId = nextCorr();
        pendingProfiles.set(corrId, { resolve, reject });
        client.send({ t: 'delete_profile', id, project, corrId });
      }),
    prompt: (agentId, input) =>
      new Promise<AckResult>((resolve) => {
        const corrId = nextCorr();
        pendingAcks.set(corrId, (result) => {
          if (!result.error) set((st) => ({ drafts: { ...st.drafts, [agentId]: '' } }));
          resolve(result);
        });
        client.send(typeof input === 'string'
          ? { t: 'prompt', agentId, text: input, corrId }
          : { t: 'prompt', agentId, blocks: input, corrId });
      }),
    removeQueuedPrompt: (agentId, promptId) =>
      new Promise<AckResult>((resolve) => {
        const corrId = nextCorr();
        pendingAcks.set(corrId, resolve);
        client.send({ t: 'remove_queued_prompt', agentId, promptId, corrId });
      }),
    clearPromptQueue: (agentId) =>
      new Promise<AckResult>((resolve) => {
        const corrId = nextCorr();
        pendingAcks.set(corrId, resolve);
        client.send({ t: 'clear_prompt_queue', agentId, corrId });
      }),
    interruptAndClearQueue: (agentId) =>
      new Promise<AckResult>((resolve) => {
        const corrId = nextCorr();
        pendingAcks.set(corrId, resolve);
        client.send({ t: 'interrupt_and_clear_queue', agentId, corrId });
      }),
    setDraft: (agentId, text) => set((st) => ({ drafts: { ...st.drafts, [agentId]: text } })),
    interrupt: (agentId) => client.send({ t: 'interrupt', agentId }),
    respond: (agentId, reqId, optionId) => {
      // Optimistically drop the approval so the rail feels instant; the daemon
      // confirms via a status/event stream.
      set((st) => {
        const a = st.agents[agentId];
        if (!a) return st;
        return { agents: { ...st.agents, [agentId]: { ...a, pendingApprovals: a.pendingApprovals.filter((p) => p.reqId !== reqId) } } };
      });
      client.send({ t: 'permission_response', agentId, reqId, optionId });
    },
    setMode: (agentId, modeId) => client.send({ t: 'set_mode', agentId, modeId }),
    setConfigOption: (agentId, configId, value) => client.send({ t: 'set_config_option', agentId, configId, value }),
    getClosePreview: (agentId) =>
      new Promise<ClosePreview>((resolve, reject) => {
        const corrId = nextCorr();
        pendingClosePreviews.set(corrId, { resolve, reject });
        client.send({ t: 'get_close_preview', agentId, corrId });
      }),
    getDiff: (agentId) =>
      new Promise<WorkspaceDiff>((resolve, reject) => {
        const corrId = nextCorr();
        pendingDiffs.set(corrId, { resolve, reject });
        client.send({ t: 'get_diff', agentId, corrId });
      }),
    closeAgent: (agentId, force, deleteWorktree) =>
      new Promise<AckResult>((resolve) => {
        const corrId = nextCorr();
        pendingAcks.set(corrId, resolve);
        client.send({ t: 'close_agent', agentId, force, deleteWorktree, corrId });
      }),
    send: (m) => client.send(m),
    // Opt an agent's browser channel in/out (screencast focus rule). Re-subscribe
    // both the previously- and newly-viewing agents so the daemon starts/stops the
    // screencast accordingly.
    setBrowserSub: (agentId) => {
      const prev = get().browserSubAgent;
      if (prev === agentId) return;
      set({ browserSubAgent: agentId });
      if (prev && get().agents[prev]) subscribeAgent(prev);
      if (agentId && get().agents[agentId]) subscribeAgent(agentId);
    },
    browserControl: (agentId, action) => client.send({ t: 'browser_control', agentId, action }),
    restartBrowser: (agentId, snapshotId) =>
      new Promise<AckResult>((resolve) => {
        const corrId = nextCorr();
        pendingAcks.set(corrId, resolve);
        client.send({ t: 'restart_browser', agentId, snapshotId, corrId });
      }),
    browserInput: (agentId, event) => client.send({ t: 'browser_input', agentId, event }),
    toggleWheel: (agentId) => {
      const a = get().agents[agentId];
      if (!a || !a.browserActive) return;
      client.send({ t: 'browser_control', agentId, action: a.browserOwner === 'user' ? 'release' : 'grab' });
    },
    nav: (dir) =>
      set((st) => {
        const ranked = rankAgents(st.agents, st.order);
        if (ranked.length === 0) return st;
        const i = st.focusedId ? ranked.indexOf(st.focusedId) : -1;
        const next = ranked[(i + dir + ranked.length) % ranked.length];
        return { focusedId: next };
      }),
  };
});

// Derived selector: rail order with blocked/error floated to the top.
export function rankedOrder(st: StoreState): string[] {
  return rankAgents(st.agents, st.order);
}
// All pending approvals across all agents (for the right rail), urgent first.
export function allApprovals(st: StoreState): { agentId: string; approval: Approval }[] {
  const out: { agentId: string; approval: Approval }[] = [];
  for (const id of rankAgents(st.agents, st.order)) {
    const a = st.agents[id];
    for (const ap of a.pendingApprovals) out.push({ agentId: id, approval: ap });
  }
  return out;
}

function shell(id: string): AgentView {
  return {
    id,
    name: id,
    workspace: { kind: 'worktree', repo: '', repoPath: '', branch: '', cwd: '' },
    status: 'idle',
    events: [],
    lastSeq: 0,
    pendingApprovals: [],
    hasPty: false,
    shellExited: false,
    shellExitMessage: null,
    browserActive: false,
    browserOwner: 'agent',
    browserTakeoverHeld: false,
    takeovers: [],
    sessionConfig: null,
    usage: readStoredUsage(id),
    commands: [],
    imagePromptSupport: null,
    queuedPrompts: [],
    controlMode: 'transcript',
    adapter: 'acp',
    canHandoff: false,
  };
}

function mergeSummary(prev: AgentView | undefined, s: AgentSummary): AgentView {
  const base = prev ?? shell(s.id);
  return { ...base, name: s.name, agent: s.agent, workspace: s.workspace, status: s.status, controlMode: s.controlMode, adapter: s.adapter, canHandoff: s.canHandoff };
}

// Fold status/permission side effects of an event into the view (mirrors the
// daemon's session-side handling so the rail stays correct between snapshots).
function applyEventToView(v: AgentView, event: WireEvent, receivedAt = Date.now()): void {
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
  if (event.kind === 'usage') v.usage = { used: event.used, size: event.size, cost: event.cost, updatedAt: receivedAt };
  if (event.kind === 'control_state') v.controlMode = event.mode;
  if (event.kind === 'prompt_queued' && !v.queuedPrompts.some((prompt) => prompt.id === event.promptId)) {
    v.queuedPrompts = [...v.queuedPrompts, { id: event.promptId, blocks: event.blocks, queuedAt: event.queuedAt }];
  }
  if (event.kind === 'prompt_started' || event.kind === 'prompt_removed') {
    v.queuedPrompts = v.queuedPrompts.filter((prompt) => prompt.id !== event.promptId);
  }
}

// All pending browser takeovers across agents, for the attention rail.
export function allTakeovers(st: StoreState): { agentId: string; takeover: Takeover }[] {
  const out: { agentId: string; takeover: Takeover }[] = [];
  for (const id of rankAgents(st.agents, st.order)) {
    const a = st.agents[id];
    for (const t of a.takeovers) out.push({ agentId: id, takeover: t });
  }
  return out;
}
