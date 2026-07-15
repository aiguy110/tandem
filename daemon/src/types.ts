// Normalized agent vocabulary — the boundary every adapter emits into.
// Mirrors docs/agent-adapter.md (ACP-shaped, since ACP is the reference).
//
// This file is also the single home for the WS protocol envelopes
// (docs/ws-protocol.md) and the SpawnSpec (docs/spawn-and-workspaces.md), so the
// normalized boundary and the wire contract live in one place. The WS layer only
// ever ships these shapes — it must never leak ACP JSON-RPC.

export type AgentStatus = 'idle' | 'working' | 'blocked' | 'error';
export type ControlMode = 'transcript' | 'switching' | 'terminal';

export type ToolStatus = 'pending' | 'running' | 'done' | 'error' | 'cancelled';

// ---- Session modes + config options (ACP session-modes / session-config-options) --
// Permission-mode style modes (session/set_mode) and a generic config-option list
// (session/set_config_option) — the latter is how ACP surfaces a model selector
// (SessionConfigOption.category === 'model'), among other selectors. Normalized
// here so the WS layer never leaks the ACP option/group shapes.
export interface SessionMode {
  id: string;
  name: string;
  description?: string;
}
export interface SessionModeState {
  currentModeId: string;
  availableModes: SessionMode[];
}
export interface SessionConfigSelectOption {
  value: string;
  name: string;
  description?: string;
}
export interface SessionConfigOption {
  id: string;
  name: string;
  description?: string;
  // Semantic hint for UX placement (SessionConfigOptionCategory) — 'model' is the
  // one Tandem cares about today; others pass through untouched.
  category?: string;
  type: 'select' | 'boolean';
  currentValue: string | boolean;
  // Present for type:'select' — flattened (groups collapsed) since Tandem's UI is
  // a plain dropdown, not a grouped menu.
  options?: SessionConfigSelectOption[];
}

// ACP AvailableCommand (session/available_commands_update) — the agent's
// slash-command menu (e.g. "/compact", "/review"). `input` is a free-form
// hint string for the argument the command expects, if any.
export interface SlashCommand {
  name: string;
  description?: string;
  input?: string;
}

// Durable/wire prompt content. Image bytes live in the daemon asset store; ACP's
// inline base64 representation is assembled only at the adapter boundary.
export type PromptBlock =
  | { type: 'text'; text: string }
  | { type: 'image'; assetId: string; mimeType: string; name?: string };

export type AdapterPromptBlock =
  | { type: 'text'; text: string }
  | { type: 'image'; mimeType: string; data: string };

export type AgentEvent =
  // The human's prompt, echoed into the log so it renders in the transcript and
  // replays for every client (incl. after a daemon restart) — the daemon owns it,
  // the browser never fabricates it.
  | { kind: 'user_message'; text: string; blocks?: PromptBlock[] }
  | { kind: 'message_chunk'; text: string }
  | { kind: 'thought_chunk'; text: string }
  | { kind: 'tool_call'; id: string; title: string; status: ToolStatus; content?: unknown; rawInput?: unknown }
  | { kind: 'tool_call_update'; id: string; status?: ToolStatus; content?: unknown }
  | { kind: 'plan'; entries: { label: string; status: 'pending' | 'in_progress' | 'done' }[] }
  | { kind: 'terminal_output'; termId: string; chunk: string; truncated: boolean }
  | { kind: 'permission_request'; reqId: string; toolCallId: string; title: string; options: { optionId: string; name: string }[] }
  | { kind: 'status'; status: AgentStatus }
  | { kind: 'error'; message: string }
  // Current permission-mode + config-option (incl. model selector) state, pushed
  // once modes/configOptions are known and again on every current_mode_update /
  // config_option_update. The UI folds these to "latest wins" like `status`.
  | { kind: 'session_config'; modes: SessionModeState | null; configOptions: SessionConfigOption[] }
  // The agent's slash-command menu, pushed once known and again on every
  // available_commands_update. The UI folds this to "latest wins" like `status`.
  | { kind: 'available_commands'; commands: SlashCommand[] }
  | { kind: 'prompt_capabilities'; image: boolean }
  // Current context-window occupancy and optional cumulative session cost,
  // normalized from ACP usage_update. Latest value wins in client projections.
  | { kind: 'usage'; used: number; size: number; cost?: { amount: number; currency: string } | null }
  // Agent-initiated browser handoff (docs/browser.md Attention): the agent called
  // the Tandem-control MCP's browser.request_takeover. A normalized event so it
  // logs, replays, and folds into the attention rail. `reason` is human-facing.
  | { kind: 'takeover_request'; reqId: string; reason: string }
  // Which interface currently owns this coding-agent session. Emitted by the
  // daemon during ACP ↔ resumable CLI handoffs so every client shrouds the
  // inactive surface consistently.
  | { kind: 'control_state'; mode: ControlMode }
  | { kind: 'raw_pty'; data: Uint8Array };

