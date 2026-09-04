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
  | ({ type: 'image' } & ImageAssetRef)
  | { type: 'quote'; refSeq: number; role: string; quote: string; comment: string };

export interface QueuedPrompt {
  id: string;
  blocks: PromptBlock[];
  queuedAt: string;
}

// A durable, mutable draft comment anchored to one transcript row (the
// representative seq encoded in the row's React key). Persisted in the
// daemon so the review tray syncs across devices; consumed (cleared) once
// sent as `quote` prompt blocks. docs/transcript-annotations.md.
export interface Annotation {
  id: string;
  agentId: string;
  seq: number;
  role: 'assistant' | 'user' | 'thought' | 'tool';
  quote: string;
  comment: string;
  createdAt: number;
  updatedAt: number;
}

export type AgentEvent =
  | { kind: 'user_message'; text?: string; blocks?: PromptBlock[] }
  // parentId (when present) is the toolCallId of the tool call that spawned the
  // emitter — e.g. a subagent's parent Task call — normalized by the daemon from
  // an agent-specific `_meta` path. Absent for top-level activity. Lets the
  // transcript group subagent output under its spawn instead of interleaving it.
  | { kind: 'message_chunk'; text: string; parentId?: string }
  | { kind: 'thought_chunk'; text: string; parentId?: string }
  | { kind: 'tool_call'; id: string; title: string; status: ToolStatus; content?: unknown; rawInput?: unknown; toolKind?: string; parentId?: string }
  | { kind: 'tool_call_update'; id: string; status?: ToolStatus; content?: unknown; title?: string; rawInput?: unknown; toolKind?: string; parentId?: string }
  | { kind: 'plan'; entries: { label: string; status: 'pending' | 'in_progress' | 'done' }[] }
  | { kind: 'terminal_output'; termId: string; chunk: string; truncated: boolean }
  | { kind: 'permission_request'; reqId: string; toolCallId: string; title: string; options: { optionId: string; name: string }[] }
  | { kind: 'status'; status: AgentStatus }
  | { kind: 'error'; message: string }
  | { kind: 'takeover_request'; reqId: string; reason: string }
  | { kind: 'takeover_resolved'; reqId: string }
  | { kind: 'session_config'; modes: SessionModeState | null; configOptions: SessionConfigOption[] }
  | { kind: 'available_commands'; commands: SlashCommand[] }
  | { kind: 'prompt_capabilities'; image: boolean }
  | { kind: 'aside_capabilities'; fork: boolean }
  | { kind: 'aside_started'; asideId: string; question: string }
  | { kind: 'aside_event'; asideId: string; event: AgentEvent }
  | { kind: 'aside_completed'; asideId: string; stopReason?: string; error?: string }
  | { kind: 'usage'; used: number; size: number; cost?: { amount: number; currency: string } | null; updatedAt?: number }
  | { kind: 'control_state'; mode: ControlMode }
  | { kind: 'prompt_queued'; promptId: string; blocks: PromptBlock[]; queuedAt: string; position: number }
  | { kind: 'prompt_started'; promptId: string; blocks: PromptBlock[]; queuedAt: string }
  | { kind: 'prompt_removed'; promptId: string; blocks: PromptBlock[]; queuedAt: string }
  | { kind: 'audio_preference'; enabled: boolean }
  | { kind: 'audio_state'; state: 'rendering' | 'ready' | 'error'; seq: number; message?: string; durationMs?: number };

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
    gitState?: 'dirty' | 'ahead' | 'behind' | 'diverged' | 'merged' | 'synced' | 'target_missing' | 'unknown';
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
  profile?: {
    id?: string;
    model?: string;
    effort?: string;
    permission?: string;
    snapshot?: string;
  };
}

// A resumable coding-agent session for the Resume picker — either a session
// Tandem spawned ('tandem', with full linkage), ACP-discovered ('acp'), or
// transcript-imported ('history'). Identity is `(agent, sessionId)`.
export interface ResumableSession {
  sessionId: string;
  source: 'tandem' | 'acp' | 'history';
  agent: string;
  adapter?: 'acp' | 'pty';
  cwd: string;
  // Source repository the session belongs to — the Resume palette's grouping
  // identity (repoPath) and its display label (repo).
  repo?: string;
  repoPath?: string;
  title?: string;
  updatedAt?: string;
  agentId?: string;
  agentName?: string;
  branch?: string;
  live?: boolean;
  closed?: boolean;
  status?: AgentStatus;
  resumable: boolean;
  historyOnly?: boolean;
  resumeError?: string;
}
export interface ResumeAdapterInfo {
  agent: string;
  supportsList: boolean;
}
export interface ResumeCatalog {
  sessions: ResumableSession[];
  adapters: ResumeAdapterInfo[];
}
export interface HistoryHighlight {
  start: number;
  end: number;
}
export interface HistoryExcerpt {
  text: string;
  highlights: HistoryHighlight[];
}
export interface SessionSearchHit {
  entryId: string;
  role?: string;
  kind?: string;
  timestamp?: string;
  match: HistoryExcerpt;
  before?: HistoryExcerpt;
  after?: HistoryExcerpt;
}
export interface SessionSearchResult {
  session: ResumableSession;
  score: number;
  hits: SessionSearchHit[];
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
  harness?: string; // configured harness id (agent launch variant); agent remains populated for older daemons
  terminalArgs?: string[]; // argv entries appended to a configured direct-terminal launch
  workspace: Workspace;
  name?: string;
  task?: string;
  sessionConfig?: { modeId?: string; configOptions?: Record<string, string | boolean> };
  preset?: string;
  // Human-facing profile identity + browser snapshot seed. The daemon resolves
  // or creates a Profile from these and fills in `id`; snapshot '' = fresh state.
  profile?: { id?: string; model?: string; effort?: string; permission?: string; snapshot?: string };
}

