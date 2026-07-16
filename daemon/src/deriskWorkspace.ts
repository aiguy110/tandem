// Workspace de-risk (D8 / Phase 2): proves the WorkspaceManager end-to-end —
//
//   a) spawn_agent(worktree) → worktree exists under worktreesDir, a
//      context-qualified tandem/<source>/<name> branch is checked out from the
//      selected immutable source commit, and the original checkout is untouched.
//   b) two agents on the same repo get independent worktrees (a file written
//      in one doesn't appear in the other).
//   c) close_agent with uncommitted changes is refused with a structured
//      error; force:true removes the worktree but keeps the branch.
//   d) a clean close removes the worktree and keeps the branch.
//   e) daemon restart (real child-process restart, deriskRestart.ts's
//      pattern) restores an agent attached to the same worktree path; if the
//      worktree dir is deleted first, it's recreated from the branch.
//   f) list_dirs/list_git_refs return repository and feature-branch context,
//      including integration targets, checkout occupancy, and divergence.
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
import { WebSocket } from 'ws';
import { rule, report, type Frame } from './testHarness.ts';
import { parseDaemonCommand, startDaemon as startDaemonProcess, type RunningDaemon } from './processHarness.ts';

const PORT = Number(process.env.TANDEM_TEST_PORT || 7729);
const mockPath = new URL('./mock-acp-agent.mjs', import.meta.url).pathname;
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

function git(cwd: string, args: string[]): string {
  return execFileSync('git', args, { cwd, encoding: 'utf8' }).trim();
}

type Daemon = RunningDaemon;