// An ACP MCP server registration (McpServerStdio in the SDK schema): the AGENT
// spawns these; the daemon only declares them at session/new. `command` is an
// absolute executable path; `env` is name/value pairs.
export interface McpServerSpec {
  name: string;
  command: string;
  args: string[];
  env: { name: string; value: string }[];
}

export interface SpawnOpts {
  cwd?: string;
  cmd?: string;
  args?: string[];
  // Restore path: when set and the agent advertises the loadSession capability,
  // the adapter resumes this ACP session (session/load) instead of session/new.
  resumeSessionId?: string;
  // When resuming: capture the agent's replayed history (session/load re-streams
  // the whole prior conversation) into the event log instead of suppressing it.
  // Restore leaves this false — the log already holds that history under the same
  // agentId, so re-emitting would duplicate it. A Resume of an *external* session
  // (no prior Tandem log) sets it true so the transcript is populated.
  captureReplay?: boolean;
  // MCP servers to register at session/new (Phase 5: Playwright MCP + the
  // Tandem-control MCP). Empty/omitted = none.
  mcpServers?: McpServerSpec[];
}

// ---- ClientServices (docs/agent-adapter.md) ------------------------------
// ACP makes the *client* own fs + terminals, so an adapter needs handles back
// into the daemon. Phase 3 wires these for real: WorkspaceFs (the sandbox choke
// point) + TerminalHost (daemon-owned terminal execution & buffering).

// The exit disposition of a terminal command, mirroring ACP's TerminalExitStatus
// (exitCode is null when killed by a signal). Normalized — no ACP JSON leaks out.
export interface TermExit {
  exitCode: number | null;
  signal: string | null;
}

// Options for spawning a daemon-owned terminal. `outputByteLimit` is the ACP
// cap the agent asked for — it bounds ONLY the terminal/output poll response,
// not the daemon's own (larger) scrollback.
export interface TerminalCreateOpts {
  command: string;
  args?: string[];
  cwd?: string;
  env?: { name: string; value: string }[];
  outputByteLimit?: number;
}

export interface TerminalHost {
  create(opts: TerminalCreateOpts): Promise<string>;
  // ACP terminal/output view: honors the per-terminal outputByteLimit and the
  // truncated-from-the-start contract.
  output(termId: string): { output: string; truncated: boolean; exitStatus: TermExit | null };
  waitForExit(termId: string): Promise<TermExit>;
  kill(termId: string): void;
  release(termId: string): void;
  // ---- daemon-side extras (not part of ACP) ----
  // The full daemon scrollback (bounded by the daemon's own, larger cap),
  // independent of the ACP outputByteLimit. This is what makes terminal
  // re-capture on reconnect richer than the agent's own view.
  scrollback(termId: string): { output: string; truncated: boolean };
  exitStatus(termId: string): TermExit | null;
  has(termId: string): boolean;
  disposeAll(): void;
}

