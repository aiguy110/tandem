// Automated proof of the spine's two riskiest theses:
//
//   1. DURABILITY — the daemon owns the agent process; a client socket can die
//      (simulated dropped SSH pipe) and reconnect with GAPLESS seq-replay while
//      the agent keeps working the entire time.
//   2. ACP PATH — a real JSON-RPC-over-stdio adapter translates session/update
//      into normalized events and round-trips session/request_permission.
//
// Run: npm run derisk

import { WebSocket } from 'ws';
import { AcpAdapter } from './acpAdapter.ts';
import { AgentSession } from './session.ts';
import { startServer } from './server.ts';

const PORT = 7719;
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
const rule = '─'.repeat(64);
const mockPath = new URL('./mock-acp-agent.mjs', import.meta.url).pathname;

interface Frame {
  t: string;
  seq?: number;
  event?: any;
  events?: { seq: number; event: any }[];
}

function connect(sinceSeq: number, onFrame: (f: Frame) => void): Promise<WebSocket> {
  return new Promise((res) => {
    const ws = new WebSocket(`ws://localhost:${PORT}`);
    ws.on('open', () => {
      ws.send(JSON.stringify({ t: 'subscribe', agentId: 'web-1', sinceSeq }));
      res(ws);
    });
    ws.on('message', (raw) => onFrame(JSON.parse(raw.toString())));
  });
}

const tickOf = (ev: any): number | null => {
  const m = /tick #(\d+)/.exec(ev?.text ?? '');
  return m ? +m[1] : null;
};

async function main() {
  console.log('\n' + rule);
  console.log('  TANDEM · pty/ACP spine — de-risk harness');
  console.log(rule);

  // ---- daemon in-process; the ACP agent is a real subprocess ----
  const adapter = new AcpAdapter('web-1', { cmd: process.execPath, args: [mockPath] });
  const session = new AgentSession('web-1', adapter);
  await session.start({ cwd: process.cwd() });
  const wss = startServer(session, PORT);
  const pidBefore = session.agentPid;
  console.log(`\n  ▸ daemon up · ACP agent subprocess pid=${pidBefore}`);

  await sleep(700); // build some heartbeat scrollback before anyone connects

  // ---- Phase 1: attach, collect, then HARD-KILL the socket ----
  const seenA: number[] = [];
  const ticksA: number[] = [];
  const ws = await connect(0, (f) => {
    const take = (seq: number, ev: any) => {
      seenA.push(seq);
      const t = tickOf(ev);
      if (t !== null) ticksA.push(t);
    };
    if (f.t === 'snapshot') f.events!.forEach((e) => take(e.seq, e.event));
    if (f.t === 'event') take(f.seq!, f.event);
  });
  await sleep(700);
  const lastSeq = Math.max(...seenA);
  console.log(`\n  ▸ client A  subscribe@0   → ${seenA.length} events, up to seq ${lastSeq}`);
  ws.terminate(); // simulate SSH/pipe death — no close handshake
  console.log('  ✖ client A socket TERMINATED (simulated dropped pipe)');

  // ---- gap: agent works on with nobody attached ----
  await sleep(900);
  console.log('  … 900ms elapsed with NO client attached (agent still running)');

  // ---- Phase 2: reconnect from checkpoint, expect gapless replay ----
  const seenB: number[] = [];
  const ticksB: number[] = [];
  let firstReplay: number | undefined;
  let sawPerm = false;
  let sawDone = false;
  let permReqId = '';

  const ws2 = await connect(lastSeq, (f) => {
    const take = (seq: number, ev: any) => {
      if (firstReplay === undefined) firstReplay = seq;
      seenB.push(seq);
      const t = tickOf(ev);
      if (t !== null) ticksB.push(t);
      if (ev?.kind === 'permission_request') {
        sawPerm = true;
        permReqId = ev.reqId;
        ws2.send(JSON.stringify({ t: 'permission_response', agentId: 'web-1', reqId: ev.reqId, optionId: 'allow' }));
      }
      if (ev?.kind === 'message_chunk' && /Done/.test(ev.text)) sawDone = true;
    };
    if (f.t === 'snapshot') f.events!.forEach((e) => take(e.seq, e.event));
    if (f.t === 'event') take(f.seq!, f.event);
  });
  await sleep(500);
  console.log(`  ▸ client B  subscribe@${lastSeq}  → ${seenB.length} events, first replayed seq=${firstReplay}`);

  // ---- approval round-trip over the reconnected socket ----
  ws2.send(JSON.stringify({ t: 'prompt', agentId: 'web-1', text: 'reinstall deps and run tests' }));
  await sleep(900);
  const pidAfter = session.agentPid;

  // ---- assertions ----
  const merged = [...seenA, ...seenB.filter((s) => s > lastSeq)].sort((a, b) => a - b);
  const gapless = merged.every((s, i) => i === 0 || s === merged[i - 1] + 1);
  const seamOk = firstReplay === lastSeq + 1;
  const allTicks = [...ticksA, ...ticksB];
  const uniqTicks = [...new Set(allTicks)].sort((a, b) => a - b);
  const ticksGapless = uniqTicks.every((t, i) => i === 0 || t === uniqTicks[i - 1] + 1);
  const pidSurvived = pidBefore !== undefined && pidBefore === pidAfter;

  const checks: [string, boolean, string][] = [
    ['agent subprocess survived the disconnect', pidSurvived, `pid ${pidBefore} → ${pidAfter}`],
    ['reconnect replayed from exactly lastSeq+1', seamOk, `first replayed = ${firstReplay}, expected ${lastSeq + 1}`],
    ['no seq gaps across the disconnect', gapless, `${merged.length} events, seq ${merged[0]}..${merged.at(-1)}`],
    ['no heartbeat ticks lost during the gap', ticksGapless, `ticks ${uniqTicks[0]}..${uniqTicks.at(-1)} contiguous (${uniqTicks.length})`],
    ['ACP permission delivered after reconnect', sawPerm, `reqId=${permReqId || '—'}`],
    ['approval → agent completed the turn', sawDone, sawDone ? 'received "Done" message' : 'no completion seen'],
  ];

  console.log('\n' + rule);
  console.log('  RESULTS');
  console.log(rule);
  let pass = true;
  for (const [name, ok, detail] of checks) {
    pass = pass && ok;
    console.log(`  ${ok ? '✅' : '❌'}  ${name}`);
    console.log(`        ${detail}`);
  }
  console.log(rule);
  console.log(
    pass
      ? '  ✅ SPINE DE-RISKED — daemon-owns-state survives pipe death,\n     reconnects gaplessly, and the ACP approval loop works end-to-end.'
      : '  ❌ FAILURES ABOVE',
  );
  console.log(rule + '\n');

  ws2.close();
  wss.close();
  await session.dispose();
  process.exit(pass ? 0 : 1);
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
