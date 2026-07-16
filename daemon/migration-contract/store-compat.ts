import assert from 'node:assert/strict';
import { Db } from '../src/db.ts';

const [mode, dbPath] = process.argv.slice(2);
assert.ok(dbPath, 'usage: store-compat.ts <create|check> <db-path>');

if (mode === 'create') {
  const db = new Db(dbPath);
  db.upsertAgent({
    id: 'node-1', name: 'node-1', spec: { adapter: 'acp', agent: 'codex' },
    cwd: '/fixtures/node', acpSessionId: 'node-session', status: 'idle',
    createdAt: 1700000010000, closedAt: null,
  });
  db.putAsset('node-1', { id: 'a'.repeat(64), mimeType: 'image/png', size: 68 });
  db.close();
} else if (mode === 'check') {
  const db = new Db(dbPath);
  const node = db.getAgent('node-1');
  assert.equal(node?.status, 'blocked');
  assert.equal(node?.acpSessionId, 'go-session');
  assert.deepEqual(db.getAgentAsset('node-1', 'b'.repeat(64)), { id: 'b'.repeat(64), mimeType: 'image/jpeg', size: 99 });
  const go = db.getAgent('go-2');
  assert.deepEqual(go, {
    id: 'go-2', name: 'go-2', spec: { adapter: 'acp', agent: 'codex' }, cwd: '/fixtures/go',
    acpSessionId: null, status: 'idle', createdAt: 1700000020000, closedAt: null,
  });
  db.close();
} else {
  throw new Error(`unknown mode: ${mode}`);
}
