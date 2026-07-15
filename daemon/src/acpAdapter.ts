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
import { PathEscapeError } from './workspaceFs.ts';
import type { AgentAdapter, AgentEvent, ClientServices, SessionConfigOption, SessionModeState, SlashCommand, SpawnOpts } from './types.ts';

let permCounter = 0;

// Normalizes the ACP wire shape (SessionConfigOption, whose `options` may be a
// flat list OR grouped under headers — SessionConfigSelectOptions) into our flat
// SessionConfigOption. Groups are collapsed since Tandem's UI is a plain dropdown.
function normalizeConfigOption(o: any): SessionConfigOption {
  const type: 'select' | 'boolean' = o?.type === 'boolean' ? 'boolean' : 'select';
  if (type === 'boolean') {
    return { id: o.id, name: o.name, description: o.description ?? undefined, category: o.category ?? undefined, type, currentValue: !!o.currentValue };
  }
  const options: { value: string; name: string; description?: string }[] = [];
  for (const item of o?.options ?? []) {
    if (item && Array.isArray(item.options)) {
      for (const opt of item.options) options.push({ value: opt.value, name: opt.name, description: opt.description ?? undefined });
    } else if (item) {
      options.push({ value: item.value, name: item.name, description: item.description ?? undefined });
    }
  }
  return { id: o.id, name: o.name, description: o.description ?? undefined, category: o.category ?? undefined, type, currentValue: o.currentValue, options };
}

// Normalizes ACP's AvailableCommand (name/description/input.hint) into our flat
// SlashCommand (input is just the hint string, or undefined if no input).
function normalizeCommand(c: any): SlashCommand {
  return { name: c.name, description: c.description ?? undefined, input: c.input?.hint ?? undefined };
}

function normalizeModes(m: any): SessionModeState | null {
  if (!m) return null;
  return {
    currentModeId: m.currentModeId,
    availableModes: (m.availableModes ?? []).map((mo: any) => ({ id: mo.id, name: mo.name, description: mo.description ?? undefined })),
  };
}

export class AcpAdapter implements AgentAdapter {
  // What this adapter *services*. Phase 3: fs + terminals are now backed by the
  // daemon-owned WorkspaceFs + TerminalHost passed in via ClientServices, so we
  // advertise them to the agent and handle its fs/* and terminal/* requests.
  readonly capabilities = { structured: true, terminals: true, loadSession: true, fs: true };

  private q = new AsyncQueue<AgentEvent>();
  private proc?: ChildProcess;
  private inbuf = '';
  private nextId = 1;
  private pending = new Map<number, { resolve: (r: unknown) => void; reject: (e: unknown) => void }>();
  private sessionId = '';
  private agentLoadSession = false; // from the agent's initialize response
  private permIds = new Map<string, number>(); // our reqId -> agent's JSON-RPC request id
  private services?: ClientServices; // daemon-owned fs + terminals
  // Unfinished tool calls, tracked so session/cancel can mark them cancelled in
  // the transcript (acp-notes.md cancellation contract).
  private liveToolCalls = new Set<string>();
  // Current modes/configOptions (permission-mode + model selectors) — set from
  // session/new|load's response, refreshed by current_mode_update /
  // config_option_update notifications. Null modes = agent doesn't support them.
  private modes: SessionModeState | null = null;
  private configOptions: SessionConfigOption[] = [];
  private commands: SlashCommand[] = [];

  constructor(readonly id: string, private launch: { cmd: string; args: string[] }) {}

  get events() {
    return this.q;
  }
  get pid() {
    return this.proc?.pid;
  }
  // Persisted for restore (D11). Empty until session/new or session/load resolves.
  get acpSessionId() {
    return this.sessionId || undefined;
  }