export interface WorkspaceFs {
  readTextFile(path: string, opts?: { line?: number; limit?: number }): Promise<string>;
  writeTextFile(path: string, text: string): Promise<void>;
}

export interface ClientServices {
  fs: WorkspaceFs;
  terminals: TerminalHost;
  permissions: { request(reqId: string): void };
}

export interface AgentAdapter {
  readonly id: string;
  readonly capabilities: { structured: boolean; terminals: boolean; loadSession: boolean; fs: boolean; image: boolean };
  readonly pid?: number;
  // The ACP sessionId once established — persisted for restore (D11). Undefined
  // for adapters without a session concept (pty).
  readonly acpSessionId?: string;
  // Resolves when an interactive adapter exits. ACP adapters omit it; handoff
  // PTYs use it to automatically return control to the transcript surface.
  readonly exited?: Promise<number>;

  spawn(opts: SpawnOpts, services?: ClientServices): Promise<void>;
  // Resolves with the ACP stopReason for the turn (end_turn | max_tokens |
  // max_turn_requests | refusal | cancelled). Non-ACP adapters return 'end_turn'.
  prompt(input: string | AdapterPromptBlock[]): Promise<string>;
  sendInput(bytes: Uint8Array): void;
  resize?(cols: number, rows: number): void;
  respondPermission(reqId: string, optionId: string): void;
  interrupt(): void;
  loadSession?(sessionId: string): Promise<void>; // only if capabilities.loadSession
  // Session modes / config options (ACP-only; pty has neither). Adapters that
  // don't support these simply omit them — session.ts rejects with a clear error.
  setMode?(modeId: string): Promise<void>;
  setConfigOption?(configId: string, value: string | boolean): Promise<void>;
  dispose(): Promise<void>;

  readonly events: AsyncIterable<AgentEvent>;
}

// ---- SpawnSpec (docs/spawn-and-workspaces.md) ----------------------------
// `branch`/`baseRef` are optional on input (Phase 2 deviation from the doc's
// illustrative interface, which showed them as plain strings): the
// WorkspaceManager fills sensible defaults — branch `tandem/<agent-name>`,
// baseRef the repo's current HEAD — and persists the *resolved* values back
// into the stored spec so restore/respawn are unambiguous.
export type Workspace =
  | { kind: 'worktree'; repo: string; branch?: string; baseRef?: string }
  | { kind: 'existing'; cwd: string };

export interface SpawnSpec {
  adapter: 'acp' | 'pty'; // default: acp
  // Which ACP-speaking coding agent to launch (Config.acp.agents key, e.g.
  // 'claude' | 'codex' | 'pi'). Ignored when adapter is 'pty'. Defaults to
  // Config.acp.default.
  agent?: string;
  workspace: Workspace;
  name?: string; // auto: web-1, api-2…
  task?: string; // optional initial prompt, dispatched on spawn
  // Initial ACP configuration, applied after session/new reveals the session's
  // controls and before an optional first task is dispatched.
  sessionConfig?: { modeId?: string; configOptions?: Record<string, string | boolean> };
  preset?: string; // reserved; single default agent for now
}

export interface SpawnOptions {
  modes: SessionModeState | null;
  configOptions: SessionConfigOption[];
}

// A persisted agent row (D14). `spec` + `acpSessionId` are what restore needs.
// `cwd` (Phase 2) is the resolved working directory — the worktree checkout
// path for `kind:'worktree'`, or the raw dir for `kind:'existing'` — so
// restore/list_dirs/collision-detection never have to recompute it.
export interface AgentRecord {
  id: string;
  name: string;
  spec: SpawnSpec;
  cwd: string;
  acpSessionId: string | null;
  status: AgentStatus;
  createdAt: number;
  closedAt: number | null;
}

