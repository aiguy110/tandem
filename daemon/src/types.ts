// Normalized agent vocabulary — the boundary every adapter emits into.
// Mirrors docs/agent-adapter.md (ACP-shaped, since ACP is the reference).

export type AgentStatus = 'idle' | 'working' | 'blocked' | 'error';

export type AgentEvent =
  | { kind: 'message_chunk'; text: string }
  | { kind: 'thought_chunk'; text: string }
  | { kind: 'tool_call'; id: string; title: string; status: 'pending' | 'running' | 'done' | 'error'; content?: unknown }
  | { kind: 'tool_call_update'; id: string; status?: string }
  | { kind: 'plan'; entries: { label: string; status: 'pending' | 'in_progress' | 'done' }[] }
  | { kind: 'terminal_output'; termId: string; chunk: string; truncated: boolean }
  | { kind: 'permission_request'; reqId: string; toolCallId: string; title: string; options: { optionId: string; name: string }[] }
  | { kind: 'status'; status: AgentStatus }
  | { kind: 'error'; message: string }
  | { kind: 'raw_pty'; data: Uint8Array };

export interface SpawnOpts {
  cwd?: string;
  cmd?: string;
  args?: string[];
}

export interface AgentAdapter {
  readonly id: string;
  readonly capabilities: { structured: boolean; terminals: boolean; loadSession: boolean; fs: boolean };
  readonly pid?: number;

  spawn(opts: SpawnOpts): Promise<void>;
  prompt(text: string): Promise<void>;
  sendInput(bytes: Uint8Array): void;
  respondPermission(reqId: string, optionId: string): void;
  interrupt(): void;
  dispose(): Promise<void>;

  readonly events: AsyncIterable<AgentEvent>;
}
