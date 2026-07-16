import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import Database from 'better-sqlite3';
import YAML from 'yaml';
import { Db } from '../src/db.ts';
import { serializeWireEvent } from '../src/server.ts';
import type { AgentEvent, AgentRecord, ClientMsg, ServerMsg } from '../src/types.ts';

export const fixtureDir = fileURLToPath(new URL('./fixtures/', import.meta.url));

const workspaces = {
  canonicalRefs: {
    local: { ref: 'refs/heads/feature/migration', kind: 'local-branch', commit: '1111111111111111111111111111111111111111' },
    remote: { ref: 'refs/remotes/origin/main', kind: 'remote-branch', commit: '2222222222222222222222222222222222222222' },
    tag: { ref: 'refs/tags/v1.0.0', kind: 'tag', commit: '3333333333333333333333333333333333333333' },
    detached: { ref: '4444444444444444444444444444444444444444', kind: 'detached', commit: '4444444444444444444444444444444444444444' },
  },
  create: {
    kind: 'worktree', repo: '/fixtures/repo', branch: 'tandem/migration/api-1', branchMode: 'create',
    source: { ref: 'refs/heads/feature/migration', commit: '1111111111111111111111111111111111111111' },
    integration: { kind: 'local-branch', ref: 'refs/heads/feature/migration' },
    baseRef: '1111111111111111111111111111111111111111',
  },
  attach: {
    kind: 'worktree', repo: '/fixtures/repo', branch: 'tandem/migration/api-1', branchMode: 'attach',
    source: { ref: 'refs/heads/tandem/migration/api-1', commit: '5555555555555555555555555555555555555555' },
    integration: { kind: 'remote-branch', ref: 'refs/remotes/origin/main' },
    baseRef: '5555555555555555555555555555555555555555',
  },
  legacy: { kind: 'worktree', repo: '/fixtures/repo', branch: 'tandem/legacy/api-1', baseRef: 'main' },
};

const clientMessages: ClientMsg[] = [
  { t: 'subscribe', agentId: 'api-1', channels: ['transcript', 'status'], sinceSeq: 7, corrId: 'c-1' },
  { t: 'prompt', agentId: 'api-1', blocks: [{ type: 'text', text: 'inspect' }, { type: 'image', assetId: 'sha256:fixture', mimeType: 'image/png', name: 'pixel.png' }], corrId: 'c-2' },
  { t: 'spawn_agent', spec: { adapter: 'acp', agent: 'codex', profile: 'balanced', workspace: workspaces.create as any, sessionConfig: { modeId: 'default', configOptions: { model: 'fixture-model', reasoning: true } } }, corrId: 'c-3' },
  { t: 'browser_control', agentId: 'api-1', action: 'grab', corrId: 'c-4' },
  { t: 'close_agent', agentId: 'api-1', force: true, deleteWorktree: false, corrId: 'c-5' },
];

const events: AgentEvent[] = [
  { kind: 'user_message', text: 'inspect' },
  { kind: 'message_chunk', text: 'working' },
  { kind: 'tool_call', id: 'tool-1', title: 'Read file', status: 'running', rawInput: { path: 'README.md' } },
  { kind: 'permission_request', reqId: 'perm-1', toolCallId: 'tool-1', title: 'Run command', options: [{ optionId: 'allow', name: 'Allow' }] },
  { kind: 'status', status: 'blocked' },
  { kind: 'raw_pty', data: new Uint8Array([0, 10, 255]) },
];

// Exercise the production WS serializer, especially its binary-to-base64 edge.
const wireEvents = events.map(serializeWireEvent);
const serverMessages: ServerMsg[] = [
  { t: 'snapshot', agentId: 'api-1', seq: 7, transcript: wireEvents.slice(0, 2).map((event, index) => ({ seq: index + 1, event })), status: 'idle', controlMode: 'transcript', pendingApprovals: [] },
  { t: 'event', agentId: 'api-1', seq: 8, event: wireEvents[2] },
  { t: 'ack', corrId: 'c-3', agentId: 'api-1' },
  { t: 'browser_state', agentId: 'api-1', active: true, controlOwner: 'user' },
];

