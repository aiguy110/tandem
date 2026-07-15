// WorkspaceManager (D8, docs/spawn-and-workspaces.md): turns a SpawnSpec's
// `workspace` into a real host directory.
//
//   kind:'worktree'  → `git worktree add` a fresh checkout under
//                       <worktreesDir>/<repo-basename>/<agent-name>/ on a new
//                       branch `tandem/<agent-name>` (or spec.branch), based
//                       on spec.baseRef (default: the repo's current HEAD).
//   kind:'existing'  → use the directory as-is (validated to exist).
//
// Teardown removes the worktree checkout but NEVER deletes the branch — work
// is never lost, and a later respawn with the same branch name re-checks it
// out (see `provisionWorktree`'s `exists && explicit` path) rather than
// erroring or silently creating a second branch.
//
// All git access goes through `execFile` (no shell interpolation of
// user-controlled strings — repo paths, branch names, refs all arrive as argv
// elements, never concatenated into a command string).

import { execFile } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import type { Config } from './config.ts';
import type { RepoInfo, SpawnSpec, Workspace } from './types.ts';

// A structured workspace error. `code` is a stable machine-readable token; by
// convention it's also prefixed onto `message` (`"<code>: <detail>"`) so it
// survives the WS protocol's plain-string `ack.error` today without a wire
// change — the UI can `error.split(':')[0]` to branch on it, and the doc'd
// codes below are the intended action:
//   no_such_dir      → the target directory doesn't exist
//   dir_occupied     → a live agent already owns that exact directory
//   dirty_worktree   → uncommitted changes block the close (force to override)
//   worktree_exists  → the computed worktree path is already on disk
export class WorkspaceError extends Error {
  constructor(readonly code: string, detail: string) {
    super(`${code}: ${detail}`);
    this.name = 'WorkspaceError';
  }
}

export interface ProvisionResult {
  cwd: string;
  workspace: Workspace; // resolved: repo is the abs repo root; branch/baseRef filled in
}

function runGit(cwd: string, args: string[]): Promise<string> {
  return new Promise((resolve, reject) => {
    execFile('git', args, { cwd, maxBuffer: 16 * 1024 * 1024 }, (err, stdout, stderr) => {
      if (err) {
        reject(new Error(`git ${args.join(' ')} (cwd=${cwd}) failed: ${(stderr || err.message).trim()}`));
        return;
      }
      resolve(stdout.trim());
    });
  });
}

export class WorkspaceManager {
  constructor(private config: Config) {}

  /** New spawn: turn spec.workspace into a real, ready-to-use cwd. */
  async provision(spec: SpawnSpec, agentName: string, occupantOfDir: (dir: string) => string | undefined): Promise<ProvisionResult> {
    if (spec.workspace.kind === 'existing') return this.provisionExisting(spec.workspace, occupantOfDir);
    return this.provisionWorktree(spec.workspace, agentName);
  }

  private async provisionExisting(ws: Extract<Workspace, { kind: 'existing' }>, occupantOfDir: (dir: string) => string | undefined): Promise<ProvisionResult> {
    const cwd = path.resolve(ws.cwd);
    if (!fs.existsSync(cwd) || !fs.statSync(cwd).isDirectory()) {
      throw new WorkspaceError('no_such_dir', `directory does not exist: ${cwd}`);
    }
    const occupant = occupantOfDir(cwd);
    if (occupant) {
      throw new WorkspaceError('dir_occupied', `${cwd} is already in use by agent "${occupant}" — open a worktree instead, or attach to the existing agent`);
    }
    return { cwd, workspace: { kind: 'existing', cwd } };
  }

