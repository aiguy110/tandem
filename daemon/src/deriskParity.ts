// Black-box implementation parity gate. Both daemons are driven exclusively
// through their public HTTP/WebSocket surface and compared after normalizing
// implementation-specific IDs, PIDs, timestamps, ports, and temp paths.

import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { execFileSync } from 'node:child_process';
import { WebSocket } from 'ws';
import { assertPortReleased, parseDaemonCommand, startDaemon, type DaemonCommand, type RunningDaemon } from './processHarness.ts';
import { mockPath, sleep } from './testHarness.ts';

type Frame = Record<string, any>;
type CheckMap = Record<string, boolean>;
const PNG = Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=', 'base64');

function commandFrom(name: string): DaemonCommand {
  const raw = process.env[name];
  if (!raw) throw new Error(`${name} is required (JSON command array)`);
  return parseDaemonCommand(raw);
}

function git(cwd: string, ...args: string[]): string {
  return execFileSync('git', args, { cwd, encoding: 'utf8' }).trim();
}

function makeRepo(prefix: string): { root: string; repo: string } {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), prefix));
  const repo = path.join(root, 'checkout');
  fs.mkdirSync(repo);
  git(repo, 'init', '-q', '-b', 'main');
  git(repo, 'config', 'user.email', 'parity@tandem.dev');
  git(repo, 'config', 'user.name', 'Parity');
  fs.writeFileSync(path.join(repo, 'README.md'), '# parity\n');
  git(repo, 'add', '-A');
  git(repo, 'commit', '-qm', 'initial');
  return { root, repo };
}

class Client {
  frames: Frame[] = [];
  private constructor(readonly ws: WebSocket) {}
  static async open(daemon: RunningDaemon): Promise<Client> {
    const ws = new WebSocket(`ws://127.0.0.1:${daemon.port}/?token=${daemon.token}`);
    const client = new Client(ws);
    ws.on('message', (raw) => client.frames.push(JSON.parse(raw.toString())));
    await new Promise<void>((resolve, reject) => { ws.once('open', resolve); ws.once('error', reject); });
    return client;
  }
  send(value: unknown): void { this.ws.send(JSON.stringify(value)); }
  async rpc(value: Frame, expected = 'ack', timeout = 10_000): Promise<Frame> {
    const corrId = value.corrId ?? `c-${Math.random().toString(36).slice(2)}`;
    const before = this.frames.length;
    this.send({ ...value, corrId });
    return waitFor(() => this.frames.slice(before).find((f) => f.corrId === corrId && f.t === expected), timeout, `${value.t} response`);
  }
  events(agentId: string): Frame[] {
    const snapshot = this.frames.filter((f) => f.t === 'snapshot' && f.agentId === agentId).flatMap((f) => f.transcript ?? []);
    const streamed = this.frames.filter((f) => f.t === 'event' && f.agentId === agentId).map((f) => ({ seq: f.seq, event: f.event }));
    return [...snapshot, ...streamed];
  }
  close(hard = false): void { hard ? this.ws.terminate() : this.ws.close(); }
}

async function waitFor<T>(read: () => T | undefined, timeout: number, description: string): Promise<T> {
  const deadline = Date.now() + timeout;
  while (Date.now() < deadline) {
    const value = read();
    if (value !== undefined) return value;
    await sleep(25);
  }
  throw new Error(`timed out waiting for ${description}`);
}

async function rejected(port: number, token?: string): Promise<number> {
  const suffix = token === undefined ? '' : `?token=${token}`;
  const ws = new WebSocket(`ws://127.0.0.1:${port}/${suffix}`);
  return new Promise((resolve) => { ws.once('close', resolve); ws.once('error', () => {}); });
}

function daemonEnv(projectRoot: string): NodeJS.ProcessEnv {
  return {
    TANDEM_ACP_CMD: JSON.stringify([process.execPath, mockPath]),
    TANDEM_PROJECT_ROOTS: projectRoot,
    TANDEM_BROWSER_MCP: 'off',
    TANDEM_NO_PTY: '1',
  };
}

