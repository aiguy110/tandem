// Deferred-restart de-risk: a restart request reports active turns, remains
// idempotent, and asks the daemon to shut down only after the last turn ends.

import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { WebSocket } from 'ws';
import { report, sleep, mockPath } from './testHarness.ts';
import { parseDaemonCommand, startDaemon } from './processHarness.ts';

const PORT = 7761;

async function request(port: number, token: string): Promise<[number, number]> {
  const response = await fetch(`http://127.0.0.1:${port}/internal/shutdown-after-turns?token=${token}`, { method: 'POST' });
  if (!response.ok) return [-response.status, -response.status];
  const [active, already] = (await response.text()).trim().split(/\s+/).map(Number);
  return [active, already];
}

async function main() {
  const home = fs.mkdtempSync(path.join(os.tmpdir(), 'tandem-deferred-home-'));
  const daemon = await startDaemon({ command: parseDaemonCommand(), home, port: Number(process.env.TANDEM_TEST_PORT || PORT), env: {
    TANDEM_ACP_CMD: JSON.stringify([process.execPath, mockPath]), TANDEM_BROWSER_MCP: 'off',
  } });
  const h = { port: daemon.port, token: daemon.token };
  const checks: [string, boolean, string][] = [];
  const frames: any[] = [];
  const ws = new WebSocket(`ws://127.0.0.1:${h.port}/?token=${h.token}`);
  ws.on('message', (raw) => frames.push(JSON.parse(raw.toString())));
  await new Promise<void>((resolve, reject) => { ws.once('open', resolve); ws.once('error', reject); });
  ws.send(JSON.stringify({ t: 'spawn_agent', corrId: 'spawn', spec: { adapter: 'acp', workspace: { kind: 'existing', cwd: home } } }));
  for (let i = 0; i < 200 && !frames.find((f) => f.corrId === 'spawn'); i++) await sleep(25);
  const agentId = frames.find((f) => f.corrId === 'spawn')?.agentId;
  if (!agentId) throw new Error('spawn failed');
  ws.send(JSON.stringify({ t: 'subscribe', agentId, sinceSeq: 0 }));
  ws.send(JSON.stringify({ t: 'prompt', agentId, text: 'DERISK_SLOWTERM' }));
  await sleep(150);

  const first = await request(h.port, h.token);
  const second = await request(h.port, h.token);
  checks.push(['first request reports one active turn and sets the flag', first[0] === 1 && first[1] === 0, `response=${first.join(' ')}`]);
  checks.push(['repeat request reports the flag was already set', second[0] === 1 && second[1] === 1, `response=${second.join(' ')}`]);

  await sleep(300);
  const aliveDuringTurn = daemon.proc.exitCode === null;
  checks.push(['daemon stays up while the turn is active', aliveDuringTurn, `alive=${aliveDuringTurn}`]);
  for (let i = 0; i < 200 && daemon.proc.exitCode === null; i++) await sleep(25);
  const exitedAfterTurn = daemon.proc.exitCode !== null;
  checks.push(['daemon exits once after the turn finishes', exitedAfterTurn, `exitCode=${daemon.proc.exitCode}`]);

  const pass = report(checks);
  ws.close();
  await daemon.stop();
  fs.rmSync(home, { recursive: true, force: true });
  process.exit(pass ? 0 : 1);
}

main().catch((error) => {
  console.error(error);
  process.exit(1);
});
