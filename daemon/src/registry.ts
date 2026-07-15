// The multi-agent registry: owns every AgentSession keyed by agentId, persists
// the SpawnSpec + ACP sessionId (D14), and restores non-closed agents on daemon
// start (D11). This is the daemon's source of truth for "which agents exist".

import path from 'node:path';
import { AcpAdapter } from './acpAdapter.ts';
import { PtyAdapter } from './ptyAdapter.ts';
import { AgentSession } from './session.ts';
import { WorkspaceManager, listRepos } from './workspace.ts';
import { buildBrowserMcpServers, type BrowserWiring } from './browser/mcpWiring.ts';
import type { Db } from './db.ts';
import type { Config } from './config.ts';
import type { AgentAdapter, AgentRecord, AgentSummary, McpServerSpec, RepoInfo, SpawnSpec } from './types.ts';

// A short rotating word pool for auto-names: web-1, api-2, db-3, … (docs D9).
const NAME_WORDS = ['web', 'api', 'db', 'cli', 'ui', 'svc', 'job', 'net'];

export class AgentRegistry {
  private sessions = new Map<string, AgentSession>();
  // agentId -> resolved cwd (worktree checkout path, or the raw dir for
  // kind:'existing'). Kept alongside `sessions` for fast collision checks and
  // teardown, and mirrored into AgentRecord.cwd for persistence/restore.
  private cwdByAgent = new Map<string, string>();
  private workspace: WorkspaceManager;
  private counter: number;

  constructor(private db: Db, private config: Config, private browser?: BrowserWiring) {
    // Continue the name counter past whatever the DB already contains so restored
    // + fresh agents never collide. Must include closed agents too: their ids
    // are still valid primary keys in `agents`/`events`, and reissuing one to a
    // new agent would splice the old agent's persisted history onto it.
    this.counter = db.maxAgentSuffix();
    this.workspace = new WorkspaceManager(config);
  }

  // Per-agent MCP server registrations (Phase 5): Playwright MCP pointed at the
  // broker's stable per-agent CDP URL + the Tandem-control MCP. Empty when the
  // browser subsystem is disabled (TANDEM_BROWSER_MCP=off or no wiring).
  private mcpServersFor(agentId: string): McpServerSpec[] {
    if (!this.browser || !this.config.browser.mcpEnabled) return [];
    return buildBrowserMcpServers(this.browser, agentId);
  }

  get(agentId: string): AgentSession | undefined {
    return this.sessions.get(agentId);
  }
  list(): AgentSession[] {
    return [...this.sessions.values()];
  }

  // Compact rail metadata for every live agent (docs/ws-protocol.md list_agents).
  // Derived from the resolved SpawnSpec + live session state, so it reflects the
  // durable declaration even for restored agents whose transcript is replay-only.
  summaries(): AgentSummary[] {
    return [...this.sessions.values()].map((s) => {
      const ws = s.spec.workspace;
      const cwd = this.cwdByAgent.get(s.id) ?? this.defaultCwd(s.spec);
      const workspace =
        ws.kind === 'worktree'
          ? { kind: 'worktree' as const, repo: path.basename(ws.repo), repoPath: ws.repo, branch: ws.branch ?? `tandem/${s.name}`, cwd }
          : { kind: 'existing' as const, repo: path.basename(ws.cwd), repoPath: ws.cwd, branch: '', cwd };
      return { id: s.id, name: s.name, workspace, status: s.status, pendingApprovals: s.pendingApprovals().length };
    });
  }

  // ---- workspace + naming ----
  // Legacy/fallback cwd guess for rows persisted before `AgentRecord.cwd`
  // existed (or if it's somehow empty) — not used on the happy path.
  private defaultCwd(spec: SpawnSpec): string {
    return spec.workspace.kind === 'existing' ? spec.workspace.cwd : spec.workspace.repo;
  }
  // Does any *live* agent already occupy this exact directory? Used for
  // kind:'existing' collision detection (docs D8: "attach instead / open a
  // worktree instead").
  private occupantOfDir(dir: string): string | undefined {
    const target = path.resolve(dir);
    for (const [id, cwd] of this.cwdByAgent) {
      if (path.resolve(cwd) === target) return id;
    }
    return undefined;
  }
  // Does any live agent have a worktree (or existing-dir) tied to this repo?
  // Used by list_dirs' `hasLiveAgent` column.
  private hasLiveAgentForRepo(repoPath: string): boolean {
    const target = path.resolve(repoPath);
    for (const s of this.sessions.values()) {
      const ws = s.spec.workspace;
      if (ws.kind === 'worktree' && path.resolve(ws.repo) === target) return true;
      if (ws.kind === 'existing' && path.resolve(ws.cwd) === target) return true;
    }
    return false;
  }
  /** Repo discovery for the quick-spawn palette (list_dirs). */
  listDirs(): Promise<RepoInfo[]> {
    return listRepos(this.config.projectRoots, this.config.dirScanDepth, (repo) => this.hasLiveAgentForRepo(repo));
  }
  private autoName(): string {
    const n = ++this.counter;
    return `${NAME_WORDS[(n - 1) % NAME_WORDS.length]}-${n}`;
  }
  private uniqueName(desired: string): string {
    let name = desired;
    let i = 2;
    while (this.sessions.has(name)) name = `${desired}-${i++}`;
    return name;
  }

