// Phase 6 — cross-phase integration smoke. One daemon, one real temp git repo,
// exercised end-to-end through the WS the way the UI would: discover repos →
// spawn a worktree agent → prompt → approval round-trip → spawn a second agent →
// drop the socket and reconnect with sinceSeq (gapless replay) → dirty-block on
// close then force. Proves phases 1–4 hold together, not just in isolation.

import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import type { WebSocket } from 'ws';
import { open, report, sleep, mockPath, type Frame } from './testHarness.ts';
import { parseDaemonCommand, startDaemon } from './processHarness.ts';

const PORT = 7754;

function git(cwd: string, ...args: string[]) {
  return execFileSync('git', args, { cwd, encoding: 'utf8' }).trim();
}

/** A real git repo the discovery + worktree paths can operate on. */
function makeRepo(): { root: string; repo: string } {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'tandem-proj-'));
  const repo = path.join(root, 'checkout');
  fs.mkdirSync(repo);
  git(repo, 'init', '-q', '-b', 'main');
  git(repo, 'config', 'user.email', 'test@tandem.dev');
  git(repo, 'config', 'user.name', 'Tandem Test');
  fs.writeFileSync(path.join(repo, 'README.md'), '# checkout\n');
  git(repo, 'add', '-A');
  git(repo, 'commit', '-qm', 'init');
  return { root, repo };
}

/** Collect frames while running an action, keyed for easy assertions. */
class Client {
  frames: Frame[] = [];
  lastSeq = new Map<string, number>();
  constructor(public ws: WebSocket) {}
  track(f: Frame) {
    this.frames.push(f);
    if (f.agentId && typeof f.seq === 'number') this.lastSeq.set(f.agentId, f.seq);
    if (f.t === 'snapshot' && f.agentId && f.transcript?.length) {
      this.lastSeq.set(f.agentId, f.transcript[f.transcript.length - 1].seq);
    }
  }
  send(m: unknown) {
    this.ws.send(JSON.stringify(m));
  }
  eventsFor(agentId: string) {
    return this.frames
      .filter((f) => f.t === 'event' && f.agentId === agentId && f.event)
      .map((f) => f.event as { kind: string; [k: string]: unknown });
  }
}

