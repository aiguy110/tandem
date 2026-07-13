// ACP adapter: the daemon is the ACP *client*. Speaks JSON-RPC 2.0 over the
// agent subprocess's stdio (newline-delimited framing for this PoC — the exact
// framing must be confirmed against the ACP spec when wiring a real agent).
//
// Translates:
//   session/update            -> normalized AgentEvent
//   session/request_permission -> permission_request event + response round-trip

import { spawn, type ChildProcess } from 'node:child_process';
import { AsyncQueue } from './asyncQueue.ts';
import type { AgentAdapter, AgentEvent, SpawnOpts } from './types.ts';

let permCounter = 0;

export class AcpAdapter implements AgentAdapter {
  readonly capabilities = { structured: true, terminals: true, loadSession: true, fs: true };
  private q = new AsyncQueue<AgentEvent>();
  private proc?: ChildProcess;
  private inbuf = '';
  private nextId = 1;
  private pending = new Map<number, (r: unknown) => void>();
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
      this.q.push({ kind: 'status', status: code ? 'error' : 'idle' });
      this.q.close();
    });

    await this.rpc('initialize', {
      protocolVersion: 1,
      clientCapabilities: { fs: { readTextFile: true, writeTextFile: true }, terminal: true },
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
    return new Promise((res) => {
      this.pending.set(id, res);
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
      const r = this.pending.get(msg.id)!;
      this.pending.delete(msg.id);
      r(msg.result);
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
    // fs/* and terminal/* are client-owned; a real daemon services them here.
    // PoC: acknowledge so the agent can proceed.
    this.send({ jsonrpc: '2.0', id: msg.id, result: {} });
  }

  private onUpdate(u: any): void {
    if (!u) return;
    switch (u.sessionUpdate) {
      case 'agent_message_chunk':
        this.q.push({ kind: 'message_chunk', text: u.content?.text ?? '' });
        break;
      case 'agent_thought_chunk':
        this.q.push({ kind: 'thought_chunk', text: u.content?.text ?? '' });
        break;
      case 'tool_call':
        this.q.push({ kind: 'tool_call', id: u.toolCallId, title: u.title ?? '', status: u.status ?? 'pending', content: u.content });
        break;
      case 'tool_call_update':
        this.q.push({ kind: 'tool_call_update', id: u.toolCallId, status: u.status });
        break;
      case 'plan':
        this.q.push({ kind: 'plan', entries: (u.entries ?? []).map((e: any) => ({ label: e.content ?? e.label ?? '', status: e.status ?? 'pending' })) });
        break;
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
    void this.rpc('session/cancel', { sessionId: this.sessionId }).catch(() => {});
  }

  async dispose(): Promise<void> {
    this.proc?.kill();
  }
}
