// One running agent, owned by the daemon. Consumes the adapter's normalized
// event stream into an EventLog and fans out to any attached WS clients.
// Crucially: this keeps running whether or not any client is connected.

import { EventLog, type LoggedEvent } from './eventLog.ts';
import type { AgentAdapter, AgentStatus, SpawnOpts } from './types.ts';

export class AgentSession {
  readonly log = new EventLog();
  status: AgentStatus = 'idle';
  private listeners = new Set<(e: LoggedEvent) => void>();

  constructor(readonly id: string, private adapter: AgentAdapter) {}

  async start(opts: SpawnOpts): Promise<void> {
    await this.adapter.spawn(opts);
    void this.pump();
  }

  private async pump(): Promise<void> {
    for await (const ev of this.adapter.events) {
      if (ev.kind === 'status') this.status = ev.status;
      if (ev.kind === 'permission_request') this.status = 'blocked';
      const le = this.log.append(ev);
      for (const l of this.listeners) l(le);
    }
  }

  onEvent(cb: (e: LoggedEvent) => void): () => void {
    this.listeners.add(cb);
    return () => this.listeners.delete(cb);
  }

  prompt(text: string): Promise<void> {
    return this.adapter.prompt(text);
  }
  respondPermission(reqId: string, optionId: string): void {
    this.adapter.respondPermission(reqId, optionId);
  }
  sendInput(bytes: Uint8Array): void {
    this.adapter.sendInput(bytes);
  }
  dispose(): Promise<void> {
    return this.adapter.dispose();
  }

  get agentPid(): number | undefined {
    return this.adapter.pid;
  }
}
