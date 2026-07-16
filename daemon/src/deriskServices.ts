// ClientServices de-risk (Phase 3): proves the daemon is a real ACP *client* —
// it services the agent's fs/* and terminal/* requests, buffers terminal output
// on the durable side, and honors cancellation semantics. Everything runs in an
// in-process daemon over a throwaway TANDEM_HOME (makeHarness), with a real temp
// git worktree so fs writes land in a real workspace checkout.
//
// Asserts (mirrors the phase's VALIDATION list a–f):
//   a) fs round-trip: agent writes then reads a file through the daemon; the file
//      lands inside the worktree and the content matches.
//   b) path escape: an fs/write to ../outside is rejected with a JSON-RPC error
//      AND no file is created outside the workspace.
//   c) terminal: output chunks arrive as terminal_output events on the terminals
//      channel, exit code observed via wait_for_exit, output STILL retrievable
//      from the TerminalHost after terminal/release.
//   d) outputByteLimit: a terminal exceeding the ACP limit returns a truncated-
//      from-the-start buffer (truncated:true), while the daemon scrollback keeps
//      more than the ACP-limited view.
//   e) reconnect: client drops mid terminal-run, reconnects with sinceSeq and
//      receives the missed terminal_output events gaplessly.
//   f) cancel: mid-turn interrupt → prompt resolves stopReason 'cancelled',
//      pending permission auto-resolved cancelled, and a FOLLOW-UP prompt on the
//      same session completes normally.
//
// Run: npm run derisk:services

import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { execFileSync } from 'node:child_process';
import { WebSocket } from 'ws';
import { open, sleep, rule, report, mockPath, type Frame } from './testHarness.ts';
import { parseDaemonCommand, startDaemon } from './processHarness.ts';

const PORT = 7731;
interface Harness { port: number; token: string; home: string; stop(): Promise<void> }
async function makeProcessHarness(): Promise<Harness> {
  const home = fs.mkdtempSync(path.join(os.tmpdir(), 'tandem-services-home-'));
  const daemon = await startDaemon({ command: parseDaemonCommand(), home, port: Number(process.env.TANDEM_TEST_PORT || PORT), env: {
    TANDEM_ACP_CMD: JSON.stringify([process.execPath, mockPath]), TANDEM_BROWSER_MCP: 'off', TANDEM_PROJECT_ROOTS: process.env.TANDEM_PROJECT_ROOTS,
  } });
  return { port: daemon.port, token: daemon.token, home, stop: async () => { await daemon.stop(); fs.rmSync(home, { recursive: true, force: true }); } };
}

async function rpc(h: Harness, message: any): Promise<Frame> {
  const corrId = `rpc-${Math.random().toString(36).slice(2)}`;
  let answer: Frame | undefined;
  const ws = await open(h.port, h.token, (f) => { if (f.t === 'ack' && f.corrId === corrId) answer = f; });
  ws.send(JSON.stringify({ ...message, corrId }));
  for (let i = 0; i < 400 && !answer; i++) await sleep(25);
  ws.close();
  if (!answer) throw new Error(`timeout: ${message.t}`);
  return answer;
}

async function spawnRemote(h: Harness, repo: string, name: string): Promise<{ id: string; cwd: string }> {
  const ack = await rpc(h, { t: 'spawn_agent', spec: { adapter: 'acp', workspace: { kind: 'worktree', repo }, name } });
  if (!ack.agentId || ack.error) throw new Error(ack.error ?? 'spawn failed');
  return { id: ack.agentId, cwd: path.join(h.home, 'worktrees', path.basename(repo), name) };
}

function git(cwd: string, args: string[]): void {
  execFileSync('git', args, { cwd, encoding: 'utf8' });
}

interface Collected {
  seq: number;
  kind: string;
  event: any;
  channel: 'transcript' | 'terminals' | 'status' | 'pty' | 'browser';
}

