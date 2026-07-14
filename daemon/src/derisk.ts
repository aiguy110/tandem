// Automated proof of the spine's two riskiest theses, now on the multi-agent
// daemon (registry + SQLite + auth):
//
//   1. DURABILITY — the daemon owns the agent process; a client socket can die
//      (simulated dropped SSH pipe) and reconnect with GAPLESS seq-replay while
//      the agent keeps working the entire time.
//   2. ACP PATH — a JSON-RPC-over-stdio adapter translates session/update into
//      normalized events and round-trips session/request_permission.
//
// Run: npm run derisk

import { makeHarness, open, sleep, rule, report, type Frame } from './testHarness.ts';

const PORT = 7719;

const tickOf = (ev: any): number | null => {
  const m = /tick #(\d+)/.exec(ev?.text ?? '');
  return m ? +m[1] : null;
};

async function main() {
  console.log('\n' + rule);
  console.log('  TANDEM · durability + ACP spine — de-risk harness');
  console.log(rule);

  const h = await makeHarness(PORT);
  const session = await h.registry.spawn({ adapter: 'acp', workspace: { kind: 'existing', cwd: process.cwd() }, name: 'web-1' });
  const agentId = session.id;
  const pidBefore = session.agentPid;
  console.log(`\n  ▸ daemon up · agent ${agentId} · ACP subprocess pid=${pidBefore}`);

  await sleep(700); // build some heartbeat scrollback before anyone connects

  // ---- Phase 1: attach, collect, then HARD-KILL the socket ----
  const seenA: number[] = [];
  const ticksA: number[] = [];
  const take = (arr: number[], ticks: number[], seq: number, ev: any) => {
    arr.push(seq);
    const t = tickOf(ev);
    if (t !== null) ticks.push(t);
  };
  const wsA = await open(h.port, h.token, (f: Frame) => {
    if (f.t === 'snapshot') f.transcript!.forEach((e) => take(seenA, ticksA, e.seq, e.event));
    if (f.t === 'event') take(seenA, ticksA, f.seq!, f.event);
  });
  wsA.send(JSON.stringify({ t: 'subscribe', agentId, sinceSeq: 0 }));
  await sleep(700);
  const lastSeq = Math.max(...seenA);
  console.log(`\n  ▸ client A  subscribe@0   → ${seenA.length} events, up to seq ${lastSeq}`);
  wsA.terminate(); // simulate SSH/pipe death — no close handshake
  console.log('  ✖ client A socket TERMINATED (simulated dropped pipe)');

  await sleep(900);
  console.log('  … 900ms elapsed with NO client attached (agent still running)');

  // ---- Phase 2: reconnect from checkpoint, expect gapless replay ----
  const seenB: number[] = [];
  const ticksB: number[] = [];
  let firstReplay: number | undefined;
  let sawPerm = false;
  let sawDone = false;
  let permReqId = '';

  const wsB = await open(h.port, h.token, (f: Frame) => {
    const t = (seq: number, ev: any) => {
      if (firstReplay === undefined) firstReplay = seq;
      take(seenB, ticksB, seq, ev);
      if (ev?.kind === 'permission_request') {
        sawPerm = true;
        permReqId = ev.reqId;
        wsB.send(JSON.stringify({ t: 'permission_response', agentId, reqId: ev.reqId, optionId: 'allow' }));
      }
      if (ev?.kind === 'message_chunk' && /Done/.test(ev.text)) sawDone = true;
    };
    if (f.t === 'snapshot') f.transcript!.forEach((e) => t(e.seq, e.event));
    if (f.t === 'event') t(f.seq!, f.event);
  });
  wsB.send(JSON.stringify({ t: 'subscribe', agentId, sinceSeq: lastSeq }));
  await sleep(500);
  console.log(`  ▸ client B  subscribe@${lastSeq}  → ${seenB.length} events, first replayed seq=${firstReplay}`);

  // ---- approval round-trip over the reconnected socket ----
  wsB.send(JSON.stringify({ t: 'prompt', agentId, text: 'reinstall deps and run tests' }));
  await sleep(900);
  const pidAfter = session.agentPid;

  // ---- assertions ----
  const merged = [...seenA, ...seenB.filter((s) => s > lastSeq)].sort((a, b) => a - b);
  const gapless = merged.every((s, i) => i === 0 || s === merged[i - 1] + 1);
  const seamOk = firstReplay === lastSeq + 1;
  const uniqTicks = [...new Set([...ticksA, ...ticksB])].sort((a, b) => a - b);
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

  const pass = report(checks);
  wsB.close();
  await h.stop();
  process.exit(pass ? 0 : 1);
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
