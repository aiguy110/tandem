// The source of truth for one agent's stream: a monotonic seq per event plus a
// bounded in-memory ring buffer for hot replay. Appends write THROUGH to a
// persistent store (SQLite); the ring is the fast path and the store is the
// backstop for older ranges and for history after a daemon restart.
//
// Gapless reconnect (docs/ws-protocol.md): if the ring still holds events past
// the client's checkpoint we replay from the ring; otherwise we fall back to the
// store, so replay stays gapless for any range the DB still has.

import type { AgentEvent } from './types.ts';

export interface LoggedEvent {
  seq: number;
  event: AgentEvent;
  ts: number;
}

// The persistent backing (implemented by db.ts). Kept as an interface so tests
// (and a future non-SQLite store) can substitute it.
export interface EventStore {
  appendEvent(agentId: string, le: LoggedEvent): void;
  rangeEvents(agentId: string, afterSeq: number): LoggedEvent[];
  maxSeq(agentId: string): number;
  minSeq(agentId: string): number;
}

// A non-persistent EventStore for standalone scripts (acpLive, ptySmoke) that
// don't want a SQLite file. The daemon proper always uses the SQLite-backed Db.
export class MemoryStore implements EventStore {
  private byAgent = new Map<string, LoggedEvent[]>();
  appendEvent(agentId: string, le: LoggedEvent): void {
    (this.byAgent.get(agentId) ?? this.byAgent.set(agentId, []).get(agentId)!).push(le);
  }
  rangeEvents(agentId: string, afterSeq: number): LoggedEvent[] {
    return (this.byAgent.get(agentId) ?? []).filter((e) => e.seq > afterSeq);
  }
  maxSeq(agentId: string): number {
    const a = this.byAgent.get(agentId);
    return a && a.length ? a[a.length - 1].seq : 0;
  }
  minSeq(agentId: string): number {
    const a = this.byAgent.get(agentId);
    return a && a.length ? a[0].seq : 0;
  }
}

export class EventLog {
  private buf: LoggedEvent[] = [];
  private _head: number;

  constructor(
    private agentId: string,
    private store: EventStore,
    private cap = 5000,
  ) {
    // Continue seq numbering from whatever the store already has (restore).
    this._head = store.maxSeq(agentId);
  }

  append(event: AgentEvent): LoggedEvent {
    const le: LoggedEvent = { seq: ++this._head, event, ts: Date.now() };
    this.store.appendEvent(this.agentId, le); // write-through first: durability before fan-out
    this.buf.push(le);
    if (this.buf.length > this.cap) this.buf.shift();
    return le;
  }

  get head(): number {
    return this._head;
  }

  /** seq of the oldest event still in the in-memory ring. */
  get earliestInRing(): number {
    return this.buf.length ? this.buf[0].seq : this._head + 1;
  }

  /** Events strictly newer than `seq`: ring if we have them, else the store. */
  since(seq: number): LoggedEvent[] {
    if (seq >= this.earliestInRing - 1) return this.buf.filter((e) => e.seq > seq);
    // Older than the ring — pull from persistence (still gapless).
    return this.store.rangeEvents(this.agentId, seq);
  }

  /** Full transcript from the store (used to build a fresh snapshot, incl. after restart). */
  fullHistory(): LoggedEvent[] {
    return this.store.rangeEvents(this.agentId, 0);
  }

  /** True only when even the store can't cover the checkpoint (events pruned). */
  hasGap(sinceSeq: number): boolean {
    if (sinceSeq <= 0) return false;
    const min = this.store.minSeq(this.agentId);
    return min > 0 && sinceSeq < min - 1;
  }
}