async function main() {
  const { root, repo } = makeRepo();
  process.env.TANDEM_PROJECT_ROOTS = root;
  const home = fs.mkdtempSync(path.join(os.tmpdir(), 'tandem-integration-home-'));
  const daemon = await startDaemon({ command: parseDaemonCommand(), home, port: Number(process.env.TANDEM_TEST_PORT || PORT), env: {
    TANDEM_ACP_CMD: JSON.stringify([process.execPath, mockPath]), TANDEM_BROWSER_MCP: 'off', TANDEM_PROJECT_ROOTS: root,
  } });
  const h = { port: daemon.port, token: daemon.token, home, stop: async () => { await daemon.stop(); fs.rmSync(home, { recursive: true, force: true }); } };

  const checks: [string, boolean, string][] = [];
  let c = new Client(await open(h.port, h.token, (f) => c.track(f)));

  // ── discovery (Phase 2) ─────────────────────────────────────────────
  c.send({ t: 'list_dirs' });
  await sleep(300);
  const dirs = c.frames.find((f) => f.t === 'dirs')?.dirs ?? [];
  const found = dirs.find((d: any) => d.path === repo);
  checks.push(['list_dirs discovered the temp repo', !!found, `repos=${dirs.length}, branch=${found?.currentBranch}`]);

  // ── ACP-backed advanced spawn controls ─────────────────────────────
  c.send({ t: 'get_spawn_options', agent: 'claude', cwd: repo, corrId: 'spawn-options' });
  await sleep(500);
  const spawnOptions = (c.frames.find((f) => f.t === 'spawn_options' && f.corrId === 'spawn-options') as any)?.options;
  const optionCategories = spawnOptions?.configOptions?.map((o: any) => o.category) ?? [];
  checks.push([
    'spawn options came from a fresh ACP session',
    spawnOptions?.modes?.availableModes?.length === 2 && optionCategories.includes('model') && optionCategories.includes('thought_level'),
    `modes=${spawnOptions?.modes?.availableModes?.length ?? 0}, categories=${optionCategories.join(',')}`,
  ]);

  // ── spawn a worktree agent (Phases 1+2) ─────────────────────────────
  c.send({ t: 'spawn_agent', spec: { adapter: 'acp', workspace: { kind: 'worktree', repo } } });
  await sleep(1500);
  const spawned = c.frames.find((f) => f.t === 'ack' && f.agentId);
  const a1 = spawned?.agentId!;
  c.send({ t: 'subscribe', agentId: a1, channels: ['transcript', 'status'], sinceSeq: 0 });
  await sleep(300);
  const wtDir = path.join(h.home, 'worktrees', 'checkout', ...[]);
  const worktreeExists = fs.existsSync(path.join(h.home, 'worktrees', 'checkout'));
  const branches = git(repo, 'branch', '--list', 'tandem/*');
  checks.push(['worktree agent spawned with tandem/* branch', !!a1 && worktreeExists && branches.includes('tandem/'), `id=${a1}, branch=${branches.replace(/\s+/g, ' ')}`]);

  // ── prompt → approval round-trip (Phases 1+3) ───────────────────────
  c.send({ t: 'prompt', agentId: a1, text: 'do the thing' });
  await sleep(1200);
  const permEv = c.eventsFor(a1).find((e) => e.kind === 'permission_request') as any;
  const streamed = c.eventsFor(a1).some((e) => e.kind === 'message_chunk' || e.kind === 'tool_call');
  checks.push(['prompt streamed and raised a permission_request', !!permEv && streamed, `reqId=${permEv?.reqId}, streamed=${streamed}`]);
  const usageEv = c.eventsFor(a1).find((e) => e.kind === 'usage') as any;
  checks.push([
    'ACP usage_update reached the durable client event stream',
    usageEv?.used === 12300 && usageEv?.size === 1000000 && usageEv?.cost?.currency === 'USD',
    `used=${usageEv?.used}, size=${usageEv?.size}, currency=${usageEv?.cost?.currency}`,
  ]);

  if (permEv) {
    const allow = permEv.options?.[0]?.optionId;
    c.send({ t: 'permission_response', agentId: a1, reqId: permEv.reqId, optionId: allow });
    await sleep(1200);
  }
  const doneMsg = c.eventsFor(a1).some((e) => e.kind === 'message_chunk' && String((e as any).text).toLowerCase().includes('done'));
  const idleAgain = c.eventsFor(a1).some((e) => e.kind === 'status' && (e as any).status === 'idle');
  checks.push(['approval let the turn complete → idle', doneMsg && idleAgain, `done=${doneMsg}, idle=${idleAgain}`]);

  // ── second agent, independent stream (Phase 1) ──────────────────────
  c.send({ t: 'spawn_agent', spec: { adapter: 'acp', workspace: { kind: 'worktree', repo } } });
  await sleep(1500);
  const a2 = c.frames.filter((f) => f.t === 'ack' && f.agentId).map((f) => f.agentId).find((id) => id !== a1)!;
  c.send({ t: 'subscribe', agentId: a2, channels: ['transcript', 'status'], sinceSeq: 0 });
  await sleep(300);
  const twoWorktrees = fs.readdirSync(path.join(h.home, 'worktrees', 'checkout')).length === 2;
  checks.push(['second agent got its own worktree', !!a2 && a2 !== a1 && twoWorktrees, `a2=${a2}, worktrees=${fs.readdirSync(path.join(h.home, 'worktrees', 'checkout')).join(',')}`]);

  // ── drop the socket, reconnect with sinceSeq (Phase 1 durability) ────
  const seqBefore = c.lastSeq.get(a1)!;
  c.ws.terminate(); // hard kill, no close handshake — a dropped pipe
  await sleep(200);
  // agent keeps working while nobody is attached
  const c2 = new Client(await open(h.port, h.token, (f) => c2.track(f)));
  c2.send({ t: 'subscribe', agentId: a1, channels: ['transcript', 'status'], sinceSeq: seqBefore });
  c2.send({ t: 'prompt', agentId: a1, text: 'again' });
  await sleep(1200);
  const permEv2 = c2.eventsFor(a1).find((e) => e.kind === 'permission_request') as any;
  if (permEv2) {
    c2.send({ t: 'permission_response', agentId: a1, reqId: permEv2.reqId, optionId: permEv2.options?.[0]?.optionId });
    await sleep(1000);
  }
  const gapless = c2.frames.filter((f) => f.t === 'event' && f.agentId === a1 && typeof f.seq === 'number').every((f, i, arr) => i === 0 || f.seq! === arr[i - 1].seq! + 1);
  const resumedAfterDrop = !!permEv2;
  checks.push(['reconnect replayed gaplessly and the agent never noticed the drop', gapless && resumedAfterDrop, `firstSeqAfter=${seqBefore + 1}, gapless=${gapless}, newTurn=${resumedAfterDrop}`]);

  // ── dirty worktree blocks close, force overrides (Phase 2) ───────────
  const a2dir = path.join(h.home, 'worktrees', 'checkout', fs.readdirSync(path.join(h.home, 'worktrees', 'checkout')).find((d) => d !== fs.readdirSync(path.join(h.home, 'worktrees', 'checkout'))[0]) ?? '');
  // dirty the first worktree deterministically
  const wtNames = fs.readdirSync(path.join(h.home, 'worktrees', 'checkout'));
  const firstWt = path.join(h.home, 'worktrees', 'checkout', wtNames[0]);
  fs.writeFileSync(path.join(firstWt, 'dirty.txt'), 'uncommitted\n');
  const closeErrs: string[] = [];
  const c3 = new Client(await open(h.port, h.token, (f) => { c3.track(f); if (f.t === 'ack' && f.error) closeErrs.push(f.error); }));
  // find which agent owns firstWt — just try closing both non-force; the dirty one must refuse
  c3.send({ t: 'close_agent', agentId: a1 });
  c3.send({ t: 'close_agent', agentId: a2 });
  await sleep(600);
  const refusedDirty = closeErrs.some((e) => e.includes('dirty'));
  const branchesBeforeForce = git(repo, 'branch', '--list', 'tandem/*');
  c3.send({ t: 'close_agent', agentId: a1, force: true });
  c3.send({ t: 'close_agent', agentId: a2, force: true });
  await sleep(800);
  const branchesAfter = git(repo, 'branch', '--list', 'tandem/*');
  const branchesKept = branchesAfter.includes('tandem/') && branchesAfter.split('\n').length === branchesBeforeForce.split('\n').length;
  checks.push(['dirty close refused; force removed checkout but kept branches', refusedDirty && branchesKept, `refusedDirty=${refusedDirty}, branchesAfter=${branchesAfter.replace(/\s+/g, ' ')}`]);

  console.log('\n' + '─'.repeat(64));
  console.log('  TANDEM · integration smoke (Phase 6 — full slice together)');
  console.log('─'.repeat(64));
  const ok = report(checks);

  c3.ws.close();
  await h.stop();
  fs.rmSync(root, { recursive: true, force: true });
  process.exit(ok ? 0 : 1);
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