async function runRuntime(label: string, command: DaemonCommand): Promise<CheckMap> {
  const { root, repo } = makeRepo(`tandem-parity-${label}-proj-`);
  const home = fs.mkdtempSync(path.join(os.tmpdir(), `tandem-parity-${label}-home-`));
  let daemon: RunningDaemon | undefined;
  const checks: CheckMap = {};
  try {
    daemon = await startDaemon({ command, home, env: daemonEnv(root) });
    checks.auth_no_token = await rejected(daemon.port) === 4401;
    checks.auth_bad_token = await rejected(daemon.port, 'wrong') === 4401;

    let client = await Client.open(daemon);
    const dirs = await client.rpc({ t: 'list_dirs' }, 'dirs');
    checks.repo_discovery = dirs.dirs?.some((entry: any) => entry.path === repo) === true;

    const spawn1 = await client.rpc({ t: 'spawn_agent', spec: { adapter: 'acp', workspace: { kind: 'worktree', repo } } });
    const spawn2 = await client.rpc({ t: 'spawn_agent', spec: { adapter: 'acp', workspace: { kind: 'worktree', repo } } });
    assert.ok(spawn1.agentId && spawn2.agentId && spawn1.agentId !== spawn2.agentId, `${label}: failed to spawn distinct agents`);
    const a1 = spawn1.agentId as string;
    const a2 = spawn2.agentId as string;
    client.send({ t: 'subscribe', agentId: a1, sinceSeq: 0 });
    client.send({ t: 'subscribe', agentId: a2, sinceSeq: 0 });
    await waitFor(() => client.frames.find((f) => f.t === 'snapshot' && f.agentId === a1), 5_000, 'first snapshot');
    await waitFor(() => client.frames.find((f) => f.t === 'snapshot' && f.agentId === a2), 5_000, 'second snapshot');
    checks.multi_agent_independent = client.events(a1).some((e) => e.seq === 1) && client.events(a2).some((e) => e.seq === 1);
    checks.worktrees_isolated = fs.readdirSync(path.join(home, 'worktrees', 'checkout')).length === 2;

    client.send({ t: 'prompt', agentId: a1, text: 'parity approval turn' });
    const permission = await waitFor(() => client.events(a1).map((e) => e.event).find((e) => e.kind === 'permission_request'), 8_000, 'permission request');
    client.send({ t: 'permission_response', agentId: a1, reqId: permission.reqId, optionId: permission.options?.[0]?.optionId ?? 'allow' });
    await waitFor(() => client.events(a1).map((e) => e.event).find((e) => e.kind === 'message_chunk' && /done/i.test(e.text ?? '')), 8_000, 'turn completion');
    checks.acp_permission_roundtrip = true;
    checks.normalized_usage = client.events(a1).some((entry) => entry.event.kind === 'usage' && entry.event.used === 12300 && entry.event.size === 1000000);

    const upload = await fetch(`http://127.0.0.1:${daemon.port}/api/agents/${a1}/assets`, {
      method: 'POST', headers: { authorization: `Bearer ${daemon.token}`, 'content-type': 'image/png', 'x-file-name': 'parity.png' }, body: PNG,
    });
    const assetId = (await upload.json() as any).asset?.assetId;
    const download = await fetch(`http://127.0.0.1:${daemon.port}/api/agents/${a1}/assets/${assetId}`, { headers: { authorization: `Bearer ${daemon.token}` } });
    checks.asset_roundtrip = upload.status === 201 && download.status === 200 && Buffer.from(await download.arrayBuffer()).equals(PNG);

    const lastSeq = Math.max(...client.events(a1).map((e) => Number(e.seq)));
    client.close(true);
    client = await Client.open(daemon);
    client.send({ t: 'subscribe', agentId: a1, sinceSeq: lastSeq - 1 });
    const replay = await waitFor(() => client.events(a1).find((e) => e.seq === lastSeq), 5_000, 'checkpoint replay');
    checks.checkpoint_replay = replay.seq === lastSeq;

    const close1 = await client.rpc({ t: 'close_agent', agentId: a1, force: true });
    const close2 = await client.rpc({ t: 'close_agent', agentId: a2, force: true });
    checks.close_agents = !close1.error && !close2.error;
    await waitFor(() => fs.existsSync(path.join(home, 'worktrees', 'checkout')) && fs.readdirSync(path.join(home, 'worktrees', 'checkout')).length === 0 ? true : undefined, 5_000, 'worktree cleanup');
    checks.worktree_cleanup = true;
    client.close();
    return checks;
  } finally {
    const port = daemon?.port;
    await daemon?.stop();
    if (port !== undefined) await assertPortReleased(port);
    fs.rmSync(home, { recursive: true, force: true });
    fs.rmSync(root, { recursive: true, force: true });
    assert.equal(fs.existsSync(home), false, `${label}: TANDEM_HOME leaked`);
    assert.equal(fs.existsSync(root), false, `${label}: project/worktrees leaked`);
  }
}