// ---- Resume Session catalog (docs/spawn-and-workspaces.md Resume) --------
// A resumable coding-agent session, from either of two sources:
//   'tandem'   — an agent Tandem spawned (a row in `agents`, live or closed);
//                carries the full Tandem linkage (agentId/name/branch/status).
//   'external' — a session Tandem never spawned, discovered by asking an ACP
//                agent that supports `session/list` (capability-gated). Only
//                sessionId/cwd/title/updatedAt are known.
// Every entry is resumable via the agent's `session/load` — `sessionId` is the
// same id its CLI resumes (e.g. `claude --resume <sessionId>`).
export interface ResumableSession {
  sessionId: string;
  source: 'tandem' | 'external';
  agent: string; // ACP agent type (claude | codex | pi | …)
  adapter: 'acp' | 'pty';
  cwd: string;
  title?: string;
  updatedAt?: string; // ISO 8601, best-effort
  // Tandem linkage — present only for source === 'tandem'.
  agentId?: string;
  agentName?: string;
  branch?: string;
  live?: boolean; // currently a running Tandem agent
  closed?: boolean; // a closed Tandem agent (torn-down checkout, kept branch)
  status?: AgentStatus;
}

// Per-adapter enumeration capability, so the Resume UI can flag that some
// configured adapters can't be listed (their externally-run sessions won't
// appear — only ones Tandem itself spawned).
export interface ResumeAdapterInfo {
  agent: string;
  supportsList: boolean;
}

export interface ResumeCatalog {
  sessions: ResumableSession[];
  adapters: ResumeAdapterInfo[];
}

// ---- Repo discovery (Phase 2, docs/spawn-and-workspaces.md quick-spawn) --
// Returned by `list_dirs` for the spawn palette's directory list.
export interface RepoInfo {
  path: string;
  name: string;
  currentBranch: string;
  dirty: boolean;
  hasLiveAgent: boolean;
}

// ---- WS protocol (docs/ws-protocol.md) -----------------------------------
export type Channel = 'transcript' | 'pty' | 'terminals' | 'browser' | 'status';

export interface Approval {
  reqId: string;
  toolCallId: string;
  title: string;
  options: { optionId: string; name: string }[];
}

// A compact per-agent summary for the left rail: enough to render a row (name,
// workspace repo/branch, status, pending-approval count) without subscribing to
// each agent's transcript. Returned by `list_agents`; the snapshot carries
// status + approvals but neither name nor workspace, so a freshly loaded or
// reconnecting client needs this to populate the rail (esp. after a restart).
export interface AgentSummary {
  id: string;
  name: string;
  agent?: string;
  // `repo` is a display basename; `repoPath` is the full source-repo path (the
  // spawn origin, needed for "sibling" spawns); `cwd` is this agent's checkout.
  // `gitState`: 'dirty' (uncommitted changes) > 'unmerged' (committed but not yet
  // merged into baseRef) > 'synced' (clean and merged) — undefined while unknown
  // (e.g. right after spawn, before the first poll).
  workspace: {
    kind: 'worktree' | 'existing';
    repo: string;
    repoPath: string;
    branch: string;
    cwd: string;
    gitState?: 'dirty' | 'unmerged' | 'synced';
  };
  status: AgentStatus;
  pendingApprovals: number;
  controlMode: ControlMode;
}

export interface ClosePreview {
  kind: 'worktree' | 'existing';
  uncommitted: string;
  unmerged: string;
}