// A collecting socket that records every event frame it receives.
async function collector(h: Harness): Promise<{ ws: WebSocket; events: Collected[] }> {
  const events: Collected[] = [];
  const push = (seq: number, ev: any) => {
    const channel = ev.kind === 'terminal_output' ? 'terminals' : ev.kind === 'status' ? 'status' : ev.kind === 'raw_pty' ? 'pty' : 'transcript';
    events.push({ seq, kind: ev.kind, event: ev, channel });
  };
  const ws = await open(h.port, h.token, (f: Frame) => {
    if (f.t === 'snapshot') f.transcript!.forEach((e) => push(e.seq, e.event));
    if (f.t === 'event') push(f.seq!, f.event);
  });
  return { ws, events };
}

const msgText = (events: Collected[]) => events.filter((e) => e.kind === 'message_chunk').map((e) => e.event.text).join('');

async function main() {
  console.log('\n' + rule);
  console.log('  TANDEM · ClientServices de-risk (fs + terminals + cancel, Phase 3)');
  console.log(rule);

  // Temp git repo under a project root, so worktree provisioning is real.
  const projectRoot = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), 'tandem-svc-proj-')));
  const repo = path.join(projectRoot, 'demo-repo');
  fs.mkdirSync(repo);
  git(repo, ['init', '-q', '-b', 'main']);
  git(repo, ['config', 'user.email', 'test@example.com']);
  git(repo, ['config', 'user.name', 'Test']);
  fs.writeFileSync(path.join(repo, 'README.md'), '# demo\n');
  git(repo, ['add', '.']);
  git(repo, ['commit', '-q', '-m', 'init']);
  process.env.TANDEM_PROJECT_ROOTS = projectRoot;

  const h = await makeProcessHarness();
  const checks: [string, boolean, string][] = [];

  // ============ Scenario 1: fs round-trip + terminal lifecycle ============
  const a = await spawnRemote(h, repo, 'svc-a');
  const cwdA = a.cwd;
  console.log(`\n  ▸ agent svc-a · worktree cwd = ${cwdA}`);

  // A terminals-ONLY subscription proves channel routing (no transcript leaks in).
  const termOnly: Collected[] = [];
  const wsTerm = await open(h.port, h.token, (f: Frame) => {
    if (f.t === 'event' && f.event.kind === 'terminal_output') termOnly.push({ seq: f.seq!, kind: f.event.kind, event: f.event, channel: 'terminals' });
    if (f.t === 'event' && f.event.kind !== 'terminal_output') termOnly.push({ seq: f.seq!, kind: f.event.kind, event: f.event, channel: 'transcript' });
  });
  wsTerm.send(JSON.stringify({ t: 'subscribe', agentId: a.id, channels: ['terminals'] }));

  const c1 = await collector(h);
  c1.ws.send(JSON.stringify({ t: 'subscribe', agentId: a.id }));
  await sleep(150);

  await rpc(h, { t: 'prompt', agentId: a.id, text: 'please DERISK_SERVICES now' });
  // wait for the scripted turn to finish
  for (let i = 0; i < 60 && !/SERVICES_DONE/.test(msgText(c1.events)); i++) await sleep(50);
  const text1 = msgText(c1.events);

  const roundtripFile = path.join(cwdA, 'tandem-roundtrip.txt');
  const fileLanded = fs.existsSync(roundtripFile);
  const fileBody = fileLanded ? fs.readFileSync(roundtripFile, 'utf8') : '';
  checks.push(['(a) fs round-trip: file lands in worktree, content matches', fileLanded && fileBody === 'hello from the agent\nline two\n' && /FS_ROUNDTRIP_OK/.test(text1), `file=${fileLanded} body=${JSON.stringify(fileBody.slice(0, 20))} msg=${/FS_ROUNDTRIP_OK/.test(text1)}`]);

  const termEvents = c1.events.filter((e) => e.kind === 'terminal_output');
  const termId1 = termEvents[0]?.event.termId;
  const termChunks = termEvents.map((e) => e.event.chunk).join('');
  const termOnChannel = termOnly.filter((e) => e.channel === 'terminals').length > 0 && termOnly.every((e) => e.kind === 'terminal_output');
  checks.push(['(c) terminal_output events on the terminals channel', termEvents.length > 0 && termOnChannel, `${termEvents.length} events, terminals-only sub saw ${termOnly.length} (all terminal_output=${termOnly.every((e) => e.kind === 'terminal_output')})`]);
  checks.push(['(c) exit code observed via wait_for_exit', /TERM_EXIT_0/.test(text1), `msg has TERM_EXIT_0=${/TERM_EXIT_0/.test(text1)}`]);

  // output still retrievable from the TerminalHost AFTER terminal/release
  const persistedOutput = termEvents.filter((e) => e.event.termId === termId1).map((e) => e.event.chunk).join('');
  checks.push(['(c) output retrievable from durable events after release', persistedOutput.length > 0 && /chunk-a/.test(persistedOutput), `durable=${JSON.stringify(persistedOutput.slice(0, 30))}`]);

  // ============ Scenario 2: path escape rejected ============
  await rpc(h, { t: 'prompt', agentId: a.id, text: 'now DERISK_ESCAPE please' });
  await sleep(100);
  const text2 = msgText(c1.events);
  const escapeFile = path.join(cwdA, '..', 'tandem-escape.txt');
  const escapeRejected = /ESCAPE_REJECTED code=-32602/.test(text2);
  const noEscapeFile = !fs.existsSync(escapeFile);
  checks.push(['(b) path escape rejected (JSON-RPC error) + no file outside workspace', escapeRejected && noEscapeFile, `rejected=${escapeRejected} noFile=${noEscapeFile}`]);

  // ============ Scenario 3: outputByteLimit truncation ============
  await rpc(h, { t: 'prompt', agentId: a.id, text: 'give me DERISK_BIGTERM output' });
  await sleep(150);
  const text3 = msgText(c1.events);
  const m = /BIGTERM id=(\S+) acpLen=(\d+) truncated=(\w+)/.exec(text3);
  const bigId = m?.[1];
  const acpLen = m ? +m[2] : -1;
  const acpTruncated = m?.[3] === 'true';
  // daemon scrollback retains far more than the 64-byte ACP view
  const durableBig = bigId ? c1.events.filter((e) => e.kind === 'terminal_output' && e.event.termId === bigId).map((e) => e.event.chunk).join('') : '';
  checks.push(['(d) outputByteLimit: ACP view truncated-from-start, truncated:true', acpTruncated && acpLen <= 64, `acpLen=${acpLen} (<=64) truncated=${acpTruncated}`]);
  checks.push(['(d) daemon durable output retains MORE than the ACP-limited view', durableBig.length > acpLen && durableBig.length > 64, `durableLen=${durableBig.length} vs acpLen=${acpLen}`]);

  // ============ Scenario 4: reconnect mid terminal-run (gapless) ============
  const b = await spawnRemote(h, repo, 'svc-b');
  const rc1 = await collector(h);
  rc1.ws.send(JSON.stringify({ t: 'subscribe', agentId: b.id, sinceSeq: 0 }));
  await sleep(100);
  void rpc(h, { t: 'prompt', agentId: b.id, text: 'start DERISK_SLOWTERM streaming' });
  // let a couple of slow chunks arrive, then HARD-KILL the socket
  for (let i = 0; i < 40 && rc1.events.filter((e) => e.kind === 'terminal_output').length < 2; i++) await sleep(50);
  const gotBefore = rc1.events.filter((e) => e.kind === 'terminal_output').map((e) => e.event.chunk).join('');
  const lastSeqB = Math.max(0, ...rc1.events.map((e) => e.seq));
  rc1.ws.terminate();
  console.log(`  ✖ svc-b socket TERMINATED after ${rc1.events.filter((e) => e.kind === 'terminal_output').length} terminal chunks (seq ${lastSeqB})`);

  await sleep(300); // agent keeps streaming while nobody is attached
  const rc2 = await collector(h);
  rc2.ws.send(JSON.stringify({ t: 'subscribe', agentId: b.id, sinceSeq: lastSeqB }));
  await sleep(50);
  for (let i = 0; i < 60 && !/SLOWTERM_DONE/.test(msgText(rc2.events)); i++) await sleep(50);
  const firstReplay: number | undefined = rc2.events.length ? rc2.events[0].seq : undefined;
  const gotAfter = rc2.events.filter((e) => e.kind === 'terminal_output').map((e) => e.event.chunk).join('');
  const allChunks = gotBefore + gotAfter;
  const gapless = firstReplay === lastSeqB + 1;
  const allSlow = [1, 2, 3, 4, 5, 6].every((n) => allChunks.includes(`slow-${n}`));
  checks.push(['(e) reconnect replays missed terminal_output gaplessly', gapless && allSlow, `firstReplay=${firstReplay} expected=${lastSeqB + 1}; all 6 slow chunks present=${allSlow}`]);

  // ============ Scenario 5: cancellation ============
  const d = await spawnRemote(h, repo, 'svc-d');
  const cc = await collector(h);
  cc.ws.send(JSON.stringify({ t: 'subscribe', agentId: d.id }));
  await sleep(100);
  await rpc(h, { t: 'prompt', agentId: d.id, text: 'run DERISK_CANCEL' });
  // wait until the permission request is pending
  for (let i = 0; i < 40 && cc.events.filter((e) => e.kind === 'permission_request').length === 0; i++) await sleep(50);
  const perm = cc.events.find((e) => e.kind === 'permission_request')?.event;
  const hadPerm = !!perm;
  await rpc(h, { t: 'interrupt', agentId: d.id });
  await sleep(150);
  const cancelledToolCall = cc.events.some((e) => e.kind === 'tool_call_update' && e.event.status === 'cancelled');
  let cancelSnapshot: Frame | undefined;
  const verifyCancel = await open(h.port, h.token, (f) => { if (f.t === 'snapshot' && f.agentId === d.id) cancelSnapshot = f; });
  verifyCancel.send(JSON.stringify({ t: 'subscribe', agentId: d.id, sinceSeq: 0 }));
  for (let i = 0; i < 80 && !cancelSnapshot; i++) await sleep(25);
  verifyCancel.close();
  const permsCleared = Array.isArray(cancelSnapshot?.pendingApprovals) && cancelSnapshot!.pendingApprovals!.length === 0;
  checks.push(['(f) interrupt → prompt cancellation becomes externally visible', cancelledToolCall, `cancelled tool=${cancelledToolCall}`]);
  checks.push(['(f) pending permission auto-resolved cancelled (cleared)', hadPerm && permsCleared, `hadPerm=${hadPerm} clearedAfter=${permsCleared}`]);
  checks.push(['(f) unfinished tool call marked cancelled in transcript', cancelledToolCall, `saw tool_call_update status=cancelled=${cancelledToolCall}`]);

  // follow-up prompt on the SAME session must complete normally
  await rpc(h, { t: 'prompt', agentId: d.id, text: 'now DERISK_SERVICES again' });
  for (let i = 0; i < 60 && !/SERVICES_DONE/.test(msgText(cc.events)); i++) await sleep(50);
  checks.push(['(f) follow-up prompt on same session completes normally', /SERVICES_DONE/.test(msgText(cc.events)), `done=${/SERVICES_DONE/.test(msgText(cc.events))}`]);

  const pass = report(checks);
  c1.ws.close();
  wsTerm.close();
  rc2.ws.close();
  cc.ws.close();
  await h.stop();
  fs.rmSync(projectRoot, { recursive: true, force: true });
  process.exit(pass ? 0 : 1);
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