  async spawn(opts: SpawnOpts, services?: ClientServices): Promise<void> {
    this.services = services;
    this.proc = spawn(this.launch.cmd, this.launch.args, { stdio: ['pipe', 'pipe', 'inherit'], cwd: opts.cwd });
    this.proc.stdout!.on('data', (d: Buffer) => this.onData(d));
    this.proc.on('exit', (code) => {
      for (const [, p] of this.pending) p.reject(new Error(`agent exited (code ${code}) before responding`));
      this.pending.clear();
      this.q.push({ kind: 'status', status: code ? 'error' : 'idle' });
      this.q.close();
    });

    // Phase 3: the daemon owns a WorkspaceFs + TerminalHost (via ClientServices),
    // so advertise fs + terminal and service the agent's fs/* and terminal/*
    // requests. `terminal` is a plain boolean per the schema (ClientCapabilities).
    const init = (await this.rpc('initialize', {
      protocolVersion: 1,
      clientCapabilities: { fs: { readTextFile: true, writeTextFile: true }, terminal: true },
    })) as { agentCapabilities?: { loadSession?: boolean } };
    this.agentLoadSession = !!init.agentCapabilities?.loadSession;

    if (opts.resumeSessionId && this.agentLoadSession) {
      // Restore path (D11): resume the persisted ACP session instead of a new one.
      const res = (await this.rpc('session/load', { sessionId: opts.resumeSessionId, cwd: opts.cwd ?? process.cwd(), mcpServers: [] })) as {
        modes?: unknown;
        configOptions?: unknown[];
      };
      this.sessionId = opts.resumeSessionId;
      this.modes = normalizeModes(res.modes);
      this.configOptions = (res.configOptions ?? []).map(normalizeConfigOption);
    } else {
      // Phase 5: register any MCP servers (Playwright MCP + Tandem-control MCP)
      // the daemon wants this agent to have. McpServerStdio shape: {name, command,
      // args, env}. The AGENT spawns them; we only declare them here.
      const mcpServers = (opts.mcpServers ?? []).map((s) => ({ name: s.name, command: s.command, args: s.args, env: s.env }));
      const res = (await this.rpc('session/new', { cwd: opts.cwd ?? process.cwd(), mcpServers })) as {
        sessionId: string;
        modes?: unknown;
        configOptions?: unknown[];
      };
      this.sessionId = res.sessionId;
      this.modes = normalizeModes(res.modes);
      this.configOptions = (res.configOptions ?? []).map(normalizeConfigOption);
    }
    if (this.modes || this.configOptions.length) this.q.push({ kind: 'session_config', modes: this.modes, configOptions: this.configOptions });
    this.q.push({ kind: 'status', status: 'idle' });
  }

  // Explicit resume entry-point (docs/agent-adapter.md), for callers that spawn
  // then load. spawn(opts.resumeSessionId) is the primary path used by restore.
  async loadSession(sessionId: string): Promise<void> {
    if (!this.agentLoadSession) throw new Error('agent does not advertise loadSession');
    await this.rpc('session/load', { sessionId, cwd: process.cwd(), mcpServers: [] });
    this.sessionId = sessionId;
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
    // fs/* and terminal/* are serviced by the daemon-owned ClientServices. These
    // are async; we respond when they settle (JSON-RPC allows out-of-order ids),
    // so e.g. terminal/wait_for_exit never blocks the read loop.
    void this.serviceRequest(msg);
  }

  private ok(id: number, result: unknown): void {
    this.send({ jsonrpc: '2.0', id, result });
  }
  private err(id: number, code: number, message: string): void {
    this.send({ jsonrpc: '2.0', id, error: { code, message } });
  }

