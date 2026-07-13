// ACP adapter: the daemon is the ACP *client*. Speaks JSON-RPC 2.0 over the
// agent subprocess's stdio, newline-delimited (JSON.stringify(msg) + "\n").
//
// Pinned against @agentclientprotocol/sdk 1.2.1 (protocol v1) and exercised
// against the real @agentclientprotocol/claude-agent-acp — see docs/acp-notes.md.
//
// Key shape facts:
//   * session/update variants FLATTEN their payload next to the `sessionUpdate`
//     discriminator (allOf), e.g. tool_call carries toolCallId/title/status
//     directly — they are NOT nested under a `toolCall` key.
//   * ToolCallStatus  = pending | in_progress | completed | failed
//   * PlanEntryStatus = pending | in_progress | completed
//   These are mapped to Tandem's normalized vocabulary below.

import { spawn, type ChildProcess } from 'node:child_process';
import { AsyncQueue } from './asyncQueue.ts';
import type { AgentAdapter, AgentEvent, SpawnOpts } from './types.ts';

let permCounter = 0;

export class AcpAdapter implements AgentAdapter {
  // What this adapter currently *services*. fs/terminal are advertised as false
  // to the agent until the daemon has a real workspace fs + TerminalHost (TODO).
  readonly capabilities = { structured: true, terminals: false, loadSession: true, fs: false };

  private q = new AsyncQueue<AgentEvent>();
  private proc?: ChildProcess;
  private inbuf = '';
  private nextId = 1;
  private pending = new Map<number, { resolve: (r: unknown) => void; reject: (e: unknown) => void }>();
  private sessionId = '';
  private permIds = new Map<string, number>(); // our reqId -> agent's JSON-RPC request id

  constructor(readonly id: string, private launch: { cmd: string; args: string[] }) {}

  get events() {
    return this.q;
  }
  get pid() {
    return this.proc?.pid;
  }

  async spawn(opts: SpawnOpts): Promise<void> {
    this.proc = spawn(this.launch.cmd, this.launch.args, { stdio: ['pipe', 'pipe', 'inherit'] });
    this.proc.stdout!.on('data', (d: Buffer) => this.onData(d));
    this.proc.on('exit', (code) => {
      for (const [, p] of this.pending) p.reject(new Error(`agent exited (code ${code}) before responding`));
      this.pending.clear();
      this.q.push({ kind: 'status', status: code ? 'error' : 'idle' });
      this.q.close();
    });

    // TODO: once the daemon owns a workspace fs + TerminalHost, advertise these
    // and service fs/* and terminal/* requests. For now advertise none so the
    // agent does its own I/O and we just consume the update stream.
    await this.rpc('initialize', {
      protocolVersion: 1,
      clientCapabilities: { fs: { readTextFile: false, writeTextFile: false }, terminal: false },
    });
    const res = (await this.rpc('session/new', { cwd: opts.cwd ?? process.cwd(), mcpServers: [] })) as { sessionId: string };
    this.sessionId = res.sessionId;
    this.q.push({ kind: 'status', status: 'idle' });
  }

  private send(obj: unknown): void {
    this.proc!.stdin!.write(JSON.stringify(obj) + '\n');
  }

