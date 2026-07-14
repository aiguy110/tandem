// Normalized agent vocabulary — the boundary every adapter emits into.
// Mirrors docs/agent-adapter.md (ACP-shaped, since ACP is the reference).
//
// This file is also the single home for the WS protocol envelopes
// (docs/ws-protocol.md) and the SpawnSpec (docs/spawn-and-workspaces.md), so the
// normalized boundary and the wire contract live in one place. The WS layer only
// ever ships these shapes — it must never leak ACP JSON-RPC.

export type AgentStatus = 'idle' | 'working' | 'blocked' | 'error';

export type ToolStatus = 'pending' | 'running' | 'done' | 'error' | 'cancelled';

export type AgentEvent =
  | { kind: 'message_chunk'; text: string }
  | { kind: 'thought_chunk'; text: string }
  | { kind: 'tool_call'; id: string; title: string; status: ToolStatus; content?: unknown }
  | { kind: 'tool_call_update'; id: string; status?: ToolStatus }
  | { kind: 'plan'; entries: { label: string; status: 'pending' | 'in_progress' | 'done' }[] }
  | { kind: 'terminal_output'; termId: string; chunk: string; truncated: boolean }
  | { kind: 'permission_request'; reqId: string; toolCallId: string; title: string; options: { optionId: string; name: string }[] }
  | { kind: 'status'; status: AgentStatus }
  | { kind: 'error'; message: string }
  // Agent-initiated browser handoff (docs/browser.md Attention): the agent called
  // the Tandem-control MCP's browser.request_takeover. A normalized event so it
  // logs, replays, and folds into the attention rail. `reason` is human-facing.
  | { kind: 'takeover_request'; reqId: string; reason: string }
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
  readonly capabilities: { structured: boolean; terminals: boolean; loadSession: boolean; fs: boolean };
  readonly pid?: number;
  // The ACP sessionId once established — persisted for restore (D11). Undefined
  // for adapters without a session concept (pty).
  readonly acpSessionId?: string;

  spawn(opts: SpawnOpts, services?: ClientServices): Promise<void>;
  // Resolves with the ACP stopReason for the turn (end_turn | max_tokens |
  // max_turn_requests | refusal | cancelled). Non-ACP adapters return 'end_turn'.
  prompt(text: string): Promise<string>;
  sendInput(bytes: Uint8Array): void;
  resize?(cols: number, rows: number): void;
  respondPermission(reqId: string, optionId: string): void;
  interrupt(): void;
  loadSession?(sessionId: string): Promise<void>; // only if capabilities.loadSession
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
  workspace: Workspace;
  name?: string; // auto: web-1, api-2…
  task?: string; // optional initial prompt, dispatched on spawn
  preset?: string; // reserved; single default agent for now
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
  // `repo` is a display basename; `repoPath` is the full source-repo path (the
  // spawn origin, needed for "sibling" spawns); `cwd` is this agent's checkout.
  workspace: { kind: 'worktree' | 'existing'; repo: string; repoPath: string; branch: string; cwd: string };
  status: AgentStatus;
  pendingApprovals: number;
}

// corrId is an optional client-supplied correlation token echoed back on `ack`.
export type ClientMsg =
  | { t: 'subscribe'; agentId: string; channels?: Channel[]; sinceSeq?: number; corrId?: string }
  | { t: 'list_agents'; corrId?: string } // rail discovery: which agents exist + their metadata
  | { t: 'unsubscribe'; agentId: string; channels?: Channel[]; corrId?: string }
  | { t: 'prompt'; agentId: string; text: string; corrId?: string }
  | { t: 'input'; agentId: string; bytesB64: string; corrId?: string }
  | { t: 'resize'; agentId: string; cols: number; rows: number; corrId?: string }
  | { t: 'permission_response'; agentId: string; reqId: string; optionId: string; corrId?: string }
  | { t: 'interrupt'; agentId: string; corrId?: string }
  | { t: 'spawn_agent'; spec: SpawnSpec; corrId?: string }
  | { t: 'close_agent'; agentId: string; force?: boolean; corrId?: string }
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
  | { t: 'snapshot'; agentId: string; seq: number; transcript: { seq: number; event: WireEvent }[]; status: AgentStatus; pendingApprovals: Approval[] }
  | { t: 'event'; agentId: string; seq: number; event: WireEvent }
  | { t: 'ack'; corrId?: string; agentId?: string; error?: string }
  | { t: 'agent_closed'; agentId: string }
  | { t: 'agents'; corrId?: string; agents: AgentSummary[] } // reply to list_agents
  | { t: 'dirs'; corrId?: string; dirs: RepoInfo[] } // reply to list_dirs
  // Phase 5 browser channel. `browser_frame` is a CDP screencast frame (JSON +
  // base64; real binary framing is a future optimization). `browser_state`
  // announces lifecycle + control-owner so the UI knows when to show the pane.
  | { t: 'browser_frame'; agentId: string; dataB64: string; meta: { deviceWidth: number; deviceHeight: number; offsetTop: number; timestamp?: number } }
  | { t: 'browser_state'; agentId: string; active: boolean; controlOwner: 'agent' | 'user' };