  private async serviceRequest(msg: any): Promise<void> {
    const svc = this.services;
    const p = msg.params ?? {};
    try {
      if (!svc) throw Object.assign(new Error('client services unavailable'), { code: -32603 });
      switch (msg.method) {
        case 'fs/read_text_file': {
          const content = await svc.fs.readTextFile(p.path, { line: p.line ?? undefined, limit: p.limit ?? undefined });
          return this.ok(msg.id, { content });
        }
        case 'fs/write_text_file': {
          await svc.fs.writeTextFile(p.path, p.content ?? '');
          return this.ok(msg.id, {});
        }
        case 'terminal/create': {
          const terminalId = await svc.terminals.create({
            command: p.command,
            args: p.args ?? [],
            cwd: p.cwd ?? undefined,
            env: p.env ?? [],
            outputByteLimit: p.outputByteLimit ?? undefined,
          });
          return this.ok(msg.id, { terminalId });
        }
        case 'terminal/output': {
          const { output, truncated, exitStatus } = svc.terminals.output(p.terminalId);
          return this.ok(msg.id, { output, truncated, exitStatus });
        }
        case 'terminal/wait_for_exit': {
          const e = await svc.terminals.waitForExit(p.terminalId);
          return this.ok(msg.id, { exitCode: e.exitCode, signal: e.signal });
        }
        case 'terminal/kill': {
          svc.terminals.kill(p.terminalId);
          return this.ok(msg.id, {});
        }
        case 'terminal/release': {
          svc.terminals.release(p.terminalId);
          return this.ok(msg.id, {});
        }
        default:
          return this.err(msg.id, -32601, `client does not support method: ${msg.method}`);
      }
    } catch (e: any) {
      // Path escapes are the sandbox choke point → a proper invalid-params error.
      const code = e instanceof PathEscapeError ? -32602 : e?.code ?? -32603;
      this.err(msg.id, code, e?.message ?? 'client method failed');
    }
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
      case 'tool_call': {
        const status = AcpAdapter.toolStatus(u.status);
        if (status === 'pending' || status === 'running') this.liveToolCalls.add(u.toolCallId);
        else this.liveToolCalls.delete(u.toolCallId);
        this.q.push({ kind: 'tool_call', id: u.toolCallId, title: u.title ?? '', status, content: u.content, rawInput: u.rawInput });
        break;
      }
      case 'tool_call_update': {
        const status = u.status ? AcpAdapter.toolStatus(u.status) : undefined;
        if (status && status !== 'pending' && status !== 'running') this.liveToolCalls.delete(u.toolCallId);
        this.q.push({ kind: 'tool_call_update', id: u.toolCallId, status, content: u.content });
        break;
      }
      case 'plan':
      case 'plan_update': // 1.2.x incremental plan; both carry `entries`
        this.q.push({
          kind: 'plan',
          entries: (u.entries ?? []).map((e: any) => ({ label: e.content ?? '', status: AcpAdapter.planStatus(e.status) })),
        });
        break;
      case 'current_mode_update':
        this.modes = this.modes ? { ...this.modes, currentModeId: u.currentModeId } : { currentModeId: u.currentModeId, availableModes: [] };
        this.q.push({ kind: 'session_config', modes: this.modes, configOptions: this.configOptions });
        break;
      case 'config_option_update':
        this.configOptions = (u.configOptions ?? []).map(normalizeConfigOption);
        this.q.push({ kind: 'session_config', modes: this.modes, configOptions: this.configOptions });
        break;
      case 'available_commands_update':
        this.commands = (u.availableCommands ?? []).map(normalizeCommand);
        this.q.push({ kind: 'available_commands', commands: this.commands });
        break;
      // user_message_chunk / usage_update / plan_removed / session_info_update
      // are ignored for now.
    }
  }

  async prompt(text: string): Promise<string> {
    this.q.push({ kind: 'status', status: 'working' });
    const res = (await this.rpc('session/prompt', { sessionId: this.sessionId, prompt: [{ type: 'text', text }] })) as { stopReason?: string };
    this.q.push({ kind: 'status', status: 'idle' });
    return res?.stopReason ?? 'end_turn';
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

  async setMode(modeId: string): Promise<void> {
    await this.rpc('session/set_mode', { sessionId: this.sessionId, modeId });
    // The real agent confirms via a current_mode_update notification, but that
    // races the RPC response on the wire — apply optimistically too so a client
    // that only awaits the ack sees the change immediately.
    this.modes = this.modes ? { ...this.modes, currentModeId: modeId } : { currentModeId: modeId, availableModes: [] };
    this.q.push({ kind: 'session_config', modes: this.modes, configOptions: this.configOptions });
  }

  async setConfigOption(configId: string, value: string | boolean): Promise<void> {
    const params: Record<string, unknown> =
      typeof value === 'boolean' ? { sessionId: this.sessionId, configId, type: 'boolean', value } : { sessionId: this.sessionId, configId, value };
    const res = (await this.rpc('session/set_config_option', params)) as { configOptions?: unknown[] };
    // Pinned fact (acp-notes.md candidate): the real agent's response already
    // carries the full updated configOptions list, so apply it directly rather
    // than waiting on a config_option_update notification that may not follow.
    if (res.configOptions) this.configOptions = res.configOptions.map(normalizeConfigOption);
    this.q.push({ kind: 'session_config', modes: this.modes, configOptions: this.configOptions });
  }

  interrupt(): void {
    // acp-notes.md cancellation contract:
    //  1. resolve any pending permission requests as `cancelled`,
    //  2. mark unfinished tool calls cancelled in the transcript,
    //  3. send the session/cancel notification (no id).
    // The in-flight session/prompt resolves when the agent answers it with
    // stopReason 'cancelled' — the pending map clears normally, so the adapter
    // is never wedged and subsequent prompts work.
    for (const [reqId, id] of this.permIds) {
      this.send({ jsonrpc: '2.0', id, result: { outcome: { outcome: 'cancelled' } } });
      this.permIds.delete(reqId);
    }
    for (const id of this.liveToolCalls) this.q.push({ kind: 'tool_call_update', id, status: 'cancelled' });
    this.liveToolCalls.clear();
    if (this.proc && this.sessionId) this.send({ jsonrpc: '2.0', method: 'session/cancel', params: { sessionId: this.sessionId } });
  }

  async dispose(): Promise<void> {
    this.proc?.kill();
  }
}
