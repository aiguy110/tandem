// Multi-agent de-risk: spawn TWO ACP agents via spawn_agent WS messages and
// prove they are fully independent —
//   * independent seq streams (each agent's log is its own),
//   * independent permission queues (answering one doesn't touch the other),
//   * close_agent tears down ONLY the target (the other keeps running).
//
// Run: npm run derisk:multi

import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { WebSocket } from 'ws';
import { open, sleep, rule, report, mockPath, type Frame } from './testHarness.ts';
import { parseDaemonCommand, startDaemon } from './processHarness.ts';

const PORT = 7721;

// Collector for one agent's frames on a socket.
interface Collected {
  seqs: number[];
  perms: { reqId: string }[];
  closed: boolean;
  status?: string;
  snapshotSeq?: number;
}

async function spawnAgent(ws: WebSocket, cwd: string): Promise<string> {
  return new Promise((res) => {
    const corrId = 'spawn-' + Math.random().toString(36).slice(2);
    const onMsg = (raw: any) => {
      const f = JSON.parse(raw.toString()) as Frame;
      if (f.t === 'ack' && f.corrId === corrId) {
        ws.off('message', onMsg);
        res(f.agentId!);
      }
    };
    ws.on('message', onMsg);
    ws.send(JSON.stringify({ t: 'spawn_agent', corrId, spec: { adapter: 'acp', workspace: { kind: 'existing', cwd } } }));
  });
}

async function main() {
  console.log('\n' + rule);
  console.log('  TANDEM · multi-agent de-risk (two independent ACP agents)');
  console.log(rule);

  const home = fs.mkdtempSync(path.join(os.tmpdir(), 'tandem-multi-home-'));
  const daemon = await startDaemon({ command: parseDaemonCommand(), home, port: Number(process.env.TANDEM_TEST_PORT || PORT), env: {
    TANDEM_ACP_CMD: JSON.stringify([process.execPath, mockPath]), TANDEM_BROWSER_MCP: 'off',
  } });
  const h = { port: daemon.port, token: daemon.token, stop: async () => { await daemon.stop(); fs.rmSync(home, { recursive: true, force: true }); } };

  // Two distinct existing-dirs: Phase 2 collision detection now refuses a
  // second kind:'existing' spawn into a dir a live agent already occupies
  // (docs D8), so each agent here needs its own directory.
  const cwdB = fs.mkdtempSync(path.join(os.tmpdir(), 'tandem-multi-b-'));

  // A control socket used only to spawn agents.
  const ctl = await open(h.port, h.token, () => {});
  const idA = await spawnAgent(ctl, process.cwd());
  const idB = await spawnAgent(ctl, cwdB);
  console.log(`\n  ▸ spawned two agents: ${idA}, ${idB}`);

  const col: Record<string, Collected> = { [idA]: mk(), [idB]: mk() };
  function mk(): Collected {
    return { seqs: [], perms: [], closed: false };
  }

  // One viewer socket subscribed to BOTH agents (multiplexing by agentId).
  const view = await open(h.port, h.token, (f: Frame) => {
    const c = f.agentId ? col[f.agentId] : undefined;
    if (!c) return;
    if (f.t === 'snapshot') {
      c.snapshotSeq = f.seq;
      c.status = f.status;
      f.transcript!.forEach((e) => c.seqs.push(e.seq));
    }
    if (f.t === 'event') {
      c.seqs.push(f.seq!);
      if (f.event?.kind === 'permission_request') c.perms.push({ reqId: f.event.reqId });
    }
    if (f.t === 'agent_closed') c.closed = true;
  });
  view.send(JSON.stringify({ t: 'subscribe', agentId: idA, sinceSeq: 0 }));
  view.send(JSON.stringify({ t: 'subscribe', agentId: idB, sinceSeq: 0 }));
  await sleep(800);

  // Prompt only agent A → only A should raise a permission request.
  view.send(JSON.stringify({ t: 'prompt', agentId: idA, text: 'do the thing' }));
  await sleep(600);
  const permsAOnly = col[idA].perms.length === 1 && col[idB].perms.length === 0;

  // Answer A's permission; B still untouched.
  const reqId = col[idA].perms[0]?.reqId;
  if (reqId) view.send(JSON.stringify({ t: 'permission_response', agentId: idA, reqId, optionId: 'allow' }));
  await sleep(400);

  // Each agent has its own seq stream, both starting at 1 (independent logs).
  const aStartsAt1 = col[idA].seqs[0] === 1;
  const bStartsAt1 = col[idB].seqs[0] === 1;
  const aAdvanced = Math.max(...col[idA].seqs) > Math.max(...col[idB].seqs); // A got the prompt traffic
  const independentStreams = aStartsAt1 && bStartsAt1 && aAdvanced;

  const bTicksBeforeClose = Math.max(...col[idB].seqs);

  // Close ONLY agent A. B must keep producing heartbeats.
  view.send(JSON.stringify({ t: 'close_agent', agentId: idA }));
  await sleep(700);
  const bStillLive = Math.max(...col[idB].seqs) > bTicksBeforeClose;
  view.send(JSON.stringify({ t: 'list_agents', corrId: 'after-close' }));
  await sleep(100);
  const live = await new Promise<any[]>((resolve) => {
    const onMsg = (raw: any) => { const f = JSON.parse(raw.toString()); if (f.t === 'agents' && f.corrId === 'after-close') { view.off('message', onMsg); resolve(f.agents ?? []); } };
    view.on('message', onMsg); view.send(JSON.stringify({ t: 'list_agents', corrId: 'after-close' }));
    setTimeout(() => resolve([]), 2000);
  });
  const aTornDown = col[idA].closed && !live.some((a) => a.id === idA);
  const bUntouched = live.some((a) => a.id === idB) && !col[idB].closed;

  const checks: [string, boolean, string][] = [
    ['two agents spawned via spawn_agent', !!idA && !!idB && idA !== idB, `${idA}, ${idB}`],
    ['independent seq streams (both start at 1)', independentStreams, `A max=${Math.max(...col[idA].seqs)}, B max=${Math.max(...col[idB].seqs)}`],
    ['permission raised on A only (independent queues)', permsAOnly, `A perms=${col[idA].perms.length}, B perms=${col[idB].perms.length}`],
    ['close_agent tore down only the target', aTornDown && bUntouched, `A gone=${aTornDown}, B alive=${bUntouched}`],
    ['non-target agent kept running after close', bStillLive, `B seq ${bTicksBeforeClose} → ${Math.max(...col[idB].seqs)}`],
  ];

  const pass = report(checks);
  ctl.close();
  view.close();
  await h.stop();
  fs.rmSync(cwdB, { recursive: true, force: true });
  process.exit(pass ? 0 : 1);
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
