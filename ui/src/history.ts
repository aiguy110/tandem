// Ranking and repo-grouping for the History palette. Kept out of the component
// so the priority rules — session name, then repo, then transcript — are
// testable without rendering.

import { fuzzyScore } from './fuzzy';
import type { AgentView } from './store';
import type { ResumableSession, SessionSearchHit, SessionSearchResult } from './wire';

// Lower tiers win outright, regardless of score. The first three are the
// requested priority order; CONTEXT is a floor for the incidental fields
// (agent, branch, working directory, session id) that used to be searchable so
// they stay findable without outranking a transcript hit.
export const TIER = { NAME: 0, REPO: 1, TRANSCRIPT: 2, CONTEXT: 3 } as const;

export interface HistoryEntry {
  session: ResumableSession;
  hits: SessionSearchHit[];
  // Set when a running agent owns this session: History focuses it instead of
  // spawning a second agent to resume the same transcript.
  liveAgentId?: string;
  tier: number;
  score: number;
}

export interface HistoryGroup {
  // repoKey is the grouping identity (a repository path where one is known);
  // repo is the label shown on the group header.
  repoKey: string;
  repo: string;
  entries: HistoryEntry[];
}

export function sessionKey(session: ResumableSession): string {
  return `${session.agent}\0${session.sessionId || session.agentId || session.cwd}`;
}

export function sessionName(session: ResumableSession): string {
  return session.title || session.agentName || session.sessionId || 'Untitled session';
}

function repoKeyOf(session: ResumableSession): string {
  return session.repoPath || session.repo || session.cwd || 'unknown';
}

function repoLabelOf(session: ResumableSession): string {
  return session.repo || lastSegment(session.repoPath) || lastSegment(session.cwd) || 'Unknown repository';
}

function lastSegment(path?: string): string {
  if (!path) return '';
  const parts = path.split('/').filter(Boolean);
  return parts.length ? parts[parts.length - 1] : path;
}

// A running agent may have no resumable-catalog row yet (no ACP session id
// recorded, or the catalog is mid-refresh), so History synthesizes one. It is
// never resumed — liveAgentId routes selection to focus instead.
export function sessionFromLiveAgent(agent: AgentView): ResumableSession {
  return {
    sessionId: '',
    source: 'tandem',
    agent: agent.agent ?? '',
    cwd: agent.workspace.cwd,
    repo: agent.workspace.repo,
    repoPath: agent.workspace.repoPath,
    title: agent.name,
    agentId: agent.id,
    agentName: agent.name,
    branch: agent.workspace.branch,
    live: true,
    status: agent.status,
    resumable: true,
  };
}

function contextText(session: ResumableSession): string {
  return [session.agent, session.branch, session.cwd, session.sessionId].filter(Boolean).join(' ');
}

// Recency is the tiebreaker inside a tier, and the only ordering when the query
// is empty. Live agents have no updatedAt of their own, so they sort as "now".
function recencyOf(entry: HistoryEntry): string {
  if (entry.liveAgentId && !entry.session.updatedAt) return '￿';
  return entry.session.updatedAt ?? '';
}

function compareEntries(a: HistoryEntry, b: HistoryEntry): number {
  if (a.tier !== b.tier) return a.tier - b.tier;
  if (a.score !== b.score) return b.score - a.score;
  // Active sessions surface above dormant ones at equal relevance.
  if (!!a.liveAgentId !== !!b.liveAgentId) return a.liveAgentId ? -1 : 1;
  const recency = recencyOf(b).localeCompare(recencyOf(a));
  if (recency !== 0) return recency;
  return sessionKey(a.session).localeCompare(sessionKey(b.session));
}

// Merges the resumable catalog, the running agents, and the daemon's transcript
// search into one repo-grouped, priority-ranked list. `historyResults` arrives
// in the daemon's relevance order, which is preserved within the transcript tier.
export function buildHistoryGroups(
  query: string,
  sessions: ResumableSession[],
  liveAgents: AgentView[],
  historyResults: SessionSearchResult[],
): HistoryGroup[] {
  const trimmed = query.trim();
  const byKey = new Map<string, HistoryEntry>();
  const add = (session: ResumableSession) => {
    const key = sessionKey(session);
    const existing = byKey.get(key);
    if (existing) return existing;
    const entry: HistoryEntry = { session, hits: [], tier: TIER.NAME, score: 0 };
    byKey.set(key, entry);
    return entry;
  };

  for (const session of sessions) add(session);

  // A running agent is authoritative about its own liveness and workspace, so it
  // overlays the catalog row rather than adding a duplicate.
  const byAgentId = new Map<string, HistoryEntry>();
  for (const entry of byKey.values()) {
    if (entry.session.agentId) byAgentId.set(entry.session.agentId, entry);
  }
  for (const agent of liveAgents) {
    const existing = byAgentId.get(agent.id);
    const entry = existing ?? add(sessionFromLiveAgent(agent));
    entry.liveAgentId = agent.id;
    entry.session = {
      ...entry.session,
      live: true,
      status: agent.status,
      title: entry.session.title || agent.name,
      agentName: agent.name,
      repo: entry.session.repo || agent.workspace.repo,
      repoPath: entry.session.repoPath || agent.workspace.repoPath,
      branch: entry.session.branch || agent.workspace.branch,
      cwd: entry.session.cwd || agent.workspace.cwd,
    };
  }

  for (const [index, result] of historyResults.entries()) {
    const entry = add(result.session);
    entry.hits = result.hits;
    // Rank within the transcript tier by the daemon's own ordering.
    entry.tier = TIER.TRANSCRIPT;
    entry.score = -index;
  }

  const entries: HistoryEntry[] = [];
  for (const entry of byKey.values()) {
    if (!trimmed) {
      entry.tier = TIER.NAME;
      entry.score = 0;
      entries.push(entry);
      continue;
    }
    const name = fuzzyScore(trimmed, sessionName(entry.session));
    const repo = fuzzyScore(trimmed, repoLabelOf(entry.session));
    const context = fuzzyScore(trimmed, contextText(entry.session));
    // Metadata outranks a transcript hit, so it is applied over any tier the
    // search results already assigned.
    if (name > -Infinity) {
      entry.tier = TIER.NAME;
      entry.score = name;
    } else if (repo > -Infinity) {
      entry.tier = TIER.REPO;
      entry.score = repo;
    } else if (entry.hits.length === 0 && context > -Infinity) {
      entry.tier = TIER.CONTEXT;
      entry.score = context;
    } else if (entry.hits.length === 0) {
      continue; // nothing matched this session at all
    }
    entries.push(entry);
  }
  entries.sort(compareEntries);

  const groups = new Map<string, HistoryGroup>();
  for (const entry of entries) {
    const key = repoKeyOf(entry.session);
    let group = groups.get(key);
    if (!group) {
      group = { repoKey: key, repo: repoLabelOf(entry.session), entries: [] };
      groups.set(key, group);
    }
    group.entries.push(entry);
  }
  // Entries are already ranked, so a group inherits the standing of its best
  // member and insertion order is that ranking.
  return [...groups.values()];
}

// The flat, keyboard-navigable order — groups are visual, selection is linear.
export function flattenGroups(groups: HistoryGroup[]): HistoryEntry[] {
  return groups.flatMap((group) => group.entries);
}