function startDaemon(home: string, projectRoots: string): Promise<Daemon> {
  return startDaemonProcess({
      command: parseDaemonCommand(), home, port: PORT,
      env: {
        TANDEM_HOME: home,
        TANDEM_PORT: String(PORT),
        TANDEM_BIND: '127.0.0.1',
        TANDEM_ACP_CMD: JSON.stringify([process.execPath, mockPath]),
        TANDEM_PROJECT_ROOTS: projectRoots,
      },
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
    if (f.corrId === corrId && (f.t === 'ack' || f.t === 'dirs' || f.t === 'git_refs' || f.t === 'agents' || f.t === 'close_preview')) {
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
  git(repo, ['switch', '-q', '-c', 'feature/migration']);
  fs.writeFileSync(path.join(repo, 'FEATURE.md'), 'feature context\n');
  git(repo, ['add', 'FEATURE.md']);
  git(repo, ['commit', '-q', '-m', 'feature base']);
  const featureHead = git(repo, ['rev-parse', 'HEAD']);
  git(repo, ['switch', '-q', 'main']);

  console.log(`\n  ▸ TANDEM_HOME  = ${home}`);
  console.log(`  ▸ project root = ${projectRoot}`);
  console.log(`  ▸ demo repo    = ${repo}`);

  const worktreesDir = path.join(home, 'worktrees');
  const repoBasename = path.basename(repo);
  const pathFor = (agent: string) => path.join(worktreesDir, repoBasename, agent);

  const d1 = await startDaemon(home, projectRoot);
  const ws = await connect(d1.token, () => {});

  // ---- branch discovery: canonical local refs + checked-out metadata ----
  const refsFrame = await rpc(ws, { t: 'list_git_refs', repo });
  const mainRef = (refsFrame.refs ?? []).find((ref: any) => ref.ref === 'refs/heads/main');
  const featureRef = (refsFrame.refs ?? []).find((ref: any) => ref.ref === 'refs/heads/feature/migration');
  const refsOk = mainRef?.isCurrent === true && path.resolve(mainRef.checkedOutAt) === path.resolve(repo)
    && featureRef?.commit === featureHead && featureRef?.kind === 'local-branch';
  const checkedOutAttach = await rpc(ws, {
    t: 'spawn_agent',
    spec: { adapter: 'acp', workspace: { kind: 'worktree', repo, branchMode: 'attach', branch: 'main', source: { ref: 'refs/heads/main' } }, name: 'checked-out' },
  });
  const checkedOutAttachRefused = checkedOutAttach.error?.includes('branch_checked_out') === true;
  const failedStart = await rpc(ws, {
    t: 'spawn_agent',
    spec: {
      adapter: 'acp',
      resolvedLaunch: { agent: 'failing-adapter', acp: { cmd: '/bin/false', args: [] } },
      workspace: { kind: 'worktree', repo, branchMode: 'create', branch: 'tandem/rollback-test', source: { ref: 'refs/heads/main' } },
      name: 'rollback-test',
    },
  });
  const afterFailedStart = await rpc(ws, { t: 'list_agents' });
  const failedStartRolledBack = !!failedStart.error
    && !fs.existsSync(pathFor('rollback-test'))
    && git(repo, ['branch', '--list', 'tandem/rollback-test']) === ''
    && !(afterFailedStart.agents ?? []).some((agent: any) => agent.id === 'rollback-test');

  // ---- (a) spawn a worktree agent ----
  const spawnA = await rpc(ws, { t: 'spawn_agent', spec: { adapter: 'acp', workspace: { kind: 'worktree', repo }, name: 'wt-a' } });
  const idA = spawnA.agentId;
  const pathA = pathFor('wt-a');
  await sleep(200);
  const worktreeExistsA = fs.existsSync(pathA) && fs.existsSync(path.join(pathA, 'README.md'));
  const branchA = worktreeExistsA ? git(pathA, ['rev-parse', '--abbrev-ref', 'HEAD']) : '';
  const branchedFromHead = worktreeExistsA && git(pathA, ['rev-parse', 'HEAD']) === originalHead;
  const repoUntouched = git(repo, ['status', '--porcelain']) === '' && git(repo, ['rev-parse', 'HEAD']) === originalHead && git(repo, ['rev-parse', '--abbrev-ref', 'HEAD']) === 'main';

  // ---- feature context: immutable source commit + integration target ----
  const spawnFeature = await rpc(ws, {
    t: 'spawn_agent',
    spec: {
      adapter: 'acp',
      workspace: {
        kind: 'worktree',
        repo,
        branchMode: 'create',
        source: { ref: 'refs/heads/feature/migration' },
        integration: { kind: 'local-branch', ref: 'refs/heads/feature/migration' },
      },
      name: 'wt-feature',
    },
  });
  const idFeature = spawnFeature.agentId;
  const pathFeature = pathFor('wt-feature');
  await sleep(150);
  const featureBranch = git(pathFeature, ['rev-parse', '--abbrev-ref', 'HEAD']);
  const featureStartsAtSelectedCommit = git(pathFeature, ['rev-parse', 'HEAD']) === featureHead && fs.existsSync(path.join(pathFeature, 'FEATURE.md'));
  fs.writeFileSync(path.join(pathFeature, 'feature-work.txt'), 'agent feature work\n');
  git(pathFeature, ['add', 'feature-work.txt']);
  git(pathFeature, ['commit', '-q', '-m', 'feature agent work']);
  // Advance the integration target independently after the agent forked.
  const targetCheckout = path.join(projectRoot, 'feature-target-checkout');
  git(repo, ['worktree', 'add', '-q', targetCheckout, 'feature/migration']);
  fs.writeFileSync(path.join(targetCheckout, 'TARGET.md'), 'target advanced\n');
  git(targetCheckout, ['add', 'TARGET.md']);
  git(targetCheckout, ['commit', '-q', '-m', 'feature target moved']);
  git(repo, ['worktree', 'remove', targetCheckout]);
  // Move main independently. Feature close/status must not compare against it.
  fs.writeFileSync(path.join(repo, 'MAIN.md'), 'main moved\n');
  git(repo, ['add', 'MAIN.md']);
  git(repo, ['commit', '-q', '-m', 'main moved']);
  const featurePreview = await rpc(ws, { t: 'get_close_preview', agentId: idFeature });
  const featurePreviewOk = featurePreview.preview?.targetRef === 'refs/heads/feature/migration'
    && featurePreview.preview?.ahead === 1
    && featurePreview.preview?.behind === 1
    && featurePreview.preview?.unmerged.includes('feature agent work');
  const featureSummary = await rpc(ws, { t: 'list_agents' });
  const summarizedFeature = (featureSummary.agents ?? []).find((agent: any) => agent.id === idFeature);
  const featureSummaryOk = summarizedFeature?.workspace.targetRef === 'refs/heads/feature/migration'
    && summarizedFeature?.workspace.startCommit === featureHead
    && summarizedFeature?.workspace.ahead === 1
    && summarizedFeature?.workspace.behind === 1
    && summarizedFeature?.workspace.gitState === 'diverged';
  const createCollision = await rpc(ws, {
    t: 'spawn_agent',
    spec: {
      adapter: 'acp',
      workspace: { kind: 'worktree', repo, branchMode: 'create', branch: featureBranch, source: { ref: featureHead } },
      name: 'wt-feature-collision',
    },
  });
  const explicitCreateCollision = createCollision.error?.includes('branch_exists') === true;
  await rpc(ws, { t: 'close_agent', agentId: idFeature });
  const retainedRefs = await rpc(ws, { t: 'list_git_refs', repo });
  const retainedFeatureRef = (retainedRefs.refs ?? []).find((ref: any) => ref.ref === `refs/heads/${featureBranch}`);
  const retainedContextDiscoverable = retainedFeatureRef?.tandem?.closed === true
    && retainedFeatureRef?.tandem?.integrationRef === 'refs/heads/feature/migration';
  const attachFeature = await rpc(ws, {
    t: 'spawn_agent',
    spec: {
      adapter: 'acp',
      workspace: {
        kind: 'worktree', repo, branchMode: 'attach', branch: featureBranch,
        source: { ref: 'refs/heads/feature/migration' },
        integration: { kind: 'local-branch', ref: 'refs/heads/feature/migration' },
      },
      name: 'wt-feature-attach',
    },
  });
  const attachPath = pathFor('wt-feature-attach');
  await sleep(150);
  const explicitAttachWorks = !!attachFeature.agentId && git(attachPath, ['rev-parse', '--abbrev-ref', 'HEAD']) === featureBranch;
  if (attachFeature.agentId) await rpc(ws, { t: 'close_agent', agentId: attachFeature.agentId });

  // ---- (b) a second agent on the same repo gets an independent worktree ----
  const spawnB = await rpc(ws, { t: 'spawn_agent', spec: { adapter: 'acp', workspace: { kind: 'worktree', repo }, name: 'wt-b' } });
  const idB = spawnB.agentId;
  const pathB = pathFor('wt-b');
  await sleep(200);
  const worktreeExistsB = fs.existsSync(pathB);
  const independentPaths = !!idA && !!idB && idA !== idB && path.resolve(pathA) !== path.resolve(pathB);
  fs.writeFileSync(path.join(pathA, 'only-in-a.txt'), 'a\n');
  const isolated = fs.existsSync(path.join(pathA, 'only-in-a.txt')) && !fs.existsSync(path.join(pathB, 'only-in-a.txt'));
  git(pathA, ['add', 'only-in-a.txt']);
  git(pathA, ['commit', '-m', 'agent work']);
  fs.appendFileSync(path.join(pathA, 'only-in-a.txt'), 'uncommitted\n');
  const closePreview = await rpc(ws, { t: 'get_close_preview', agentId: idA });
  const previewOk = closePreview.preview?.uncommitted.includes('only-in-a.txt') && closePreview.preview?.unmerged.includes('agent work');

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
  const branchAKeptAfterForce = git(repo, ['branch', '--list', 'tandem/main/wt-a']).includes('tandem/main/wt-a');

  // ---- (d) clean close (wt-b has no changes) → removed, branch kept ----
  const closeClean = await rpc(ws, { t: 'close_agent', agentId: idB });
  await sleep(150);
  const cleanRemoved = !closeClean.error && !fs.existsSync(pathB);
  const branchBKeptAfterClean = git(repo, ['branch', '--list', 'tandem/main/wt-b']).includes('tandem/main/wt-b');

  // ---- (d2) confirmed close can keep the checkout, even when dirty ----
  const spawnKeep = await rpc(ws, { t: 'spawn_agent', spec: { adapter: 'acp', workspace: { kind: 'worktree', repo }, name: 'wt-keep' } });
  const pathKeep = pathFor('wt-keep');
  await sleep(150);
  fs.writeFileSync(path.join(pathKeep, 'keep-me.txt'), 'still here\n');
  const closeKeep = await rpc(ws, { t: 'close_agent', agentId: spawnKeep.agentId, deleteWorktree: false });
  await sleep(100);
  const checkoutKept = !closeKeep.error && fs.existsSync(path.join(pathKeep, 'keep-me.txt'));
  git(repo, ['worktree', 'remove', '--force', pathKeep]);

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
    ['branch picker discovery returns canonical refs + checkout metadata', refsOk, `main=${JSON.stringify(mainRef)} feature=${JSON.stringify(featureRef)}`],
    ['explicit attach refuses a branch already checked out elsewhere', checkedOutAttachRefused, `error="${checkedOutAttach.error}"`],
    ['adapter startup failure rolls back DB row, worktree, and new branch', failedStartRolledBack, `error="${failedStart.error}"`],
    ['(a) worktree provisioned on a context-qualified tandem branch', worktreeExistsA && branchA === 'tandem/main/wt-a', `path=${pathA} branch=${branchA}`],
    ['(a) worktree based on the repo\'s current HEAD', branchedFromHead, `pathA HEAD == original HEAD (${originalHead.slice(0, 8)})`],
    ["(a) repo's original working tree untouched", repoUntouched, `repo status clean, still on main @ ${originalHead.slice(0, 8)}`],
    ['(b) two agents on one repo get independent worktree paths', independentPaths && worktreeExistsB, `A=${pathA} B=${pathB}`],
    ['(b) a file in one worktree is invisible in the other', isolated, 'only-in-a.txt present in A, absent in B'],
    ['(b) close preview includes uncommitted status and one-line unmerged commits', !!previewOk, JSON.stringify(closePreview.preview)],
    ['feature spawn starts at selected immutable commit on a context-qualified branch', featureBranch === 'tandem/migration/wt-feature' && featureStartsAtSelectedCommit, `branch=${featureBranch} start=${featureHead.slice(0, 8)}`],
    ['feature close preview compares with integration target, not source checkout HEAD', featurePreviewOk, JSON.stringify(featurePreview.preview)],
    ['agent summary exposes target, start commit, and ahead count', featureSummaryOk, JSON.stringify(summarizedFeature?.workspace)],
    ['explicit create refuses an existing branch instead of silently attaching', explicitCreateCollision, `error="${createCollision.error}"`],
    ['retained Tandem branch advertises its original integration context', retainedContextDiscoverable, JSON.stringify(retainedFeatureRef?.tandem)],
    ['explicit attach checks out a retained existing agent branch', explicitAttachWorks, `branch=${featureBranch} path=${attachPath}`],
    ['(c) close with uncommitted changes refused (dirty_worktree)', dirtyRefused, `error="${closeDirtyNoForce.error}"`],
    ['(c) force:true removes the worktree, keeps the branch', forcedRemoved && branchAKeptAfterForce, `removed=${forcedRemoved} branchKept=${branchAKeptAfterForce}`],
    ['(d) clean close removes the worktree, keeps the branch', cleanRemoved && branchBKeptAfterClean, `removed=${cleanRemoved} branchKept=${branchBKeptAfterClean}`],
    ['(d) close with deleteWorktree:false preserves a dirty checkout', checkoutKept, `path=${pathKeep}`],
    ['(e) worktree dir recreated from branch after restart', recreatedAfterRestart && branchCAfterRestart === 'tandem/main/wt-c', `existedPreRestart=${cExistedBeforeRestart} recreated=${recreatedAfterRestart} branch=${branchCAfterRestart}`],
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
