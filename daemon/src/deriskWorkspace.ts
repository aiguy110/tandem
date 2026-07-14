// Workspace de-risk (D8 / Phase 2): proves the WorkspaceManager end-to-end —
//
//   a) spawn_agent(worktree) → worktree exists under worktreesDir, branch
//      tandem/<name> checked out, based on the right ref; the repo's original
//      working tree is untouched.
//   b) two agents on the same repo get independent worktrees (a file written
//      in one doesn't appear in the other).
//   c) close_agent with uncommitted changes is refused with a structured
//      error; force:true removes the worktree but keeps the branch.
//   d) a clean close removes the worktree and keeps the branch.
//   e) daemon restart (real child-process restart, deriskRestart.ts's
//      pattern) restores an agent attached to the same worktree path; if the
//      worktree dir is deleted first, it's recreated from the branch.
//   f) list_dirs returns the temp repo with correct branch/dirty/hasLiveAgent.
//   g) a second kind:'existing' spawn into an already-occupied dir is
//      refused with a structured collision error.
//
// Uses a real temp git repo + a real child-process daemon (like
// deriskRestart.ts) so `git worktree` is exercised for real, never the mock.
// Run: npm run derisk:workspace

import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { execFileSync } from 'node:child_process';
import { spawn, type ChildProcess } from 'node:child_process';
import { WebSocket } from 'ws';
import { rule, report, type Frame } from './testHarness.ts';

const PORT = 7729;
const mockPath = new URL('./mock-acp-agent.mjs', import.meta.url).pathname;
const indexPath = new URL('./index.ts', import.meta.url).pathname;
const tsxBin = new URL('../node_modules/.bin/tsx', import.meta.url).pathname;
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

function git(cwd: string, args: string[]): string {
  return execFileSync('git', args, { cwd, encoding: 'utf8' }).trim();
}

interface Daemon {
  proc: ChildProcess;
  token: string;
  stderr: () => string;
  stop: () => Promise<void>;
}