const records: AgentRecord[] = [
  { id: 'api-1', name: 'api-1', spec: { adapter: 'acp', agent: 'codex', workspace: workspaces.create as any }, cwd: '/fixtures/worktrees/repo/api-1', acpSessionId: 'sess_fixture', status: 'idle', createdAt: 1700000000000, closedAt: null },
  { id: 'legacy-2', name: 'legacy-2', spec: { adapter: 'acp', workspace: workspaces.legacy as any }, cwd: '/fixtures/worktrees/repo/legacy-2', acpSessionId: null, status: 'error', createdAt: 1700000001000, closedAt: 1700000002000 },
];

const configExamples = {
  shipped: YAML.parse(fs.readFileSync(fileURLToPath(new URL('../../config.yml.example', import.meta.url)), 'utf8')),
  overlay: {
    version: 1,
    defaults: { agent: 'custom', profile: 'custom-fast' },
    agents: { custom: { name: 'Custom Agent', acp: { command: '{node}', args: ['{daemonRoot}/src/mock-acp-agent.mjs'], env: { FIXTURE_HOME: '{home}' } } } },
    profiles: { 'custom-fast': { agent: 'custom', name: 'Custom Fast', acpArgs: ['--fast'], terminalArgs: [] } },
  },
};

export function createSeededDatabase(dbPath: string): void {
  const db = new Db(dbPath);
  for (const record of records) db.upsertAgent(record);
  events.forEach((event, index) => db.appendEvent('api-1', { seq: index + 1, event, ts: 1700000010000 + index }));
  db.putAsset('api-1', { id: 'sha256:fixture', mimeType: 'image/png', size: 68 });
  db.close();
}

function schemaFixture(): unknown {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'tandem-contract-schema-'));
  const dbPath = path.join(dir, 'tandem.db');
  createSeededDatabase(dbPath);
  const sqlite = new Database(dbPath);
  const schema = sqlite.prepare("SELECT type, name, tbl_name AS tableName, sql FROM sqlite_master WHERE type IN ('table', 'index') AND (type = 'index' OR name NOT LIKE 'sqlite_%') ORDER BY type, name").all();
  const pragmas = {
    journal_mode: (sqlite.pragma('journal_mode', { simple: true }) as string).toLowerCase(),
    synchronous: sqlite.pragma('synchronous', { simple: true }),
    foreign_keys: sqlite.pragma('foreign_keys', { simple: true }),
    user_version: sqlite.pragma('user_version', { simple: true }),
  };
  sqlite.close();
  fs.rmSync(dir, { recursive: true, force: true });
  return { pragmas, schema };
}

export function generatedFixtures(): Record<string, unknown> {
  return {
    'agent-records.json': records,
    'client-messages.json': clientMessages,
    'config-examples.json': configExamples,
    'normalized-events.json': wireEvents,
    'server-messages.json': serverMessages,
    'sqlite-schema.json': schemaFixture(),
    'startup.json': {
      readyLine: { stream: 'stdout', pattern: '^TANDEM_READY port=(\\d+) token=(\\S+)$' },
      normalShutdown: { signals: ['SIGINT', 'SIGTERM'], exitCode: 0 },
      startupFailure: { stream: 'stderr', exitCode: 1 },
      precedingOutput: ['tandem daemon · http+ws on <bind>:<port> · home <home>', 'browser: driver=<driver> mcp=<on|off>', 'restored <count> agent(s)', 'bootstrap: <url>'],
    },
    'workspaces.json': workspaces,
  };
}

export function renderFixtures(): Map<string, string> {
  return new Map(Object.entries(generatedFixtures()).map(([name, value]) => [name, `${JSON.stringify(value, null, 2)}\n`]));
}

function main(): void {
  fs.mkdirSync(fixtureDir, { recursive: true });
  for (const [name, content] of renderFixtures()) fs.writeFileSync(path.join(fixtureDir, name), content);
}

if (process.argv[1] === fileURLToPath(import.meta.url)) main();
