// The browser wire contract. The Go server implementation lives in internal/wsserver.
// The browser holds NO authoritative state; every shape here is a projection of
// what the daemon sends. Kept as a hand-maintained copy so the UI has no build
// dependency on the daemon package.

export type AgentStatus = 'idle' | 'working' | 'blocked' | 'error';
export type ControlMode = 'transcript' | 'switching' | 'terminal';
export type ToolStatus = 'pending' | 'running' | 'done' | 'error' | 'cancelled';
export type Channel = 'transcript' | 'pty' | 'terminals' | 'browser' | 'status';

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
  category?: string;
  type: 'select' | 'boolean';
  currentValue: string | boolean;
  options?: SessionConfigSelectOption[];
}

export interface SlashCommand {
  name: string;
  description?: string;
  input?: string;
}

export interface ImageAssetRef {
  assetId: string;
  mimeType: string;
  name?: string;
}

export type PromptBlock =
  | { type: 'text'; text: string }
  | ({ type: 'image' } & ImageAssetRef);

export type AgentEvent =
  | { kind: 'user_message'; text?: string; blocks?: PromptBlock[] }
  | { kind: 'message_chunk'; text: string }
  | { kind: 'thought_chunk'; text: string }
  | { kind: 'tool_call'; id: string; title: string; status: ToolStatus; content?: unknown; rawInput?: unknown }
  | { kind: 'tool_call_update'; id: string; status?: ToolStatus; content?: unknown }
  | { kind: 'plan'; entries: { label: string; status: 'pending' | 'in_progress' | 'done' }[] }
  | { kind: 'terminal_output'; termId: string; chunk: string; truncated: boolean }
  | { kind: 'permission_request'; reqId: string; toolCallId: string; title: string; options: { optionId: string; name: string }[] }
  | { kind: 'status'; status: AgentStatus }
  | { kind: 'error'; message: string }
  | { kind: 'takeover_request'; reqId: string; reason: string }
  | { kind: 'session_config'; modes: SessionModeState | null; configOptions: SessionConfigOption[] }
  | { kind: 'available_commands'; commands: SlashCommand[] }
  | { kind: 'prompt_capabilities'; image: boolean }
  | { kind: 'usage'; used: number; size: number; cost?: { amount: number; currency: string } | null }
  | { kind: 'control_state'; mode: ControlMode };

// On the wire raw_pty/shell_pty bytes are base64; everything else is a plain
// AgentEvent. shell_pty/shell_exit carry the user escape-hatch shell (Terminal
// tab), kept separate from the agent's raw_pty stream.
export type WireEvent =
  | AgentEvent
  | { kind: 'raw_pty'; dataB64: string }
  | { kind: 'shell_pty'; dataB64: string }
  | { kind: 'shell_exit'; message: string };

export interface Approval {
  reqId: string;
  toolCallId: string;
  title: string;
  options: { optionId: string; name: string }[];
}

export interface RepoInfo {
  path: string;
  name: string;
  currentBranch: string;
  dirty: boolean;
  hasLiveAgent: boolean;
}

export type GitRefKind = 'local-branch' | 'remote-branch' | 'tag' | 'detached';
export interface GitRefInfo {
  ref: string;
  displayName: string;
  kind: GitRefKind;
  commit: string;
  subject?: string;
  updatedAt?: string;
  upstream?: string;
  ahead?: number;
  behind?: number;
  checkedOutAt?: string;
  isCurrent: boolean;
  isDefault: boolean;
  tandem?: {
    agentId: string;
    agentName: string;
    integrationRef?: string;
    integrationKind?: 'local-branch' | 'remote-branch' | 'detached';
    live: boolean;
    closed: boolean;
  };
}

export interface AgentSummary {
  id: string;
  name: string;
  agent?: string;
  workspace: {
    kind: 'worktree' | 'existing';
    repo: string;
    repoPath: string;
    branch: string;
    cwd: string;
    gitState?: 'dirty' | 'ahead' | 'behind' | 'diverged' | 'merged' | 'synced' | 'target_missing';
    ahead?: number;
    behind?: number;
    targetRef?: string;
    targetKind?: 'local-branch' | 'remote-branch' | 'detached';
    startCommit?: string;
  };
  status: AgentStatus;
  pendingApprovals: number;
  controlMode: ControlMode;
  // Stable adapter kind (does not change across an ACP↔CLI handoff) and whether
  // the Chat tab should offer the ACP/CLI switch for this agent.
  adapter: 'acp' | 'pty';
  canHandoff: boolean;
}

// A resumable coding-agent session for the Resume picker — either a session
// Tandem spawned ('tandem', with full linkage) or one discovered via an ACP
// agent's session/list ('external'). `sessionId` == the agent's CLI --resume id.
export interface ResumableSession {
  sessionId: string;
  source: 'tandem' | 'external';
  agent: string;
  adapter: 'acp' | 'pty';
  cwd: string;
  title?: string;
  updatedAt?: string;
  agentId?: string;
  agentName?: string;
  branch?: string;
  live?: boolean;
  closed?: boolean;
  status?: AgentStatus;
}
export interface ResumeAdapterInfo {
  agent: string;
  supportsList: boolean;
}
export interface ResumeCatalog {
  sessions: ResumableSession[];
  adapters: ResumeAdapterInfo[];
}