function startDaemon(home: string, projectRoots: string): Promise<Daemon> {
  return new Promise((resolve, reject) => {
    const proc = spawn(tsxBin, [indexPath], {
      env: {
        ...process.env,
        TANDEM_HOME: home,
        TANDEM_PORT: String(PORT),
        TANDEM_BIND: '127.0.0.1',
        TANDEM_ACP_CMD: JSON.stringify([process.execPath, mockPath]),
        TANDEM_PROJECT_ROOTS: projectRoots,
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

/** Send a message and resolve with the matching ack/dirs frame by corrId. */
function rpc(ws: WebSocket, msg: any): Promise<Frame> {
  return new Promise((resolve) => {
    const corrId = msg.corrId ?? Math.random().toString(36).slice(2);
    const onMsg = (raw: any) => {
      const f = JSON.parse(raw.toString()) as Frame;
      if (f.corrId === corrId && (f.t === 'ack' || f.t === 'dirs')) {
        ws.off('message', onMsg);
        resolve(f);
      }
    };
    ws.on('message', onMsg);
    ws.send(JSON.stringify({ ...msg, corrId }));
  });
}

async function main() {
  console.log('\n' + rule);
  console.log('  TANDEM · workspace de-risk (git worktrees, D8/Phase 2)');
  console.log(rule);

  const home = fs.mkdtempSync(path.join(os.tmpdir(), 'tandem-ws-home-'));
  const projectRoot = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), 'tandem-ws-proj-')));
  const repo = path.join(projectRoot, 'demo-repo');
  fs.mkdirSync(repo);
  git(repo, ['init', '-q', '-b', 'main']);
  git(repo, ['config', 'user.email', 'test@example.com']);
  git(repo, ['config', 'user.name', 'Test']);
  fs.writeFileSync(path.join(repo, 'README.md'), '# demo\n');
  git(repo, ['add', '.']);
  git(repo, ['commit', '-q', '-m', 'init']);
  const originalHead = git(repo, ['rev-parse', 'HEAD']);

  console.log(`\n  ▸ TANDEM_HOME  = ${home}`);
  console.log(`  ▸ project root = ${projectRoot}`);
  console.log(`  ▸ demo repo    = ${repo}`);

  const worktreesDir = path.join(home, 'worktrees');
  const repoBasename = path.basename(repo);
  const pathFor = (agent: string) => path.join(worktreesDir, repoBasename, agent);

  const d1 = await startDaemon(home, projectRoot);
  const ws = await connect(d1.token, () => {});

  // ---- (a) spawn a worktree agent ----
  const spawnA = await rpc(ws, { t: 'spawn_agent', spec: { adapter: 'acp', workspace: { kind: 'worktree', repo }, name: 'wt-a' } });
  const idA = spawnA.agentId;
  const pathA = pathFor('wt-a');
  await sleep(200);
  const worktreeExistsA = fs.existsSync(pathA) && fs.existsSync(path.join(pathA, 'README.md'));
  const branchA = worktreeExistsA ? git(pathA, ['rev-parse', '--abbrev-ref', 'HEAD']) : '';
  const branchedFromHead = worktreeExistsA && git(pathA, ['rev-parse', 'HEAD']) === originalHead;
  const repoUntouched = git(repo, ['status', '--porcelain']) === '' && git(repo, ['rev-parse', 'HEAD']) === originalHead && git(repo, ['rev-parse', '--abbrev-ref', 'HEAD']) === 'main';

  // ---- (b) a second agent on the same repo gets an independent worktree ----
  const spawnB = await rpc(ws, { t: 'spawn_agent', spec: { adapter: 'acp', workspace: { kind: 'worktree', repo }, name: 'wt-b' } });
  const idB = spawnB.agentId;
  const pathB = pathFor('wt-b');
  await sleep(200);
  const worktreeExistsB = fs.existsSync(pathB);
  const independentPaths = !!idA && !!idB && idA !== idB && path.resolve(pathA) !== path.resolve(pathB);
  fs.writeFileSync(path.join(pathA, 'only-in-a.txt'), 'a\n');
  const isolated = fs.existsSync(path.join(pathA, 'only-in-a.txt')) && !fs.existsSync(path.join(pathB, 'only-in-a.txt'));

  // ---- (f) list_dirs, while wt-a/wt-b are still live so hasLiveAgent=true ----
  const dirsFrame = await rpc(ws, { t: 'list_dirs' });
  const found = (dirsFrame.dirs ?? []).find((d: any) => path.resolve(d.path) === path.resolve(repo));
  const listDirsOk = !!found && found.name === repoBasename && found.currentBranch === 'main' && found.dirty === false && found.hasLiveAgent === true;

  // ---- (g) collision: a second kind:'existing' spawn into an occupied dir ----
  const spawnE1 = await rpc(ws, { t: 'spawn_agent', spec: { adapter: 'acp', workspace: { kind: 'existing', cwd: repo }, name: 'exist-1' } });
  await sleep(150);
  const spawnE2 = await rpc(ws, { t: 'spawn_agent', spec: { adapter: 'acp', workspace: { kind: 'existing', cwd: repo }, name: 'exist-2' } });
  const collisionRefused = !spawnE2.agentId && !!spawnE2.error && spawnE2.error.includes('dir_occupied');
  // clean up the existing-dir agent so it doesn't interfere with the rest
  if (spawnE1.agentId) await rpc(ws, { t: 'close_agent', agentId: spawnE1.agentId });

  // ---- (c) close with uncommitted changes → refused; force → removed, branch kept ----
  // pathA already has an uncommitted file (only-in-a.txt) from step (b).
  const closeDirtyNoForce = await rpc(ws, { t: 'close_agent', agentId: idA });
  await sleep(100);
  const dirtyRefused = !!closeDirtyNoForce.error && closeDirtyNoForce.error.includes('dirty_worktree') && fs.existsSync(pathA);

  const closeDirtyForced = await rpc(ws, { t: 'close_agent', agentId: idA, force: true });
  await sleep(150);
  const forcedRemoved = !closeDirtyForced.error && !fs.existsSync(pathA);
  const branchAKeptAfterForce = git(repo, ['branch', '--list', 'tandem/wt-a']).includes('tandem/wt-a');

  // ---- (d) clean close (wt-b has no changes) → removed, branch kept ----
  const closeClean = await rpc(ws, { t: 'close_agent', agentId: idB });
  await sleep(150);
  const cleanRemoved = !closeClean.error && !fs.existsSync(pathB);
  const branchBKeptAfterClean = git(repo, ['branch', '--list', 'tandem/wt-b']).includes('tandem/wt-b');

  // ---- (e) restart: spawn wt-c, delete its worktree dir, restart, expect recreation ----
  const spawnC = await rpc(ws, { t: 'spawn_agent', spec: { adapter: 'acp', workspace: { kind: 'worktree', repo }, name: 'wt-c' } });
  const pathC = pathFor('wt-c');
  await sleep(200);
  const cExistedBeforeRestart = fs.existsSync(pathC);
  fs.rmSync(pathC, { recursive: true, force: true }); // simulate the checkout vanishing
  ws.close();
  await d1.stop();
  console.log('  ✖ daemon #1 stopped cleanly (SIGTERM)');

  const d2 = await startDaemon(home, projectRoot);
  await sleep(500); // give restore time to recreate the missing worktree
  const recreatedAfterRestart = fs.existsSync(pathC) && fs.existsSync(path.join(pathC, 'README.md'));
  const branchCAfterRestart = recreatedAfterRestart ? git(pathC, ['rev-parse', '--abbrev-ref', 'HEAD']) : '';

  await d2.stop();

  // ---- results ----
  const checks: [string, boolean, string][] = [
    ['(a) worktree provisioned under worktreesDir on tandem/<name>', worktreeExistsA && branchA === 'tandem/wt-a', `path=${pathA} branch=${branchA}`],
    ['(a) worktree based on the repo\'s current HEAD', branchedFromHead, `pathA HEAD == original HEAD (${originalHead.slice(0, 8)})`],
    ["(a) repo's original working tree untouched", repoUntouched, `repo status clean, still on main @ ${originalHead.slice(0, 8)}`],
    ['(b) two agents on one repo get independent worktree paths', independentPaths && worktreeExistsB, `A=${pathA} B=${pathB}`],
    ['(b) a file in one worktree is invisible in the other', isolated, 'only-in-a.txt present in A, absent in B'],
    ['(c) close with uncommitted changes refused (dirty_worktree)', dirtyRefused, `error="${closeDirtyNoForce.error}"`],
    ['(c) force:true removes the worktree, keeps the branch', forcedRemoved && branchAKeptAfterForce, `removed=${forcedRemoved} branchKept=${branchAKeptAfterForce}`],
    ['(d) clean close removes the worktree, keeps the branch', cleanRemoved && branchBKeptAfterClean, `removed=${cleanRemoved} branchKept=${branchBKeptAfterClean}`],
    ['(e) worktree dir recreated from branch after restart', recreatedAfterRestart && branchCAfterRestart === 'tandem/wt-c', `existedPreRestart=${cExistedBeforeRestart} recreated=${recreatedAfterRestart} branch=${branchCAfterRestart}`],
    ['(f) list_dirs finds the repo with branch/dirty/hasLiveAgent', listDirsOk, JSON.stringify(found)],
    ['(g) collision on kind:existing refused', collisionRefused, `error="${spawnE2.error}"`],
  ];

  const pass = report(checks);
  fs.rmSync(home, { recursive: true, force: true });
  fs.rmSync(projectRoot, { recursive: true, force: true });
  process.exit(pass ? 0 : 1);
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