  private rpc(method: string, params: unknown): Promise<unknown> {
    const id = this.nextId++;
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject });
      this.send({ jsonrpc: '2.0', id, method, params });
    });
  }

  private onData(d: Buffer): void {
    this.inbuf += d.toString('utf8');
    let i: number;
    while ((i = this.inbuf.indexOf('\n')) >= 0) {
      const line = this.inbuf.slice(0, i);
      this.inbuf = this.inbuf.slice(i + 1);
      if (!line.trim()) continue;
      let msg: any;
      try {
        msg = JSON.parse(line);
      } catch {
        continue;
      }
      this.handle(msg);
    }
  }

  private handle(msg: any): void {
    // response to one of our requests
    if (msg.id !== undefined && (msg.result !== undefined || msg.error !== undefined) && this.pending.has(msg.id)) {
      const p = this.pending.get(msg.id)!;
      this.pending.delete(msg.id);
      if (msg.error !== undefined) p.reject(new Error(msg.error?.message ?? 'rpc error'));
      else p.resolve(msg.result);
      return;
    }
    // request initiated by the agent (permission / fs / terminal)
    if (msg.method && msg.id !== undefined) {
      this.onRequest(msg);
      return;
    }
    // notification
    if (msg.method === 'session/update') this.onUpdate(msg.params?.update);
  }

  private onRequest(msg: any): void {
    if (msg.method === 'session/request_permission') {
      const reqId = 'perm_' + ++permCounter;
      this.permIds.set(reqId, msg.id);
      const tc = msg.params?.toolCall ?? {};
      this.q.push({
        kind: 'permission_request',
        reqId,
        toolCallId: tc.toolCallId ?? '',
        title: tc.title ?? '(command)',
        options: (msg.params?.options ?? []).map((o: any) => ({ optionId: o.optionId, name: o.name })),
      });
      return;
    }
    // We advertised no fs/terminal capabilities, so we shouldn't receive those.
    // Be honest rather than acking with a bogus empty success.
    this.send({ jsonrpc: '2.0', id: msg.id, error: { code: -32601, message: `client does not support method: ${msg.method}` } });
  }

  // ---- ACP -> normalized status mapping ----
  private static toolStatus(s?: string): 'pending' | 'running' | 'done' | 'error' {
    switch (s) {
      case 'in_progress':
        return 'running';
      case 'completed':
        return 'done';
      case 'failed':
        return 'error';
      default:
        return 'pending';
    }
  }
  private static planStatus(s?: string): 'pending' | 'in_progress' | 'done' {
    return s === 'completed' ? 'done' : s === 'in_progress' ? 'in_progress' : 'pending';
  }
  // ContentBlock (or ContentBlock[]) -> plain text; non-text blocks are ignored here.
  private static text(content: any): string {
    if (Array.isArray(content)) return content.filter((c) => c?.type === 'text').map((c) => c.text).join('');
    if (content?.type === 'text') return content.text ?? '';
    return content?.text ?? '';
  }

  private onUpdate(u: any): void {
    if (!u) return;
    switch (u.sessionUpdate) {
      case 'agent_message_chunk':
        this.q.push({ kind: 'message_chunk', text: AcpAdapter.text(u.content) });
        break;
      case 'agent_thought_chunk':
        this.q.push({ kind: 'thought_chunk', text: AcpAdapter.text(u.content) });
        break;
      case 'tool_call':
        this.q.push({ kind: 'tool_call', id: u.toolCallId, title: u.title ?? '', status: AcpAdapter.toolStatus(u.status), content: u.content });
        break;
      case 'tool_call_update':
        this.q.push({ kind: 'tool_call_update', id: u.toolCallId, status: u.status ? AcpAdapter.toolStatus(u.status) : undefined });
        break;
      case 'plan':
      case 'plan_update': // 1.2.x incremental plan; both carry `entries`
        this.q.push({
          kind: 'plan',
          entries: (u.entries ?? []).map((e: any) => ({ label: e.content ?? '', status: AcpAdapter.planStatus(e.status) })),
        });
        break;
      // user_message_chunk / usage_update / current_mode_update / available_commands_update
      // / plan_removed / config_option_update / session_info_update are ignored for now.
    }
  }

  async prompt(text: string): Promise<void> {
    this.q.push({ kind: 'status', status: 'working' });
    await this.rpc('session/prompt', { sessionId: this.sessionId, prompt: [{ type: 'text', text }] });
    this.q.push({ kind: 'status', status: 'idle' });
  }

  sendInput(): void {
    /* structured agent — no raw stdin channel */
  }

  respondPermission(reqId: string, optionId: string): void {
    const id = this.permIds.get(reqId);
    if (id === undefined) return;
    this.permIds.delete(reqId);
    this.send({ jsonrpc: '2.0', id, result: { outcome: { outcome: 'selected', optionId } } });
    this.q.push({ kind: 'status', status: 'working' });
  }

  interrupt(): void {
    // session/cancel is a notification (no id) in ACP.
    if (this.proc && this.sessionId) this.send({ jsonrpc: '2.0', method: 'session/cancel', params: { sessionId: this.sessionId } });
  }

  async dispose(): Promise<void> {
    this.proc?.kill();
  }
}
