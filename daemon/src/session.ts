// One running agent, owned by the daemon. Consumes the adapter's normalized
// event stream into an EventLog and fans out to any attached WS clients.
// Crucially: this keeps running whether or not any client is connected.
//
// Phase 3: the daemon-owned ClientServices (WorkspaceFs + TerminalHost) are
// built here — the session knows the agent's workspace cwd — and handed to the
// adapter, which routes ACP fs/* and terminal/* requests to them. Terminal
// output is emitted through the SAME `emit` path as adapter events, so every
// terminal_output lands in the log with a monotonic seq (durable + replayable).

import { EventLog, type EventStore, type LoggedEvent } from './eventLog.ts';
import { TerminalHost } from './terminalHost.ts';
import { WorkspaceFs } from './workspaceFs.ts';
import type { AgentAdapter, AgentEvent, AgentStatus, Approval, ClientServices, ControlMode, SpawnOpts, SpawnSpec } from './types.ts';

export class AgentSession {
  readonly log: EventLog;
  status: AgentStatus = 'idle';
  controlMode: ControlMode = 'transcript';
  // Live permission requests, so a reconnecting/late client sees them in the
  // snapshot's pendingApprovals (the always-on approvals rail).
  private approvals = new Map<string, Approval>();
  private listeners = new Set<(e: LoggedEvent) => void>();
  // Daemon-owned terminal buffers (docs/agent-adapter.md). Exposed so the
  // registry can tear processes down and tests can inspect the ACP-view vs.
  // scrollback split directly.
  terminals?: TerminalHost;
  private adapterEpoch = 0;
  private activePrompt?: Promise<string>;

  constructor(
    readonly id: string,
    readonly name: string,
    readonly spec: SpawnSpec,
    private adapter: AgentAdapter,
    store: EventStore,
  ) {
    this.log = new EventLog(id, store);
  }

  async start(opts: SpawnOpts): Promise<void> {
    await this.attachAdapter(this.adapter, opts);
  }

  private async attachAdapter(adapter: AgentAdapter, opts: SpawnOpts, onExit?: () => void): Promise<void> {
    const cwd = opts.cwd ?? process.cwd();
    const terminals = new TerminalHost({ defaultCwd: cwd, emit: (ev) => this.emit(ev) });
    this.terminals = terminals;
    const services: ClientServices = {
      fs: new WorkspaceFs(cwd),
      terminals,
      permissions: { request: () => {} }, // perms already flow via the adapter's event queue
    };
    await adapter.spawn(opts, services);
    const epoch = ++this.adapterEpoch;
    void this.pump(adapter, epoch);
    if (adapter.exited && onExit) void adapter.exited.then(() => epoch === this.adapterEpoch && onExit());
  }

  // The single sink for everything that enters this agent's log: adapter events
  // AND daemon-side terminal_output. Keeps seq assignment and fan-out in one
  // place so the two sources can't race or diverge.
  private emit(ev: AgentEvent): void {
    if (ev.kind === 'status') this.status = ev.status;
    if (ev.kind === 'permission_request') {
      this.status = 'blocked';
      this.approvals.set(ev.reqId, { reqId: ev.reqId, toolCallId: ev.toolCallId, title: ev.title, options: ev.options });
    }
    const le = this.log.append(ev);
    for (const l of this.listeners) l(le);
  }

  private async pump(adapter: AgentAdapter, epoch: number): Promise<void> {
    for await (const ev of adapter.events) if (epoch === this.adapterEpoch) this.emit(ev);
  }

  async swapAdapter(adapter: AgentAdapter, opts: SpawnOpts, mode: ControlMode, onExit?: () => void): Promise<void> {
    ++this.adapterEpoch; // silence the old pump before disposal emits its tail
    this.terminals?.disposeAll();
    await this.adapter.dispose();
    this.adapter = adapter;
    await this.attachAdapter(adapter, opts, onExit);
    this.setControlMode(mode);
  }

  setControlMode(mode: ControlMode): void {
    this.controlMode = mode;
    this.emit({ kind: 'control_state', mode });
  }

  // Push a daemon-originated normalized event into this agent's log + fan-out
  // (Phase 5: browser takeover_request and browser-driven status transitions).
  // Goes through the same seq/emit path as adapter events.
  pushEvent(ev: AgentEvent): void {
    this.emit(ev);
  }

  onEvent(cb: (e: LoggedEvent) => void): () => void {
    this.listeners.add(cb);
    return () => this.listeners.delete(cb);
  }

  pendingApprovals(): Approval[] {
    return [...this.approvals.values()];
  }

  prompt(text: string): Promise<string> {
    if (this.controlMode !== 'transcript') return Promise.reject(new Error('agent session is controlled by the terminal'));
    // Log the human turn first so it lands in the transcript ahead of the agent's
    // response (and replays for late/reconnecting clients).
    this.emit({ kind: 'user_message', text });
    const turn = this.adapter.prompt(text);
    this.activePrompt = turn;
    void turn.finally(() => {
      if (this.activePrompt === turn) this.activePrompt = undefined;
    }).catch(() => {});
    return turn;
  }
  respondPermission(reqId: string, optionId: string): void {
    this.approvals.delete(reqId);
    this.adapter.respondPermission(reqId, optionId);
  }
  sendInput(bytes: Uint8Array): void {
    this.adapter.sendInput(bytes);
  }
  resize(cols: number, rows: number): void {
    this.adapter.resize?.(cols, rows);
  }
  interrupt(): void {
    // Cancellation resolves any outstanding permission requests (ACP semantics,
    // acp-notes.md) — the adapter answers them cancelled; we drop them here.
    this.approvals.clear();
    this.adapter.interrupt();
  }
  async interruptAndWait(timeoutMs = 4000): Promise<void> {
    this.interrupt();
    const turn = this.activePrompt;
    if (!turn) return;
    await Promise.race([turn.catch(() => undefined), new Promise<void>((resolve) => setTimeout(resolve, timeoutMs))]);
  }
  setMode(modeId: string): Promise<void> {
    if (!this.adapter.setMode) return Promise.reject(new Error('this agent does not support session modes'));
    return this.adapter.setMode(modeId);
  }
  setConfigOption(configId: string, value: string | boolean): Promise<void> {
    if (!this.adapter.setConfigOption) return Promise.reject(new Error('this agent does not support session config options'));
    return this.adapter.setConfigOption(configId, value);
  }
  async dispose(): Promise<void> {
    ++this.adapterEpoch; // suppress exit callbacks/events from intentional teardown
    this.terminals?.disposeAll();
    await this.adapter.dispose();
  }

  get acpSessionId(): string | undefined {
    return this.adapter.acpSessionId;
  }
  get agentPid(): number | undefined {
    return this.adapter.pid;
  }
}