async function crossRestart(writerLabel: string, writer: DaemonCommand, readerLabel: string, reader: DaemonCommand): Promise<void> {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), `tandem-cross-${writerLabel}-${readerLabel}-proj-`));
  const cwd = path.join(root, 'workspace');
  fs.mkdirSync(cwd);
  const home = fs.mkdtempSync(path.join(os.tmpdir(), `tandem-cross-${writerLabel}-${readerLabel}-home-`));
  let first: RunningDaemon | undefined;
  let second: RunningDaemon | undefined;
  try {
    first = await startDaemon({ command: writer, home, env: daemonEnv(root) });
    const c1 = await Client.open(first);
    const spawned = await c1.rpc({ t: 'spawn_agent', spec: { adapter: 'acp', workspace: { kind: 'existing', cwd } } });
    assert.ok(spawned.agentId, `${writerLabel}: spawn failed`);
    const id = spawned.agentId as string;
    c1.send({ t: 'subscribe', agentId: id, sinceSeq: 0 });
    c1.send({ t: 'prompt', agentId: id, text: 'cross runtime persistence' });
    const permission = await waitFor(() => c1.events(id).map((e) => e.event).find((e) => e.kind === 'permission_request'), 8_000, 'cross-runtime permission');
    c1.send({ t: 'permission_response', agentId: id, reqId: permission.reqId, optionId: permission.options?.[0]?.optionId ?? 'allow' });
    await waitFor(() => c1.events(id).find((e) => e.event.kind === 'message_chunk' && /done/i.test(e.event.text ?? '')), 8_000, 'cross-runtime completion');
    const head = Math.max(...c1.events(id).map((e) => Number(e.seq)));
    c1.close();
    const firstPort = first.port;
    await first.stop();
    await assertPortReleased(firstPort);
    first = undefined;

    second = await startDaemon({ command: reader, home, env: daemonEnv(root) });
    const c2 = await Client.open(second);
    c2.send({ t: 'subscribe', agentId: id, sinceSeq: 0 });
    const snapshot = await waitFor(() => c2.frames.find((f) => f.t === 'snapshot' && f.agentId === id), 8_000, 'cross-runtime restore snapshot');
    const seqs = (snapshot.transcript ?? []).map((entry: any) => Number(entry.seq));
    assert.ok(seqs.includes(1) && Math.max(...seqs) >= head, `${writerLabel} -> ${readerLabel}: history did not restore through seq ${head}`);
    const close = await c2.rpc({ t: 'close_agent', agentId: id, force: true });
    assert.equal(close.error, undefined, `${writerLabel} -> ${readerLabel}: restored agent did not close`);
    c2.close();
  } finally {
    const ports = [first?.port, second?.port].filter((value): value is number => value !== undefined);
    await first?.stop();
    await second?.stop();
    for (const port of ports) await assertPortReleased(port);
    fs.rmSync(home, { recursive: true, force: true });
    fs.rmSync(root, { recursive: true, force: true });
  }
}

async function main(): Promise<void> {
  const node = commandFrom('TANDEM_NODE_DAEMON_CMD');
  const go = commandFrom('TANDEM_GO_DAEMON_CMD');
  const nodeResult = await runRuntime('node', node);
  const goResult = await runRuntime('go', go);
  assert.deepEqual(goResult, nodeResult, `normalized observable mismatch\nnode=${JSON.stringify(nodeResult)}\ngo=${JSON.stringify(goResult)}`);
  assert.ok(Object.values(nodeResult).every(Boolean), `parity scenario failed: ${JSON.stringify(nodeResult)}`);
  await crossRestart('node', node, 'go', go);
  await crossRestart('go', go, 'node', node);
  console.log(`parity checks passed: ${Object.keys(nodeResult).sort().join(', ')}`);
  console.log('cross-runtime restart passed: node -> go and go -> node (rollback)');
  console.log('leak checks passed: process groups, ports, worktrees, project roots, and homes');
}

main().catch((error) => { console.error(error); process.exit(1); });
