// Restart de-risk (D11 + D14): spawn an agent, write events, stop the daemon
// PROCESS cleanly, start a NEW daemon process against the same TANDEM_HOME, and
// assert:
//   * the agent is restored (a fresh subprocess is spawned, and because the mock
//     advertises loadSession it receives session/load with the persisted id);
//   * a client subscribing with sinceSeq 0 receives the pre-restart history from
//     SQLite (the in-memory ring is empty in the new process);
//   * the seq stream continues past the pre-restart head (no reset).
//
// Unlike the other scripts this runs the real daemon entrypoint as a child
// process, so it exercises restore end-to-end. Run: npm run derisk:restart

import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawn, type ChildProcess } from 'node:child_process';
import { WebSocket } from 'ws';
import { rule, report, type Frame } from './testHarness.ts';

const PORT = 7725;
const mockPath = new URL('./mock-acp-agent.mjs', import.meta.url).pathname;
const indexPath = new URL('./index.ts', import.meta.url).pathname;
const tsxBin = new URL('../node_modules/.bin/tsx', import.meta.url).pathname;
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

interface Daemon {
  proc: ChildProcess;
  token: string;
  stderr: () => string;
  stop: () => Promise<void>;
}

function startDaemon(home: string): Promise<Daemon> {
  return new Promise((resolve, reject) => {
    const proc = spawn(tsxBin, [indexPath], {
      env: {
        ...process.env,
        TANDEM_HOME: home,
        TANDEM_PORT: String(PORT),
        TANDEM_BIND: '127.0.0.1',
        TANDEM_ACP_CMD: JSON.stringify([process.execPath, mockPath]),
      },
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    let out = '';
    let err = '';
    proc.stdout!.on('data', (d) => {
      out += d.toString();
      const m = /TANDEM_READY port=\d+ token=(\S+)/.exec(out);
      if (m) {
        resolve({
          proc,
          token: m[1],
          stderr: () => err,
          stop: () =>
            new Promise<void>((res) => {
              proc.on('exit', () => res());
              proc.kill('SIGTERM');
            }),
        });
      }
    });
    proc.stderr!.on('data', (d) => (err += d.toString()));
    proc.on('exit', (code) => reject(new Error(`daemon exited early (code ${code})\n${err}`)));
    setTimeout(() => reject(new Error('daemon did not become ready in 10s')), 10000);
  });
}

function connect(token: string, onFrame: (f: Frame) => void): Promise<WebSocket> {
  return new Promise((res, rej) => {
    const ws = new WebSocket(`ws://127.0.0.1:${PORT}/?token=${token}`);
    ws.on('open', () => res(ws));
    ws.on('error', rej);
    ws.on('message', (raw) => onFrame(JSON.parse(raw.toString())));
  });
}

async function main() {
  console.log('\n' + rule);
  console.log('  TANDEM · restart de-risk (persist → stop → restore, D11/D14)');
  console.log(rule);

  const home = fs.mkdtempSync(path.join(os.tmpdir(), 'tandem-restart-'));
  console.log(`\n  ▸ TANDEM_HOME = ${home}`);

  // ---- daemon #1: spawn an agent and generate events ----
  const d1 = await startDaemon(home);
  let agentId = '';
  let preSeq = 0;
  const seenPre: number[] = [];
  const ws1 = await connect(d1.token, (f) => {
    if (f.t === 'ack' && f.corrId === 'sp' && f.agentId) agentId = f.agentId;
    if (f.agentId === agentId) {
      if (f.t === 'snapshot') f.transcript!.forEach((e) => seenPre.push(e.seq));
      if (f.t === 'event') seenPre.push(f.seq!);
    }
  });
  ws1.send(JSON.stringify({ t: 'spawn_agent', corrId: 'sp', spec: { adapter: 'acp', workspace: { kind: 'existing', cwd: process.cwd() } } }));
  await sleep(300);
  ws1.send(JSON.stringify({ t: 'subscribe', agentId, sinceSeq: 0 }));
  // Let heartbeats + a prompt turn accrue, answering the permission so a full
  // turn (incl. a "Done" message) lands in the persisted log.
  ws1.send(JSON.stringify({ t: 'prompt', agentId, text: 'go' }));
  await sleep(700);
  // answer any pending permission via a fresh subscribe snapshot is overkill;
  // just wait for heartbeats — the point is durable events, not turn completion.
  await sleep(400);
  preSeq = Math.max(...seenPre);
  console.log(`  ▸ agent ${agentId} produced events up to seq ${preSeq}`);
  ws1.close();

  // ---- stop daemon #1 cleanly ----
  await d1.stop();
  console.log('  ✖ daemon #1 stopped cleanly (SIGTERM)');

  // ---- daemon #2: must restore from SQLite ----
  const d2 = await startDaemon(home);
  await sleep(400); // give restore's session/load time to round-trip
  console.log('  ▸ daemon #2 started — restoring from SQLite');

  const seenPost: { seq: number; kind: string }[] = [];
  let snapSeq = 0;
  let restoredStatus = '';
  const ws2 = await connect(d2.token, (f) => {
    if (f.agentId !== agentId) return;
    if (f.t === 'snapshot') {
      snapSeq = f.seq!;
      restoredStatus = f.status!;
      f.transcript!.forEach((e) => seenPost.push({ seq: e.seq, kind: e.event.kind }));
    }
    if (f.t === 'event') seenPost.push({ seq: f.seq!, kind: f.event.kind });
  });
  ws2.send(JSON.stringify({ t: 'subscribe', agentId, sinceSeq: 0 }));
  await sleep(700); // observe post-restart heartbeats continuing the seq stream
  ws2.close();

  const historySeqs = seenPost.map((e) => e.seq).sort((a, b) => a - b);
  const gotFullHistory = historySeqs.includes(1) && Math.max(...historySeqs.filter((s) => s <= preSeq), 0) === preSeq;
  const loadMarker = /MOCK_LOADSESSION (\S+)/.exec(d2.stderr());
  const restored = !!loadMarker;
  // The seq stream continues past the pre-restart head (new heartbeats appended).
  const continued = Math.max(...historySeqs) > preSeq;

  const checks: [string, boolean, string][] = [
    ['agent restored after daemon restart', restored, `session/load fired for ${loadMarker?.[1] ?? '—'}`],
    ['client got pre-restart history from SQLite', gotFullHistory && preSeq > 0, `snapshot covers seq 1..${preSeq} (preSeq=${preSeq})`],
    ['seq stream continued past pre-restart head', continued, `pre=${preSeq} → post=${Math.max(...historySeqs)}`],
    ['restored agent has a live status', restoredStatus === 'idle' || restoredStatus === 'working', `status=${restoredStatus}, snapSeq=${snapSeq}`],
  ];

  const pass = report(checks);
  await d2.stop();
  fs.rmSync(home, { recursive: true, force: true });
  process.exit(pass ? 0 : 1);
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