export type Workspace =
  | {
      kind: 'worktree';
      repo: string;
      branch?: string;
      branchMode?: 'create' | 'attach';
      source?: { ref: string; commit?: string };
      integration?: { kind: 'local-branch' | 'remote-branch' | 'detached'; ref: string };
      baseRef?: string;
    }
  | { kind: 'existing'; cwd: string };

export interface SpawnSpec {
  adapter: 'acp' | 'pty';
  agent?: string; // configured agent definition; applies to both ACP and direct Terminal launches
  profile?: string; // configured profile id; agent remains populated for older daemons
  terminalArgs?: string[]; // argv entries appended to a configured direct-terminal launch
  workspace: Workspace;
  name?: string;
  task?: string;
  sessionConfig?: { modeId?: string; configOptions?: Record<string, string | boolean> };
  preset?: string;
}

export interface AgentCatalogEntry {
  id: string;
  name: string;
  hasAcp: boolean;
  hasTerminal: boolean;
  canResume: boolean;
}
export interface AgentProfileEntry {
  id: string;
  agent: string;
  name: string;
  acpArgs: string[];
  terminalArgs: string[];
}
export interface AgentCatalog {
  defaultAgent: string;
  defaultProfile?: string;
  agents: AgentCatalogEntry[];
  profiles: AgentProfileEntry[];
}

export interface SpawnOptions {
  modes: SessionModeState | null;
  configOptions: SessionConfigOption[];
}

export interface ClosePreview {
  kind: 'worktree' | 'existing';
  uncommitted: string;
  unmerged: string;
  targetRef?: string;
  ahead?: number;
  behind?: number;
}

export type ClientMsg =
  | { t: 'subscribe'; agentId: string; channels?: Channel[]; sinceSeq?: number; corrId?: string }
  | { t: 'unsubscribe'; agentId: string; channels?: Channel[]; corrId?: string }
  | { t: 'prompt'; agentId: string; text?: string; blocks?: PromptBlock[]; corrId?: string }
  | { t: 'input'; agentId: string; bytesB64: string; corrId?: string }
  | { t: 'resize'; agentId: string; cols: number; rows: number; corrId?: string }
  | { t: 'permission_response'; agentId: string; reqId: string; optionId: string; corrId?: string }
  | { t: 'interrupt'; agentId: string; corrId?: string }
  | { t: 'set_mode'; agentId: string; modeId: string; corrId?: string }
  | { t: 'set_config_option'; agentId: string; configId: string; value: string | boolean; corrId?: string }
  | { t: 'spawn_agent'; spec: SpawnSpec; corrId?: string }
  | { t: 'get_spawn_options'; agent: string; profile?: string; acpArgs?: string[]; cwd: string; corrId?: string }
  | { t: 'get_close_preview'; agentId: string; corrId?: string }
  | { t: 'close_agent'; agentId: string; force?: boolean; deleteWorktree?: boolean; corrId?: string }
  | { t: 'merge_back'; agentId: string; mode: 'merge' | 'pr'; corrId?: string }
  | { t: 'browser_control'; agentId: string; action: 'grab' | 'release'; corrId?: string }
  | { t: 'browser_input'; agentId: string; event: BrowserInputWire; corrId?: string }
  | { t: 'list_dirs'; corrId?: string }
  | { t: 'list_git_refs'; repo: string; corrId?: string }
  | { t: 'list_agents'; corrId?: string }
  | { t: 'list_agent_catalog'; corrId?: string }
  | { t: 'list_sessions'; corrId?: string }
  | { t: 'resume_session'; sessionId: string; agent?: string; cwd?: string; corrId?: string }
  | { t: 'enter_terminal'; agentId: string; interrupt?: boolean; corrId?: string }
  | { t: 'leave_terminal'; agentId: string; corrId?: string }
  | { t: 'shell_open'; agentId: string; cols: number; rows: number; corrId?: string }
  | { t: 'shell_input'; agentId: string; bytesB64: string; corrId?: string }
  | { t: 'shell_resize'; agentId: string; cols: number; rows: number; corrId?: string }
  | { t: 'shell_close'; agentId: string; corrId?: string };

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

export type ServerMsg =
  | { t: 'snapshot'; agentId: string; seq: number; transcript: { seq: number; event: WireEvent }[]; status: AgentStatus; controlMode: ControlMode; pendingApprovals: Approval[] }
  | { t: 'event'; agentId: string; seq: number; event: WireEvent }
  | { t: 'ack'; corrId?: string; agentId?: string; error?: string }
  | { t: 'agent_closed'; agentId: string }
  | { t: 'agents'; corrId?: string; agents: AgentSummary[] }
  | { t: 'agent_catalog'; corrId?: string; catalog: AgentCatalog }
  | { t: 'dirs'; corrId?: string; dirs: RepoInfo[] }
  | { t: 'git_refs'; corrId?: string; refs?: GitRefInfo[]; error?: string }
  | { t: 'spawn_options'; corrId?: string; options?: SpawnOptions; error?: string }
  | { t: 'close_preview'; corrId?: string; preview?: ClosePreview; error?: string }
  | { t: 'sessions'; corrId?: string; catalog: ResumeCatalog }
  | { t: 'browser_frame'; agentId: string; dataB64: string; meta: { deviceWidth: number; deviceHeight: number; offsetTop: number; timestamp?: number } }
  | { t: 'browser_state'; agentId: string; active: boolean; controlOwner: 'agent' | 'user' };
