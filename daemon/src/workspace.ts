// WorkspaceManager (D8, docs/spawn-and-workspaces.md): turns a SpawnSpec's
// `workspace` into a real host directory.
//
//   kind:'worktree'  → `git worktree add` a fresh checkout under
//                       <worktreesDir>/<repo-basename>/<agent-name>/ on a new
//                       context-qualified `tandem/<target>/<agent-name>` branch
//                       (or spec.branch), based on an immutable resolved source
//                       commit and linked to an integration target.
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
import type { GitRefInfo, GitRefKind, RepoInfo, SpawnSpec, Workspace, WorkspaceIntegration } from './types.ts';

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
  workspace: Workspace; // resolved repo, branch, source ref/OID, and integration target
  createdBranch: boolean;
}

export interface GitWorkspaceState {
  status: 'dirty' | 'ahead' | 'behind' | 'diverged' | 'merged' | 'synced' | 'target_missing';
  ahead: number;
  behind: number;
  targetRef?: string;
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
    return { cwd, workspace: { kind: 'existing', cwd }, createdBranch: false };
  }

  private async provisionWorktree(ws: Extract<Workspace, { kind: 'worktree' }>, agentName: string): Promise<ProvisionResult> {
    const repoRoot = await runGit(ws.repo, ['rev-parse', '--show-toplevel']);
    const repoBasename = path.basename(repoRoot);
    const worktreeDir = path.join(this.config.worktreesDir, repoBasename, agentName);
    if (fs.existsSync(worktreeDir)) {
      throw new WorkspaceError('worktree_exists', `worktree path already exists: ${worktreeDir}`);
    }
    fs.mkdirSync(path.dirname(worktreeDir), { recursive: true });

    const requestedSource = ws.source?.ref || ws.baseRef || 'HEAD';
    let source = await resolveRef(repoRoot, requestedSource);
    const requestedIntegration = ws.integration?.ref || source.ref;
    const integrationResolved = await resolveRef(repoRoot, requestedIntegration);
    const integration: WorkspaceIntegration = {
      kind: integrationKind(integrationResolved.kind),
      ref: integrationResolved.ref,
    };

    const explicit = !!ws.branch;
    let branch = normalizeLocalBranch(ws.branch || defaultAgentBranch(integration.ref, agentName));
    await validateBranch(repoRoot, branch);
    const exists = await this.branchExists(repoRoot, branch);
    // New clients state intent. Legacy callers retain the old behavior where an
    // explicitly named existing branch means respawn/attach.
    const mode = ws.branchMode ?? (explicit && exists ? 'attach' : 'create');
    const checkedOutAt = await checkedOutPath(repoRoot, `refs/heads/${branch}`);

    if (mode === 'attach') {
      if (!exists) throw new WorkspaceError('branch_missing', `local branch does not exist: ${branch}`);
      if (checkedOutAt) throw new WorkspaceError('branch_checked_out', `branch "${branch}" is already checked out at ${checkedOutAt}`);
      source = await resolveRef(repoRoot, `refs/heads/${branch}`);
      await runGit(repoRoot, ['worktree', 'add', worktreeDir, branch]);
    } else if (exists && explicit) {
      throw new WorkspaceError('branch_exists', `local branch already exists: ${branch} — choose Continue existing branch or a different agent branch`);
    } else if (exists) {
      // Auto-derived name collided with an unrelated existing branch — suffix
      // rather than fail the spawn outright.
      let i = 2;
      let candidate = `${branch}-${i}`;
      while (await this.branchExists(repoRoot, candidate)) candidate = `${branch}-${++i}`;
      branch = candidate;
      await runGit(repoRoot, ['worktree', 'add', '-b', branch, worktreeDir, source.commit]);
    } else {
      await runGit(repoRoot, ['worktree', 'add', '-b', branch, worktreeDir, source.commit]);
    }

    return {
      cwd: worktreeDir,
      createdBranch: mode === 'create',
      workspace: {
        kind: 'worktree',
        repo: repoRoot,
        branch,
        branchMode: mode,
        source: { ref: source.ref, commit: source.commit },
        integration,
        // Retained so older daemon builds can still restore this row.
        baseRef: source.commit,
      },
    };
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
  async gitState(cwd: string, workspace: Workspace): Promise<GitWorkspaceState> {
    const dirty = await this.isDirty(cwd);
    if (workspace.kind !== 'worktree') return { status: dirty ? 'dirty' : 'synced', ahead: 0, behind: 0 };
    const targetRef = await this.targetRef(workspace);
    try {
      await runGit(workspace.repo, ['rev-parse', '--verify', `${targetRef}^{commit}`]);
    } catch {
      return { status: dirty ? 'dirty' : 'target_missing', ahead: 0, behind: 0, targetRef };
    }
    const { ahead, behind } = await aheadBehind(cwd, targetRef, 'HEAD');
    if (dirty) return { status: 'dirty', ahead, behind, targetRef };
    if (ahead > 0 && behind > 0) return { status: 'diverged', ahead, behind, targetRef };
    if (ahead > 0) return { status: 'ahead', ahead, behind, targetRef };
    if (behind > 0) {
      const head = await runGit(cwd, ['rev-parse', 'HEAD']);
      const status = workspace.source?.commit && head !== workspace.source.commit ? 'merged' : 'behind';
      return { status, ahead, behind, targetRef };
    }
    return { status: 'synced', ahead, behind, targetRef };
  }

  /** Exact details shown before closing an agent with work that may be lost. */
  async closePreview(cwd: string, workspace: Workspace): Promise<{ kind: 'worktree' | 'existing'; uncommitted: string; unmerged: string; targetRef?: string; ahead?: number; behind?: number }> {
    const uncommitted = await runGit(cwd, ['status', '--short']);
    let unmerged = '';
    let targetRef: string | undefined;
    let ahead: number | undefined;
    let behind: number | undefined;
    if (workspace.kind === 'worktree') {
      targetRef = await this.targetRef(workspace);
      try {
        ({ ahead, behind } = await aheadBehind(cwd, targetRef, 'HEAD'));
        unmerged = await runGit(cwd, ['log', '--oneline', `${targetRef}..HEAD`]);
      } catch {
        // Surface the missing target in the preview rather than comparing with
        // an unrelated checkout's HEAD.
        unmerged = `[integration target missing: ${targetRef}]`;
      }
    }
    return { kind: workspace.kind, uncommitted, unmerged, targetRef, ahead, behind };
  }

  private async targetRef(workspace: Extract<Workspace, { kind: 'worktree' }>): Promise<string> {
    if (workspace.integration?.ref) return workspace.integration.ref;
    if (workspace.source?.ref) return workspace.source.ref;
    // Legacy rows often persisted baseRef as an OID. Preserve their historical
    // live-HEAD comparison because no integration branch was recorded.
    return runGit(workspace.repo, ['symbolic-ref', '-q', 'HEAD']).catch(() => workspace.baseRef || 'HEAD');
  }

  /** Roll back a workspace whose adapter failed before spawn completed. */
  async rollback(workspace: Workspace, cwd: string, createdBranch: boolean): Promise<void> {
    if (workspace.kind !== 'worktree') return;
    if (fs.existsSync(cwd)) await runGit(workspace.repo, ['worktree', 'remove', '--force', cwd]).catch(() => {});
    await runGit(workspace.repo, ['worktree', 'prune']).catch(() => {});
    if (createdBranch && workspace.branch) await runGit(workspace.repo, ['branch', '-D', workspace.branch]).catch(() => {});
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

interface ResolvedRef { ref: string; commit: string; kind: GitRefKind }

async function resolveRef(repoRoot: string, requested: string): Promise<ResolvedRef> {
  let commit: string;
  try {
    commit = await runGit(repoRoot, ['rev-parse', '--verify', `${requested}^{commit}`]);
  } catch {
    throw new WorkspaceError('ref_not_found', `Git ref does not resolve to a commit: ${requested}`);
  }
  let ref = await runGit(repoRoot, ['rev-parse', '--symbolic-full-name', requested]).catch(() => '');
  if (!ref && requested === 'HEAD') ref = await runGit(repoRoot, ['symbolic-ref', '-q', 'HEAD']).catch(() => '');
  if (!ref) ref = commit;
  return { ref, commit, kind: refKind(ref) };
}

function refKind(ref: string): GitRefKind {
  if (ref.startsWith('refs/heads/')) return 'local-branch';
  if (ref.startsWith('refs/remotes/')) return 'remote-branch';
  if (ref.startsWith('refs/tags/')) return 'tag';
  return 'detached';
}

function integrationKind(kind: GitRefKind): WorkspaceIntegration['kind'] {
  return kind === 'local-branch' || kind === 'remote-branch' ? kind : 'detached';
}

function displayRef(ref: string): string {
  if (ref.startsWith('refs/heads/')) return ref.slice('refs/heads/'.length);
  if (ref.startsWith('refs/remotes/')) return ref.slice('refs/remotes/'.length);
  if (ref.startsWith('refs/tags/')) return ref.slice('refs/tags/'.length);
  return ref;
}

function normalizeLocalBranch(branch: string): string {
  return branch.startsWith('refs/heads/') ? branch.slice('refs/heads/'.length) : branch;
}

function defaultAgentBranch(integrationRef: string, agentName: string): string {
  let context = displayRef(integrationRef).replace(/^[^/]+\//, (prefix) => prefix === 'origin/' ? '' : prefix);
  context = context.replace(/^feature\//, '').replace(/[^A-Za-z0-9._/-]+/g, '-').replace(/\/+|\.+$/g, '-');
  context = context.replace(/^[-./]+|[-./]+$/g, '').slice(0, 64) || 'detached';
  const agent = agentName.replace(/[^A-Za-z0-9._-]+/g, '-').replace(/^[-.]+|[-.]+$/g, '') || 'agent';
  return `tandem/${context}/${agent}`;
}

async function validateBranch(repoRoot: string, branch: string): Promise<void> {
  try {
    await runGit(repoRoot, ['check-ref-format', '--branch', branch]);
  } catch {
    throw new WorkspaceError('invalid_branch', `invalid branch name: ${branch}`);
  }
}

async function worktreeBranches(repoRoot: string): Promise<Map<string, string>> {
  const out = await runGit(repoRoot, ['worktree', 'list', '--porcelain']);
  const result = new Map<string, string>();
  let worktree = '';
  for (const line of out.split('\n')) {
    if (line.startsWith('worktree ')) worktree = line.slice('worktree '.length);
    else if (line.startsWith('branch ') && worktree) result.set(line.slice('branch '.length), worktree);
    else if (!line) worktree = '';
  }
  return result;
}

async function checkedOutPath(repoRoot: string, fullRef: string): Promise<string | undefined> {
  return (await worktreeBranches(repoRoot)).get(fullRef);
}

async function aheadBehind(cwd: string, target: string, branch: string): Promise<{ ahead: number; behind: number }> {
  const raw = await runGit(cwd, ['rev-list', '--left-right', '--count', `${target}...${branch}`]);
  const [behind = 0, ahead = 0] = raw.split(/\s+/).map(Number);
  return { ahead, behind };
}

// ---- repo discovery for the spawn palette (docs/spawn-and-workspaces.md) ----

const SKIP_DIRS = new Set(['node_modules', 'dist', 'build', '.next', 'target', 'vendor', '.venv', '__pycache__']);

/** Branch/ref discovery for the Advanced spawn picker. This is deliberately
 * read-only: refreshing the picker never fetches or mutates the repository. */
export async function listGitRefs(repo: string): Promise<GitRefInfo[]> {
  const repoRoot = await runGit(repo, ['rev-parse', '--show-toplevel']);
  const [raw, currentRef, defaultRemoteRef, checkedOut] = await Promise.all([
    runGit(repoRoot, [
      'for-each-ref',
      '--format=%(refname)%1f%(objectname)%1f%(subject)%1f%(committerdate:iso-strict)%1f%(upstream:short)%00',
      'refs/heads', 'refs/remotes', 'refs/tags',
    ]),
    runGit(repoRoot, ['symbolic-ref', '-q', 'HEAD']).catch(() => ''),
    runGit(repoRoot, ['symbolic-ref', '-q', 'refs/remotes/origin/HEAD']).catch(() => ''),
    worktreeBranches(repoRoot),
  ]);
  const refs = await Promise.all(
    raw.split('\0').map((record) => record.trim()).filter(Boolean).map(async (record): Promise<GitRefInfo | undefined> => {
      const [ref, commit, subject, updatedAt, upstream] = record.split('\x1f');
      if (!ref || !commit) return undefined;
      // origin/HEAD and similar symbolic aliases add noise; the actual target is
      // already present and marked isDefault below.
      const symbolicTarget = await runGit(repoRoot, ['symbolic-ref', '-q', ref]).catch(() => '');
      if (symbolicTarget) return undefined;
      const kind = refKind(ref);
      let ahead: number | undefined;
      let behind: number | undefined;
      if (kind === 'local-branch' && upstream) {
        try {
          ({ ahead, behind } = await aheadBehind(repoRoot, upstream, ref));
        } catch {
          // A stale/missing upstream is still a valid local branch picker row.
        }
      }
      return {
        ref,
        displayName: displayRef(ref),
        kind,
        commit,
        subject: subject || undefined,
        updatedAt: updatedAt || undefined,
        upstream: upstream || undefined,
        ahead,
        behind,
        checkedOutAt: checkedOut.get(ref),
        isCurrent: ref === currentRef,
        isDefault: ref === defaultRemoteRef || (!defaultRemoteRef && ref === currentRef),
      };
    }),
  );
  const rank = (item: GitRefInfo): number => item.isCurrent ? 0 : item.isDefault ? 1 : item.kind === 'local-branch' ? 2 : item.kind === 'remote-branch' ? 3 : 4;
  return refs.filter((item): item is GitRefInfo => !!item).sort((a, b) => rank(a) - rank(b) || a.displayName.localeCompare(b.displayName));
}

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
