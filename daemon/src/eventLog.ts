// The source of truth for one agent's stream: a monotonic seq per event plus a
// bounded ring buffer for replay. This is what makes reconnect gapless.

import type { AgentEvent } from './types.ts';

export interface LoggedEvent {
  seq: number;
  event: AgentEvent;
  ts: number;
}

export class EventLog {
  private buf: LoggedEvent[] = [];
  private _head = 0;

  constructor(private cap = 5000) {}

  append(event: AgentEvent): LoggedEvent {
    const le: LoggedEvent = { seq: ++this._head, event, ts: Date.now() };
    this.buf.push(le);
    if (this.buf.length > this.cap) this.buf.shift();
    return le;
  }

  get head(): number {
    return this._head;
  }

  /** seq of the oldest event we still retain (for gap detection). */
  get earliest(): number {
    return this.buf.length ? this.buf[0].seq : this._head + 1;
  }

  /** Events strictly newer than `seq` (the replay path). */
  since(seq: number): LoggedEvent[] {
    return this.buf.filter((e) => e.seq > seq);
  }

  snapshot(): LoggedEvent[] {
    return this.buf.slice();
  }

  /** True when the client's checkpoint is older than what we can still replay. */
  hasGap(sinceSeq: number): boolean {
    return sinceSeq > 0 && sinceSeq < this.earliest - 1;
  }
}