// corrId is an optional client-supplied correlation token echoed back on `ack`.
export type ClientMsg =
  | { t: 'subscribe'; agentId: string; channels?: Channel[]; sinceSeq?: number; corrId?: string }
  | { t: 'list_agents'; corrId?: string } // rail discovery: which agents exist + their metadata
  | { t: 'list_sessions'; corrId?: string } // Resume picker: the resumable-session catalog
  // Resume a session (Resume picker). For a Tandem-owned session only sessionId
  // is needed; for an external one, `agent`+`cwd` (from the catalog) say how/where
  // to relaunch it.
  | { t: 'resume_session'; sessionId: string; agent?: string; cwd?: string; corrId?: string }
  | { t: 'enter_terminal'; agentId: string; interrupt?: boolean; corrId?: string }
  | { t: 'leave_terminal'; agentId: string; corrId?: string }
  | { t: 'unsubscribe'; agentId: string; channels?: Channel[]; corrId?: string }
  | { t: 'prompt'; agentId: string; text?: string; blocks?: PromptBlock[]; corrId?: string }
  | { t: 'input'; agentId: string; bytesB64: string; corrId?: string }
  | { t: 'resize'; agentId: string; cols: number; rows: number; corrId?: string }
  | { t: 'permission_response'; agentId: string; reqId: string; optionId: string; corrId?: string }
  | { t: 'interrupt'; agentId: string; corrId?: string }
  | { t: 'set_mode'; agentId: string; modeId: string; corrId?: string }
  | { t: 'set_config_option'; agentId: string; configId: string; value: string | boolean; corrId?: string }
  | { t: 'spawn_agent'; spec: SpawnSpec; corrId?: string }
  | { t: 'get_spawn_options'; agent: string; cwd: string; corrId?: string }
  | { t: 'get_close_preview'; agentId: string; corrId?: string }
  | { t: 'close_agent'; agentId: string; force?: boolean; deleteWorktree?: boolean; corrId?: string }
  | { t: 'merge_back'; agentId: string; mode: 'merge' | 'pr'; corrId?: string }
  | { t: 'browser_control'; agentId: string; action: 'grab' | 'release'; corrId?: string }
  // Phase 5: forward the user's mouse/key/wheel to the browser (owner=user only).
  | { t: 'browser_input'; agentId: string; event: BrowserInputWire; corrId?: string }
  | { t: 'list_dirs'; corrId?: string }; // Phase 2: repo discovery for the spawn palette

// The normalized user-input shape carried by `browser_input` (mirrors
// SharedBrowser.BrowserInputEvent — CDP Input.* under the hood).
export interface BrowserInputWire {
  kind: 'mousemove' | 'mousedown' | 'mouseup' | 'click' | 'wheel' | 'keydown' | 'keyup' | 'text';
  x?: number;
  y?: number;
  button?: 'left' | 'middle' | 'right';
  buttons?: number;
  clickCount?: number;
  deltaX?: number;
  deltaY?: number;
  key?: string;
  code?: string;
  keyCode?: number;
  text?: string;
}

// A wire-serialized event: raw_pty's bytes become base64 (a future optimization is
// real binary framing — see docs/ws-protocol.md; for now it rides in JSON).
export type WireEvent = Exclude<AgentEvent, { kind: 'raw_pty' }> | { kind: 'raw_pty'; dataB64: string };

export type ServerMsg =
  | { t: 'snapshot'; agentId: string; seq: number; transcript: { seq: number; event: WireEvent }[]; status: AgentStatus; controlMode: ControlMode; pendingApprovals: Approval[] }
  | { t: 'event'; agentId: string; seq: number; event: WireEvent }
  | { t: 'ack'; corrId?: string; agentId?: string; error?: string }
  | { t: 'agent_closed'; agentId: string }
  | { t: 'agents'; corrId?: string; agents: AgentSummary[] } // reply to list_agents
  | { t: 'dirs'; corrId?: string; dirs: RepoInfo[] } // reply to list_dirs
  | { t: 'spawn_options'; corrId?: string; options?: SpawnOptions; error?: string }
  | { t: 'close_preview'; corrId?: string; preview?: ClosePreview; error?: string }
  | { t: 'sessions'; corrId?: string; catalog: ResumeCatalog } // reply to list_sessions
  // Phase 5 browser channel. `browser_frame` is a CDP screencast frame (JSON +
  // base64; real binary framing is a future optimization). `browser_state`
  // announces lifecycle + control-owner so the UI knows when to show the pane.
  | { t: 'browser_frame'; agentId: string; dataB64: string; meta: { deviceWidth: number; deviceHeight: number; offsetTop: number; timestamp?: number } }
  | { t: 'browser_state'; agentId: string; active: boolean; controlOwner: 'agent' | 'user' };
