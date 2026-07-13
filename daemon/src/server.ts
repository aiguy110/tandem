// Browser <-> daemon WebSocket. Implements the subscribe / snapshot / event /
// replay contract from docs/ws-protocol.md for a single agent.

import { WebSocketServer } from 'ws';
import type { AgentSession } from './session.ts';
import type { AgentEvent } from './types.ts';

// raw_pty carries bytes; JSON can't, so base64 it on the wire.
function serialize(ev: AgentEvent): unknown {
  if (ev.kind === 'raw_pty') return { kind: 'raw_pty', dataB64: Buffer.from(ev.data).toString('base64') };
  return ev;
}

export function startServer(session: AgentSession, port: number): WebSocketServer {
  const wss = new WebSocketServer({ port });

  wss.on('connection', (ws) => {
    let unsub: (() => void) | null = null;

    ws.on('message', (raw) => {
      let m: any;
      try {
        m = JSON.parse(raw.toString());
      } catch {
        return;
      }

      switch (m.t) {
        case 'subscribe': {
          const since = m.sinceSeq ?? 0;
          if (since > 0 && !session.log.hasGap(since)) {
            // gapless replay from the client's checkpoint
            for (const le of session.log.since(since)) {
              ws.send(JSON.stringify({ t: 'event', seq: le.seq, event: serialize(le.event) }));
            }
          } else {
            // fresh (or gap too large) -> full snapshot
            ws.send(
              JSON.stringify({
                t: 'snapshot',
                seq: session.log.head,
                status: session.status,
                events: session.log.snapshot().map((le) => ({ seq: le.seq, event: serialize(le.event) })),
              }),
            );
          }
          unsub?.();
          unsub = session.onEvent((le) => ws.send(JSON.stringify({ t: 'event', seq: le.seq, event: serialize(le.event) })));
          break;
        }
        case 'prompt':
          void session.prompt(m.text);
          break;
        case 'permission_response':
          session.respondPermission(m.reqId, m.optionId);
          break;
        case 'input':
          session.sendInput(Buffer.from(m.bytesB64 ?? '', 'base64'));
          break;
      }
    });

    ws.on('close', () => unsub?.());
  });

  return wss;
}
