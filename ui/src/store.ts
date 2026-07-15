// The client state store: a projection of daemon messages (docs/ui.md). The
// browser holds no authoritative state — every field here is derived from a
// ServerMsg. Built on zustand (lightweight, no framework). The WsClient is owned
// here so actions and the reducer share one socket.

import { create } from 'zustand';
import { WsClient, resolveToken, type ConnState } from './ws/client';
import { ptyHub } from './terminal/ptyHub';
import { browserHub } from './terminal/browserHub';
import type {
  AgentStatus,
  AgentSummary,
  Approval,
  BrowserInputWire,
  Channel,
  ClientMsg,
  RepoInfo,
  ServerMsg,
  SessionConfigOption,
  SessionModeState,
  SlashCommand,
  SpawnSpec,
  WireEvent,
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

export type PaneId = 'transcript' | 'terminal' | 'diff' | 'browser';
export const PANES: PaneId[] = ['transcript', 'terminal', 'diff', 'browser'];

export interface AgentView {
  id: string;
  name: string;
  workspace: {
    kind: 'worktree' | 'existing';
    repo: string;
    repoPath: string;
    branch: string;
    cwd: string;
    gitState?: 'dirty' | 'unmerged' | 'synced';
  };
  status: AgentStatus;
  events: { seq: number; event: WireEvent }[]; // transcript/terminals channel, seq-ordered
  lastSeq: number;
  pendingApprovals: Approval[];
  hasPty: boolean; // any raw_pty seen → the Terminal pane has live content
  // Browser subsystem (Phase 5): whether a browser exists for this agent and who
  // holds the wheel, plus any pending agent-initiated takeover requests.
  browserActive: boolean;
  browserOwner: 'agent' | 'user';
  takeovers: Takeover[];
  // Permission-mode + config-option (incl. model selector) state, from the last
  // session_config event. Null until the ACP agent reports it (or for pty agents,
  // which never do) — the picker bar hides itself in that case.
  sessionConfig: { modes: SessionModeState | null; configOptions: SessionConfigOption[] } | null;
  // The agent's slash-command menu (ACP available_commands_update), for the
  // fuzzy-find popup in PromptBar. Empty for pty agents / until first reported.
  commands: SlashCommand[];
}

export type ModalKind = 'none' | 'spawn' | 'command';

export interface AckResult {
  agentId?: string;
  error?: string;
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
  spawn: (spec: SpawnSpec) => Promise<AckResult>;
  prompt: (agentId: string, text: string) => void;
  setDraft: (agentId: string, text: string) => void;
  interrupt: (agentId: string) => void;
  respond: (agentId: string, reqId: string, optionId: string) => void;
  setMode: (agentId: string, modeId: string) => void;
  setConfigOption: (agentId: string, configId: string, value: string | boolean) => void;
  closeAgent: (agentId: string, force?: boolean) => Promise<AckResult>;
  send: (m: ClientMsg) => void;
  nav: (dir: 1 | -1) => void;
  // Browser pane control (Phase 5).
  setBrowserSub: (agentId: string | null) => void;
  browserControl: (agentId: string, action: 'grab' | 'release') => void;
  browserInput: (agentId: string, event: BrowserInputWire) => void;
  toggleWheel: (agentId: string) => void;
}

// ---- ack correlation (spawn/close want structured results) ----
let corrCounter = 0;
const nextCorr = () => `c${++corrCounter}`;
const pendingAcks = new Map<string, (r: AckResult) => void>();

let client: WsClient;
// Guards the one-time window 'hashchange' listener boot() installs (boot may run
// twice under React StrictMode in dev).
let hashListenerAttached = false;

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
        // Subscribe to every known agent (base channels: keeps rails + transcript
        // live; the focused Browser pane adds the browser channel separately).
        for (const s of msg.agents) subscribeAgent(s.id);
        return;
      }
      case 'dirs':
        set({ dirs: msg.dirs });
        return;
      case 'browser_frame':
        // Frames bypass the reactive store (browserHub) to avoid re-render storms.
        browserHub.push(msg.agentId, { dataB64: msg.dataB64, meta: msg.meta });
        return;
      case 'browser_state': {
        set((st) => {
          const a = st.agents[msg.agentId];
          if (!a) return st;
          // Handing the wheel back (owner→agent) clears any pending takeover.
          const takeovers = msg.controlOwner === 'agent' ? [] : a.takeovers;
          return { agents: { ...st.agents, [msg.agentId]: { ...a, browserActive: msg.active, browserOwner: msg.controlOwner, takeovers } } };
        });
        return;
      }
      case 'ack': {
        if (msg.corrId && pendingAcks.has(msg.corrId)) {
          pendingAcks.get(msg.corrId)!({ agentId: msg.agentId, error: msg.error });
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
        browserHub.clear(msg.agentId);
        return;
      }
      case 'snapshot': {
        set((st) => {
          const agents = { ...st.agents };
          const prev = agents[msg.agentId] ?? shell(msg.agentId);
          const transcript = msg.transcript.filter((e) => e.event.kind !== 'raw_pty');
          // Feed any pty frames in the snapshot into the terminal hub (rehydrate).
          for (const e of msg.transcript) if (e.event.kind === 'raw_pty') ptyHub.push(msg.agentId, e.event.dataB64);
          // session_config/available_commands aren't top-level snapshot fields
          // (unlike status/pendingApprovals) — fold the latest one out of the
          // replayed transcript, mirroring the live 'event' path's applyEventToView.
          const lastConfig = [...transcript].reverse().find((e) => e.event.kind === 'session_config');
          const lastCommands = [...transcript].reverse().find((e) => e.event.kind === 'available_commands');
          agents[msg.agentId] = {
            ...prev,
            status: msg.status,
            pendingApprovals: msg.pendingApprovals,
            events: transcript,
            lastSeq: msg.seq,
            sessionConfig:
              lastConfig && lastConfig.event.kind === 'session_config'
                ? { modes: lastConfig.event.modes, configOptions: lastConfig.event.configOptions }
                : prev.sessionConfig,
            commands: lastCommands && lastCommands.event.kind === 'available_commands' ? lastCommands.event.commands : prev.commands,
            hasPty: prev.hasPty || msg.transcript.some((e) => e.event.kind === 'raw_pty'),
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
        set((st) => {
          const a = st.agents[agentId] ?? shell(agentId);
          if (seq <= a.lastSeq && st.agents[agentId]) return st; // already applied (dedupe)
          const next: AgentView = { ...a, events: [...a.events, { seq, event }], lastSeq: Math.max(a.lastSeq, seq) };
          applyEventToView(next, event);
          const agents = { ...st.agents, [agentId]: next };
          const order = st.order.includes(agentId) ? st.order : [...st.order, agentId];
          return { agents, order };
        });
        return;
      }
    }
  };

  client = new WsClient({
    onMessage: apply,
    onState: (conn) => set({ conn }),
    onOpen: () => {
      // Rediscover agents (and their metadata) and re-subscribe with sinceSeq.
      client.send({ t: 'list_agents' });
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
    pane: 'transcript',
    modal: 'none',
    inspectorOpen: false,
    dirs: [],
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
    },
    submitToken: (t) => client.setToken(t.trim()),
    focus: (id) => set({ focusedId: id }),
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
      if (m === 'spawn') get().refreshDirs();
      set({ modal: m });
    },
    toggleInspector: () => set((st) => ({ inspectorOpen: !st.inspectorOpen })),
    refreshDirs: () => client.send({ t: 'list_dirs' }),
    refreshAgents: () => client.send({ t: 'list_agents' }),
    spawn: (spec) =>
      new Promise<AckResult>((resolve) => {
        const corrId = nextCorr();
        pendingAcks.set(corrId, (r) => {
          if (r.agentId && !r.error) {
            get().refreshAgents();
            set({ focusedId: r.agentId, modal: 'none', pane: 'transcript' });
          }
          resolve(r);
        });
        client.send({ t: 'spawn_agent', spec, corrId });
      }),
    prompt: (agentId, text) => {
      client.send({ t: 'prompt', agentId, text });
      set((st) => ({ drafts: { ...st.drafts, [agentId]: '' } }));
    },
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
    closeAgent: (agentId, force) =>
      new Promise<AckResult>((resolve) => {
        const corrId = nextCorr();
        pendingAcks.set(corrId, resolve);
        client.send({ t: 'close_agent', agentId, force, corrId });
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
    browserActive: false,
    browserOwner: 'agent',
    takeovers: [],
    sessionConfig: null,
    commands: [],
  };
}

function mergeSummary(prev: AgentView | undefined, s: AgentSummary): AgentView {
  const base = prev ?? shell(s.id);
  return { ...base, name: s.name, workspace: s.workspace, status: s.status };
}

// Fold status/permission side effects of an event into the view (mirrors the
// daemon's session-side handling so the rail stays correct between snapshots).
function applyEventToView(v: AgentView, event: WireEvent): void {
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
  if (event.kind === 'session_config') v.sessionConfig = { modes: event.modes, configOptions: event.configOptions };
  if (event.kind === 'available_commands') v.commands = event.commands;
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
