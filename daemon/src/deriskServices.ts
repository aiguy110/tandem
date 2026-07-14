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
import { makeHarness, open, sleep, rule, report, type Frame, type Harness } from './testHarness.ts';

const PORT = 7731;

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

  const h = await makeHarness(PORT);
  const checks: [string, boolean, string][] = [];

  // ============ Scenario 1: fs round-trip + terminal lifecycle ============
  const a = await h.registry.spawn({ adapter: 'acp', workspace: { kind: 'worktree', repo }, name: 'svc-a' });
  const cwdA = h.db.getAgent(a.id)!.cwd;
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

  const stop1 = await a.prompt('please DERISK_SERVICES now');
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
  checks.push(['(c) exit code observed via wait_for_exit', /TERM_EXIT_0/.test(text1) && stop1 === 'end_turn', `msg has TERM_EXIT_0=${/TERM_EXIT_0/.test(text1)}, stopReason=${stop1}`]);

  // output still retrievable from the TerminalHost AFTER terminal/release
  const afterRelease = a.terminals!.output(termId1);
  const scrollAfter = a.terminals!.scrollback(termId1);
  checks.push(['(c) output retrievable from TerminalHost after release', afterRelease.output.length > 0 && /chunk-a/.test(scrollAfter.output), `acpOut=${JSON.stringify(afterRelease.output)} scroll=${JSON.stringify(scrollAfter.output.slice(0, 30))}`]);

  // ============ Scenario 2: path escape rejected ============
  const stop2 = await a.prompt('now DERISK_ESCAPE please');
  await sleep(100);
  const text2 = msgText(c1.events);
  const escapeFile = path.join(cwdA, '..', 'tandem-escape.txt');
  const escapeRejected = /ESCAPE_REJECTED code=-32602/.test(text2);
  const noEscapeFile = !fs.existsSync(escapeFile);
  checks.push(['(b) path escape rejected (JSON-RPC error) + no file outside workspace', escapeRejected && noEscapeFile && stop2 === 'end_turn', `rejected=${escapeRejected} noFile=${noEscapeFile}`]);

  // ============ Scenario 3: outputByteLimit truncation ============
  await a.prompt('give me DERISK_BIGTERM output');
  await sleep(150);
  const text3 = msgText(c1.events);
  const m = /BIGTERM id=(\S+) acpLen=(\d+) truncated=(\w+)/.exec(text3);
  const bigId = m?.[1];
  const acpLen = m ? +m[2] : -1;
  const acpTruncated = m?.[3] === 'true';
  // daemon scrollback retains far more than the 64-byte ACP view
  const scroll = bigId ? a.terminals!.scrollback(bigId) : { output: '', truncated: false };
  const daemonView = a.terminals!.output(bigId!); // ACP-view helper, same limit
  checks.push(['(d) outputByteLimit: ACP view truncated-from-start, truncated:true', acpTruncated && acpLen <= 64 && daemonView.truncated, `acpLen=${acpLen} (<=64) truncated=${acpTruncated}`]);
  checks.push(['(d) daemon scrollback retains MORE than the ACP-limited view', scroll.output.length > acpLen && scroll.output.length > 64, `scrollLen=${scroll.output.length} vs acpLen=${acpLen}`]);

  // ============ Scenario 4: reconnect mid terminal-run (gapless) ============
  const b = await h.registry.spawn({ adapter: 'acp', workspace: { kind: 'worktree', repo }, name: 'svc-b' });
  const rc1 = await collector(h);
  rc1.ws.send(JSON.stringify({ t: 'subscribe', agentId: b.id, sinceSeq: 0 }));
  await sleep(100);
  void b.prompt('start DERISK_SLOWTERM streaming');
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
  const d = await h.registry.spawn({ adapter: 'acp', workspace: { kind: 'worktree', repo }, name: 'svc-d' });
  const cc = await collector(h);
  cc.ws.send(JSON.stringify({ t: 'subscribe', agentId: d.id }));
  await sleep(100);
  const cancelPromise = d.prompt('run DERISK_CANCEL');
  // wait until the permission request is pending
  for (let i = 0; i < 40 && cc.events.filter((e) => e.kind === 'permission_request').length === 0; i++) await sleep(50);
  const hadPerm = d.pendingApprovals().length > 0;
  d.interrupt(); // mid-turn cancel
  const stopCancel = await cancelPromise;
  await sleep(150);
  const cancelledToolCall = cc.events.some((e) => e.kind === 'tool_call_update' && e.event.status === 'cancelled');
  const permsCleared = d.pendingApprovals().length === 0;
  checks.push(['(f) interrupt → prompt resolves stopReason cancelled', stopCancel === 'cancelled', `stopReason=${stopCancel}`]);
  checks.push(['(f) pending permission auto-resolved cancelled (cleared)', hadPerm && permsCleared, `hadPerm=${hadPerm} clearedAfter=${permsCleared}`]);
  checks.push(['(f) unfinished tool call marked cancelled in transcript', cancelledToolCall, `saw tool_call_update status=cancelled=${cancelledToolCall}`]);

  // follow-up prompt on the SAME session must complete normally
  const stopFollow = await d.prompt('now DERISK_SERVICES again');
  for (let i = 0; i < 60 && !/SERVICES_DONE/.test(msgText(cc.events)); i++) await sleep(50);
  checks.push(['(f) follow-up prompt on same session completes normally', stopFollow === 'end_turn' && /SERVICES_DONE/.test(msgText(cc.events)), `stopReason=${stopFollow} done=${/SERVICES_DONE/.test(msgText(cc.events))}`]);

  console.log(`\n  ▸ TerminalHost backend: ${a.terminals!.usesPty() ? 'node-pty' : 'child_process fallback'}`);

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