// BrowserSnapshot is a captured, named browser user-data snapshot used to seed a
// new agent's browser at spawn.
export interface BrowserSnapshot {
  id: string;
  name: string;
  kind: string;
  ref: string;
  createdAt: number;
}

export interface AutomationJob {
  id: string;
  repositoryId: string;
  scriptPath: string;
  name: string;
  cron: string;
  timezone: string;
  browserSnapshotId: string;
  defaultAgentProfile: string;
  wakePrompt: string;
  wakeSuppression: string;
  concurrency: string;
  manifestHash: string;
  enabled: boolean;
  createdAt: number;
  updatedAt: number;
}

export interface AutomationRun {
  id: string;
  jobId?: string;
  repositoryId: string;
  scriptPath: string;
  trigger: string;
  sourceHash: string;
  browserSnapshotId: string;
  browserCloneId: string;
  scheduledAt?: number;
  startedAt: number;
  completedAt?: number;
  outcome: string;
  reason: string;
  exitCode?: number;
  stdout: string;
  stderr: string;
  report?: unknown;
  error?: string;
}

// Profile is a daemon-owned, auto-created, renamable bundle of launch settings.
export interface Profile {
  id: string;
  name: string;
  autoNamed: boolean;
  agent: string;
  harness: string;
  model: string;
  effort: string;
  permission: string;
  snapshotId: string;
  createdAt: number;
  lastUsedAt: number;
}

export interface AgentCatalogEntry {
  id: string;
  name: string;
  hasAcp: boolean;
  hasTerminal: boolean;
  canResume: boolean;
}
export interface AgentHarnessEntry {
  id: string;
  agent: string;
  name: string;
  acpArgs: string[];
  terminalArgs: string[];
}
export interface AgentCatalog {
  defaultAgent: string;
  defaultHarness?: string;
  agents: AgentCatalogEntry[];
  harnesses: AgentHarnessEntry[];
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
  notGitRepo?: boolean;
}
export interface WorkspaceDiff {
  uncommitted: string;
  committed: string;
  targetRef?: string;
}

export interface WorkspaceEntry {
  path: string;
  isDir: boolean;
}