  private makeAdapter(id: string, spec: SpawnSpec): AgentAdapter {
    if (spec.adapter === 'pty') return new PtyAdapter(id);
    const agentName = spec.agent || this.config.acp.default;
    const launch = this.config.acp.override ?? this.config.acp.agents[agentName];
    if (!launch) throw new Error(`unknown agent: ${agentName}`);
    return new AcpAdapter(id, launch);
  }

  // Spawn a brand-new agent. Persists the row, dispatches an optional first
  // prompt, and returns the live session.
  async spawn(spec: SpawnSpec): Promise<AgentSession> {
    const name = this.uniqueName(spec.name || this.autoName());
    const id = name; // names are unique, so they double as the stable key
    // Provision the workspace BEFORE creating the session/adapter: a failed
    // provision (collision, missing dir, git error) must not register a
    // half-spawned agent.
    const { cwd, workspace } = await this.workspace.provision(spec, name, (dir) => this.occupantOfDir(dir));
    const resolvedSpec: SpawnSpec = { ...spec, name, workspace };
    const adapter = this.makeAdapter(id, resolvedSpec);
    const session = new AgentSession(id, name, resolvedSpec, adapter, this.db);

    const rec: AgentRecord = { id, name, spec: resolvedSpec, cwd, acpSessionId: null, status: 'idle', createdAt: Date.now(), closedAt: null };
    this.db.upsertAgent(rec);
    this.wireStatus(session);
    this.sessions.set(id, session);
    this.cwdByAgent.set(id, cwd);

    await session.start({ cwd, mcpServers: this.mcpServersFor(id) });
    // The ACP sessionId is known once session/new resolves — persist it for restore.
    if (session.acpSessionId) this.db.setSessionId(id, session.acpSessionId);

    if (spec.task) void session.prompt(spec.task); // dispatch initial prompt (fire-and-forget turn)
    return session;
  }

  // Restore all non-closed agents from the DB on daemon start (D11). A single
  // agent's failure marks it 'error' and is isolated — the daemon never crashes.
  async restoreAll(): Promise<void> {
    for (const rec of this.db.liveAgents()) {
      try {
        await this.restoreOne(rec);
      } catch (e) {
        console.error(`[restore] agent ${rec.id} failed:`, (e as Error).message);
        this.db.setStatus(rec.id, 'error');
        const s = this.sessions.get(rec.id);
        if (s) s.status = 'error';
      }
    }
  }

  private async restoreOne(rec: AgentRecord): Promise<void> {
    // Reattach the persisted workspace: for a worktree, recreate the checkout
    // from its (never-deleted) branch if it's missing on disk; for an
    // existing-dir workspace, the dir must still be there.
    const cwd = await this.workspace.reattach(rec.spec.workspace, rec.cwd || this.defaultCwd(rec.spec));
    const adapter = this.makeAdapter(rec.id, rec.spec);
    const session = new AgentSession(rec.id, rec.name, rec.spec, adapter, this.db);
    this.wireStatus(session);
    this.sessions.set(rec.id, session);
    this.cwdByAgent.set(rec.id, cwd);
    // Re-spawn the subprocess; resume via session/load when we have a persisted
    // sessionId and the agent supports it, else a fresh session (history stays
    // available to clients from the persisted event log).
    await session.start({ cwd, resumeSessionId: rec.acpSessionId ?? undefined, mcpServers: this.mcpServersFor(rec.id) });
    if (session.acpSessionId && session.acpSessionId !== rec.acpSessionId) this.db.setSessionId(rec.id, session.acpSessionId);
  }

  // Persist status transitions so the registry row and the event log agree.
  private wireStatus(session: AgentSession): void {
    session.onEvent((le) => {
      if (le.event.kind === 'status') this.db.setStatus(session.id, le.event.status);
    });
  }

  // Teardown one agent (docs D8): keep the branch, drop the checkout.
  // Uncommitted changes block the close unless `force` — this throws (a
  // WorkspaceError) rather than returning false, so the caller (server.ts)
  // can surface the structured reason instead of a generic "no such agent".
  async close(agentId: string, force = false): Promise<boolean> {
    const s = this.sessions.get(agentId);
    if (!s) return false;
    const cwd = this.cwdByAgent.get(agentId) ?? this.defaultCwd(s.spec);
    await this.workspace.teardown(s.spec.workspace, cwd, force);
    this.sessions.delete(agentId);
    this.cwdByAgent.delete(agentId);
    // Tear the (lazily-provisioned) browser down with the agent (docs/browser.md
    // Lifecycle: torn down when the agent closes).
    await this.browser?.broker.teardown(agentId).catch(() => {});
    await s.dispose();
    this.db.closeAgent(agentId);
    return true;
  }

  async disposeAll(): Promise<void> {
    // Shutdown (not close): keep the agents live in the DB so restart restores them,
    // but kill their ephemeral browsers (v1 browser state is not persisted).
    await Promise.all(
      [...this.sessions.values()].map(async (s) => {
        await this.browser?.broker.teardown(s.id).catch(() => {});
        await s.dispose().catch(() => {});
      }),
    );
  }
}
