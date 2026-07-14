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
import type { AgentAdapter, AgentEvent, AgentStatus, Approval, ClientServices, SpawnOpts, SpawnSpec } from './types.ts';

export class AgentSession {
  readonly log: EventLog;
  status: AgentStatus = 'idle';
  // Live permission requests, so a reconnecting/late client sees them in the
  // snapshot's pendingApprovals (the always-on approvals rail).
  private approvals = new Map<string, Approval>();
  private listeners = new Set<(e: LoggedEvent) => void>();
  // Daemon-owned terminal buffers (docs/agent-adapter.md). Exposed so the
  // registry can tear processes down and tests can inspect the ACP-view vs.
  // scrollback split directly.
  terminals?: TerminalHost;

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
    const cwd = opts.cwd ?? process.cwd();
    const terminals = new TerminalHost({ defaultCwd: cwd, emit: (ev) => this.emit(ev) });
    this.terminals = terminals;
    const services: ClientServices = {
      fs: new WorkspaceFs(cwd),
      terminals,
      permissions: { request: () => {} }, // perms already flow via the adapter's event queue
    };
    await this.adapter.spawn(opts, services);
    void this.pump();
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

  private async pump(): Promise<void> {
    for await (const ev of this.adapter.events) this.emit(ev);
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
    return this.adapter.prompt(text);
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
  async dispose(): Promise<void> {
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