  private async provisionWorktree(ws: Extract<Workspace, { kind: 'worktree' }>, agentName: string): Promise<ProvisionResult> {
    const repoRoot = await runGit(ws.repo, ['rev-parse', '--show-toplevel']);
    const repoBasename = path.basename(repoRoot);
    const worktreeDir = path.join(this.config.worktreesDir, repoBasename, agentName);
    if (fs.existsSync(worktreeDir)) {
      throw new WorkspaceError('worktree_exists', `worktree path already exists: ${worktreeDir}`);
    }
    fs.mkdirSync(path.dirname(worktreeDir), { recursive: true });

    const baseRef = ws.baseRef || (await runGit(repoRoot, ['rev-parse', 'HEAD']));
    const explicit = !!ws.branch;
    let branch = ws.branch || `tandem/${agentName}`;
    const exists = await this.branchExists(repoRoot, branch);

    if (exists && explicit) {
      // Respawn / reuse path: the caller named an existing tandem/<name>
      // branch (e.g. re-spawning after a clean close) — re-checkout rather
      // than error or silently create a duplicate.
      await runGit(repoRoot, ['worktree', 'add', worktreeDir, branch]);
    } else if (exists && !explicit) {
      // Auto-derived name collided with an unrelated existing branch — suffix
      // rather than fail the spawn outright.
      let i = 2;
      let candidate = `${branch}-${i}`;
      while (await this.branchExists(repoRoot, candidate)) candidate = `${branch}-${++i}`;
      branch = candidate;
      await runGit(repoRoot, ['worktree', 'add', '-b', branch, worktreeDir, baseRef]);
    } else {
      await runGit(repoRoot, ['worktree', 'add', '-b', branch, worktreeDir, baseRef]);
    }

    return { cwd: worktreeDir, workspace: { kind: 'worktree', repo: repoRoot, branch, baseRef } };
  }

  private async branchExists(repoRoot: string, branch: string): Promise<boolean> {
    try {
      await runGit(repoRoot, ['show-ref', '--verify', '--quiet', `refs/heads/${branch}`]);
      return true;
    } catch {
      return false;
    }
  }

  /** Uncommitted-changes check, used by teardown and exposed for status. */
  async isDirty(cwd: string): Promise<boolean> {
    const out = await runGit(cwd, ['status', '--porcelain']);
    return out.length > 0;
  }

  /**
   * Rail worktree-state indicator: 'dirty' (uncommitted changes) beats
   * 'unmerged' (this branch's HEAD isn't reachable from the source repo's
   * *current* checked-out tip) beats 'synced'. Deliberately checks against
   * the repo's live HEAD rather than the frozen fork-point `baseRef` — once
   * someone merges the branch back into the repo's checked-out branch
   * (elsewhere, out of band), this must flip to 'synced' even though HEAD
   * has long since diverged from that stale fork point. `kind:'existing'`
   * dirs have no separate branch to reconcile, so they're only ever
   * 'dirty'/'synced'.
   */
  async gitState(cwd: string, workspace: Workspace): Promise<'dirty' | 'unmerged' | 'synced'> {
    if (await this.isDirty(cwd)) return 'dirty';
    if (workspace.kind !== 'worktree') return 'synced';
    try {
      const repoHead = await runGit(workspace.repo, ['rev-parse', 'HEAD']);
      await runGit(cwd, ['merge-base', '--is-ancestor', 'HEAD', repoHead]);
      return 'synced';
    } catch {
      return 'unmerged';
    }
  }

  /** Exact details shown before closing an agent with work that may be lost. */
  async closePreview(cwd: string, workspace: Workspace): Promise<{ kind: 'worktree' | 'existing'; uncommitted: string; unmerged: string }> {
    const uncommitted = await runGit(cwd, ['status', '--short']);
    let unmerged = '';
    if (workspace.kind === 'worktree') {
      const repoHead = await runGit(workspace.repo, ['rev-parse', 'HEAD']);
      unmerged = await runGit(cwd, ['log', '--oneline', `${repoHead}..HEAD`]);
    }
    return { kind: workspace.kind, uncommitted, unmerged };
  }