export type ClientMsg =
  | { t: 'subscribe'; agentId: string; channels?: Channel[]; sinceSeq?: number; corrId?: string }
  | { t: 'unsubscribe'; agentId: string; channels?: Channel[]; corrId?: string }
  | { t: 'prompt'; agentId: string; text?: string; blocks?: PromptBlock[]; corrId?: string }
  | { t: 'aside'; agentId: string; text: string; corrId?: string }
  | { t: 'remove_queued_prompt'; agentId: string; promptId: string; corrId?: string }
  | { t: 'clear_prompt_queue'; agentId: string; corrId?: string }
  | { t: 'add_annotation'; agentId: string; seq: number; role: string; quote: string; comment: string; corrId?: string }
  | { t: 'update_annotation'; agentId: string; id: string; comment: string; corrId?: string }
  | { t: 'delete_annotation'; agentId: string; id: string; corrId?: string }
  | { t: 'clear_annotations'; agentId: string; corrId?: string }
  | { t: 'interrupt_and_clear_queue'; agentId: string; corrId?: string }
  | { t: 'input'; agentId: string; bytesB64: string; corrId?: string }
  | { t: 'resize'; agentId: string; cols: number; rows: number; corrId?: string }
  | { t: 'permission_response'; agentId: string; reqId: string; optionId: string; corrId?: string }
  | { t: 'interrupt'; agentId: string; corrId?: string }
  | { t: 'set_mode'; agentId: string; modeId: string; corrId?: string }
  | { t: 'set_config_option'; agentId: string; configId: string; value: string | boolean; corrId?: string }
  | { t: 'set_audio_enabled'; agentId: string; enabled: boolean; corrId?: string }
  | { t: 'set_audio_focus'; agentId: string; focused: boolean; corrId?: string }
  | { t: 'spawn_agent'; spec: SpawnSpec; corrId?: string }
  | { t: 'get_spawn_options'; agent: string; harness?: string; acpArgs?: string[]; cwd: string; corrId?: string }
  | { t: 'capture_snapshot'; agentId: string; name: string; corrId?: string }
  | { t: 'list_snapshots'; corrId?: string }
  | { t: 'delete_snapshot'; id: string; corrId?: string }
  | { t: 'list_profiles'; project?: string; corrId?: string }
  | { t: 'rename_profile'; id: string; name: string; project?: string; corrId?: string }
  | { t: 'rename_agent'; agentId: string; name: string; corrId?: string }
  | { t: 'delete_profile'; id: string; project?: string; corrId?: string }
  | { t: 'get_close_preview'; agentId: string; corrId?: string }
  | { t: 'get_diff'; agentId: string; corrId?: string }
  | { t: 'close_agent'; agentId: string; force?: boolean; deleteWorktree?: boolean; corrId?: string }
  | { t: 'merge_back'; agentId: string; mode: 'merge' | 'pr'; corrId?: string }
  | { t: 'browser_control'; agentId: string; action: 'grab' | 'release'; corrId?: string }
  | { t: 'restart_browser'; agentId: string; snapshotId?: string; corrId?: string }
  | { t: 'browser_input'; agentId: string; event: BrowserInputWire; corrId?: string }
  | { t: 'list_dirs'; corrId?: string }
  | { t: 'list_workspace_entries'; agentId: string; path: string; corrId?: string }
  | { t: 'list_git_refs'; repo: string; corrId?: string }
  | { t: 'list_agents'; corrId?: string }
  | { t: 'list_agent_catalog'; corrId?: string }
  | { t: 'list_sessions'; corrId?: string }
  | { t: 'list_automation'; repositoryId?: string; corrId?: string }
  | { t: 'set_automation_enabled'; id: string; enabled: boolean; corrId?: string }
  | { t: 'search_sessions'; query: string; limit?: number; maxHitsPerSession?: number; corrId?: string }
  | { t: 'refresh_history'; agent: string; reindex?: boolean; corrId?: string }
  | { t: 'history_status'; agent?: string; corrId?: string }
  | { t: 'resume_session'; sessionId: string; source: ResumableSession['source']; agent: string; cwd?: string; corrId?: string }
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
  autoRepeat?: boolean;
  text?: string;
}

export type ServerMsg =
  | { t: 'snapshot'; agentId: string; seq: number; transcript: { seq: number; event: WireEvent }[]; status: AgentStatus; controlMode: ControlMode; pendingApprovals: Approval[]; queuedPrompts: QueuedPrompt[]; annotations?: Annotation[]; audioReadySeqs?: number[]; audioReady?: { seq: number; durationMs: number }[] }
  | { t: 'prompt_queue'; agentId: string; queuedPrompts: QueuedPrompt[] }
  | { t: 'annotations'; agentId: string; annotations: Annotation[] }
  | { t: 'event'; agentId: string; seq: number; event: WireEvent }
  | { t: 'ack'; corrId?: string; agentId?: string; error?: string; promptId?: string; disposition?: 'started' | 'queued'; position?: number; cleared?: number }
  | { t: 'agent_closed'; agentId: string }
  | { t: 'agents'; corrId?: string; agents: AgentSummary[] }
  | { t: 'agent_catalog'; corrId?: string; catalog: AgentCatalog }
  | { t: 'dirs'; corrId?: string; dirs: RepoInfo[] }
  | { t: 'workspace_entries'; corrId?: string; entries?: WorkspaceEntry[]; error?: string }
  | { t: 'git_refs'; corrId?: string; refs?: GitRefInfo[]; error?: string }
  | { t: 'spawn_options'; corrId?: string; options?: SpawnOptions; error?: string }
  | { t: 'snapshots'; corrId?: string; snapshots?: BrowserSnapshot[]; captured?: BrowserSnapshot; error?: string }
  | { t: 'profiles'; corrId?: string; profiles?: Profile[]; recent?: string[]; project?: string; error?: string }
  | { t: 'close_preview'; corrId?: string; preview?: ClosePreview; error?: string }
  | { t: 'diff'; corrId?: string; diff?: WorkspaceDiff; error?: string }
  | { t: 'sessions'; corrId?: string; catalog: ResumeCatalog }
  | { t: 'automation'; corrId?: string; jobs?: AutomationJob[]; runs?: AutomationRun[]; error?: string }
  | { t: 'session_search'; corrId?: string; query?: string; results?: SessionSearchResult[]; error?: string }
  | { t: 'browser_frame'; agentId: string; dataB64: string; meta: { deviceWidth: number; deviceHeight: number; offsetTop: number; timestamp?: number } }
  | { t: 'browser_state'; agentId: string; active: boolean; controlOwner: 'agent' | 'user' };
