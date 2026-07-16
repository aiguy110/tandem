import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { Db } from '../src/db.ts';
import { createSeededDatabase, fixtureDir, renderFixtures } from './generate.ts';

async function observeStartup(home: string, extraEnv: Record<string, string> = {}): Promise<{ code: number | null; stdout: string; stderr: string }> {
  const entry = new URL('../src/index.ts', import.meta.url).pathname;
  const child = spawn(process.execPath, ['--import', 'tsx', entry], {
    env: {
      ...process.env,
      TANDEM_HOME: home,
      TANDEM_PORT: '0',
      TANDEM_BIND: '127.0.0.1',
      TANDEM_BROWSER_DRIVER: 'local',
      TANDEM_BROWSER_MCP: 'off',
      STEEL_SESSION_OPTIONS: '',
      ...extraEnv,
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  let stdout = '';
  let stderr = '';
  child.stdout.setEncoding('utf8').on('data', (chunk) => {
    stdout += chunk;
    if (/^TANDEM_READY /m.test(stdout)) child.kill('SIGTERM');
  });
  child.stderr.setEncoding('utf8').on('data', (chunk) => { stderr += chunk; });
  const code = await new Promise<number | null>((resolve, reject) => {
    const timer = setTimeout(() => { child.kill('SIGKILL'); reject(new Error('daemon startup observation timed out')); }, 10_000);
    child.once('error', reject);
    child.once('exit', (exitCode) => { clearTimeout(timer); resolve(exitCode); });
  });
  return { code, stdout, stderr };
}

for (const [name, expected] of renderFixtures()) {
  assert.equal(fs.readFileSync(path.join(fixtureDir, name), 'utf8'), expected, `${name} is stale; run npm run contract:generate`);
}

const temp = fs.mkdtempSync(path.join(os.tmpdir(), 'tandem-contract-test-'));
try {
  const dbPath = path.join(temp, 'tandem.db');
  createSeededDatabase(dbPath);
  assert.equal(fs.existsSync(`${dbPath}-wal`), false, 'seed generator must not leave a WAL file');

  // Open through the production Node store, exercising schema migration and
  // event decoding rather than inspecting rows with a test-only reader.
  const reopened = new Db(dbPath);
  const agent = reopened.getAgent('api-1');
  assert.equal(agent?.spec.workspace.kind, 'worktree');
  assert.equal(agent?.spec.workspace.kind === 'worktree' ? agent.spec.workspace.source?.commit : undefined, '1111111111111111111111111111111111111111');
  const replay = reopened.rangeEvents('api-1', 0);
  assert.deepEqual(replay.map(({ seq, event, ts }) => ({ seq, event: event.kind === 'raw_pty' ? { kind: event.kind, dataB64: Buffer.from(event.data).toString('base64') } : event, ts })), [
    { seq: 1, event: { kind: 'user_message', text: 'inspect' }, ts: 1700000010000 },
    { seq: 2, event: { kind: 'message_chunk', text: 'working' }, ts: 1700000010001 },
    { seq: 3, event: { kind: 'tool_call', id: 'tool-1', title: 'Read file', status: 'running', rawInput: { path: 'README.md' } }, ts: 1700000010002 },
    { seq: 4, event: { kind: 'permission_request', reqId: 'perm-1', toolCallId: 'tool-1', title: 'Run command', options: [{ optionId: 'allow', name: 'Allow' }] }, ts: 1700000010003 },
    { seq: 5, event: { kind: 'status', status: 'blocked' }, ts: 1700000010004 },
    { seq: 6, event: { kind: 'raw_pty', dataB64: 'AAr/' }, ts: 1700000010005 },
  ]);
  reopened.close();
} finally {
  fs.rmSync(temp, { recursive: true, force: true });
}

const startupHome = fs.mkdtempSync(path.join(os.tmpdir(), 'tandem-contract-startup-'));
try {
  const normal = await observeStartup(startupHome);
  assert.equal(normal.code, 0);
  assert.match(normal.stdout, /^TANDEM_READY port=0 token=\S+$/m);
  const failed = await observeStartup(path.join(startupHome, 'failed'), { STEEL_SESSION_OPTIONS: '{' });
  assert.equal(failed.code, 1);
  assert.match(failed.stderr, /STEEL_SESSION_OPTIONS must be valid JSON/);
} finally {
  fs.rmSync(startupHome, { recursive: true, force: true });
}

console.log('migration contract: deterministic fixtures and Node seeded-DB replay pass');