  /**
   * Teardown (docs D8): worktree case removes the checkout but keeps the
   * branch; blocks on uncommitted changes unless `force`. Existing-dir case
   * is a pure detach (no-op here).
   */
  async teardown(workspace: Workspace, cwd: string, force: boolean): Promise<void> {
    if (workspace.kind === 'existing') return;
    if (!fs.existsSync(cwd)) {
      // Already gone on disk (e.g. manually removed) — clear git's stale
      // registration so future `worktree add` calls don't trip over it.
      await runGit(workspace.repo, ['worktree', 'prune']).catch(() => {});
      return;
    }
    if (!force && (await this.isDirty(cwd))) {
      throw new WorkspaceError('dirty_worktree', `${cwd} has uncommitted changes — close with force:true to override (branch "${workspace.branch}" is kept either way)`);
    }
    const args = ['worktree', 'remove', cwd];
    if (force) args.push('--force');
    await runGit(workspace.repo, args);
  }

  /**
   * Restore/respawn: make sure `cwd` exists and is attached, recreating a
   * missing worktree checkout from its (never-deleted) branch. Existing-dir
   * workspaces can't be recreated — a missing dir there is a hard error.
   */
  async reattach(workspace: Workspace, cwd: string): Promise<string> {
    if (workspace.kind === 'existing') {
      if (!fs.existsSync(cwd)) throw new WorkspaceError('no_such_dir', `directory no longer exists: ${cwd}`);
      return cwd;
    }
    if (fs.existsSync(cwd)) return cwd;
    fs.mkdirSync(path.dirname(cwd), { recursive: true });
    await runGit(workspace.repo, ['worktree', 'prune']).catch(() => {});
    await runGit(workspace.repo, ['worktree', 'add', cwd, workspace.branch!]);
    return cwd;
  }
}

// ---- repo discovery for the spawn palette (docs/spawn-and-workspaces.md) ----

const SKIP_DIRS = new Set(['node_modules', 'dist', 'build', '.next', 'target', 'vendor', '.venv', '__pycache__']);

async function findGitRepos(root: string, maxDepth: number): Promise<string[]> {
  const found: string[] = [];
  async function walk(dir: string, depthLeft: number): Promise<void> {
    let entries: fs.Dirent[];
    try {
      entries = fs.readdirSync(dir, { withFileTypes: true });
    } catch {
      return; // unreadable / doesn't exist — skip quietly
    }
    if (entries.some((e) => e.name === '.git')) {
      found.push(dir);
      return; // don't descend into a repo's internals
    }
    if (depthLeft <= 0) return;
    for (const e of entries) {
      if (!e.isDirectory() || e.name.startsWith('.') || SKIP_DIRS.has(e.name)) continue;
      await walk(path.join(dir, e.name), depthLeft - 1);
    }
  }
  await walk(root, maxDepth);
  return found;
}

/**
 * Scan `roots` (TANDEM_PROJECT_ROOTS) for git repos, up to `depth` directories
 * deep, for the quick-spawn palette. Fast: skips node_modules/.git/etc. and
 * never descends past a repo root once found.
 */
export async function listRepos(roots: string[], depth: number, hasLiveAgentForRepo: (repoPath: string) => boolean): Promise<RepoInfo[]> {
  const out: RepoInfo[] = [];
  const seen = new Set<string>();
  for (const root of roots) {
    const dirs = await findGitRepos(root, depth);
    for (const dir of dirs) {
      const real = path.resolve(dir);
      if (seen.has(real)) continue;
      seen.add(real);
      const [currentBranch, dirty] = await Promise.all([
        runGit(real, ['rev-parse', '--abbrev-ref', 'HEAD']).catch(() => ''),
        runGit(real, ['status', '--porcelain'])
          .then((s) => s.length > 0)
          .catch(() => false),
      ]);
      out.push({ path: real, name: path.basename(real), currentBranch, dirty, hasLiveAgent: hasLiveAgentForRepo(real) });
    }
  }
  return out;
}
