// The wire contract — mirrored from daemon/src/types.ts (docs/ws-protocol.md).
// The browser holds NO authoritative state; every shape here is a projection of
// what the daemon sends. Kept as a hand-maintained copy so the UI has no build
// dependency on the daemon package.

export type AgentStatus = 'idle' | 'working' | 'blocked' | 'error';
export type ToolStatus = 'pending' | 'running' | 'done' | 'error' | 'cancelled';
export type Channel = 'transcript' | 'pty' | 'terminals' | 'browser' | 'status';

export type AgentEvent =
  | { kind: 'user_message'; text: string }
  | { kind: 'message_chunk'; text: string }
  | { kind: 'thought_chunk'; text: string }
  | { kind: 'tool_call'; id: string; title: string; status: ToolStatus; content?: unknown }
  | { kind: 'tool_call_update'; id: string; status?: ToolStatus }
  | { kind: 'plan'; entries: { label: string; status: 'pending' | 'in_progress' | 'done' }[] }
  | { kind: 'terminal_output'; termId: string; chunk: string; truncated: boolean }
  | { kind: 'permission_request'; reqId: string; toolCallId: string; title: string; options: { optionId: string; name: string }[] }
  | { kind: 'status'; status: AgentStatus }
  | { kind: 'error'; message: string }
  | { kind: 'takeover_request'; reqId: string; reason: string };

// On the wire raw_pty bytes are base64; everything else is a plain AgentEvent.
export type WireEvent = AgentEvent | { kind: 'raw_pty'; dataB64: string };

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

export interface AgentSummary {
  id: string;
  name: string;
  workspace: { kind: 'worktree' | 'existing'; repo: string; repoPath: string; branch: string; cwd: string };
  status: AgentStatus;
  pendingApprovals: number;
}

export type Workspace =
  | { kind: 'worktree'; repo: string; branch?: string; baseRef?: string }
  | { kind: 'existing'; cwd: string };

export interface SpawnSpec {
  adapter: 'acp' | 'pty';
  workspace: Workspace;
  name?: string;
  task?: string;
  preset?: string;
}

export type ClientMsg =
  | { t: 'subscribe'; agentId: string; channels?: Channel[]; sinceSeq?: number; corrId?: string }
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
  | { t: 'browser_input'; agentId: string; event: BrowserInputWire; corrId?: string }
  | { t: 'list_dirs'; corrId?: string }
  | { t: 'list_agents'; corrId?: string };

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
  | { t: 'snapshot'; agentId: string; seq: number; transcript: { seq: number; event: WireEvent }[]; status: AgentStatus; pendingApprovals: Approval[] }
  | { t: 'event'; agentId: string; seq: number; event: WireEvent }
  | { t: 'ack'; corrId?: string; agentId?: string; error?: string }
  | { t: 'agent_closed'; agentId: string }
  | { t: 'agents'; corrId?: string; agents: AgentSummary[] }
  | { t: 'dirs'; corrId?: string; dirs: RepoInfo[] }
  | { t: 'browser_frame'; agentId: string; dataB64: string; meta: { deviceWidth: number; deviceHeight: number; offsetTop: number; timestamp?: number } }
  | { t: 'browser_state'; agentId: string; active: boolean; controlOwner: 'agent' | 'user' };
