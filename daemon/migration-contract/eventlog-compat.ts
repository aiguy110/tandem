import assert from 'node:assert/strict';
import fs from 'node:fs';
import { Db } from '../src/db.ts';
import type { AgentEvent } from '../src/types.ts';
import normalized from './fixtures/normalized-events.json' with { type: 'json' };

const [mode, dbPath] = process.argv.slice(2);
if (!mode || !dbPath) throw new Error('usage: eventlog-compat.ts create|check DB');

function fromNormalized(event: any): AgentEvent {
  if (event.kind === 'raw_pty') return { kind: 'raw_pty', data: new Uint8Array(Buffer.from(event.dataB64, 'base64')) };
  return event as AgentEvent;
}

if (mode === 'create') {
  fs.rmSync(dbPath, { force: true });
  const db = new Db(dbPath);
  normalized.forEach((event, index) => db.appendEvent('api-1', { seq: index + 1, event: fromNormalized(event), ts: 1700000010000 + index }));
  db.close();
} else if (mode === 'check') {
  const db = new Db(dbPath);
  const events = db.rangeEvents('api-1', 0);
  assert.equal(events.length, normalized.length + 2);
  assert.deepEqual(events.slice(0, normalized.length).map(({ event }) => event.kind === 'raw_pty'
    ? { kind: event.kind, dataB64: Buffer.from(event.data).toString('base64') }
    : event), normalized);
  assert.deepEqual(events[normalized.length].event, { kind: 'message_chunk', text: 'from-go' });
  const raw = events[normalized.length + 1].event;
  assert.equal(raw.kind, 'raw_pty');
  assert.equal(raw.kind === 'raw_pty' ? Buffer.from(raw.data).toString('base64') : '', 'AQID/g==');
  assert.deepEqual(events.map(({ seq }) => seq), [1, 2, 3, 4, 5, 6, 7, 8]);
  db.close();
} else {
  throw new Error(`unknown mode ${mode}`);
}
