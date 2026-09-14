// Ranking and repo-grouping for the Resume palette. Kept out of the component
// so the priority rules are testable without rendering.
//
// Fields are prioritized session name > repo > transcript, but *quality wins
// over field*: a strict field tiering let a junk subsequence hit in a title
// ("Trim And Deduplicate Empty Metadata" matches "tandem") outrank an exact
// repo-name match, which is never what the typist meant. So each candidate is
// scored as (how well it matched, then which field it matched in).

import { fuzzyScore } from './fuzzy';
import type { SessionView } from './store';
import type { ResumableSession, SessionSearchHit, SessionSearchResult } from './wire';

// How well the query matched, best first. Compared before FIELD.
export const QUALITY = { EXACT: 4, PREFIX: 3, WORD: 2, SUBSTRING: 1, FUZZY: 0, NONE: -1 } as const;

// Which field matched, best first. Breaks ties between equal-quality matches,
// so an exactly-named session still beats an exactly-named repo.
export const FIELD = { NAME: 0, REPO: 1, TRANSCRIPT: 2, CONTEXT: 3 } as const;

// A transcript hit is a token-prefix FTS match, so it is a real word match on
// content rather than metadata. Ranking it as SUBSTRING puts it below a
// deliberate name/repo match (exact, prefix, or word) and above an incidental
// subsequence — which is what "transcript hits last" means in practice.
const TRANSCRIPT_QUALITY = QUALITY.SUBSTRING;

const WORD_BOUNDARY = /[\s/_\-.]/;

export interface ResumeEntry {
  session: ResumableSession;
  hits: SessionSearchHit[];
  // Set when a running agent owns this session: Resume focuses it instead of
  // spawning a second agent to resume the same transcript.
  liveAgentId?: string;
  quality: number;
  field: number;
  score: number;
}

export interface ResumeGroup {
  // repoKey is the grouping identity (a repository path where one is known);
  // repo is the label shown on the group header.
  repoKey: string;
  repo: string;
  entries: ResumeEntry[];
}

export function sessionKey(session: ResumableSession): string {
	return `${session.hostId ?? 'local'}\0${session.agent}\0${session.externalSessionId || session.sessionId || session.cwd}`;
}

export function sessionName(session: ResumableSession): string {
	return session.title || session.agentName || session.externalSessionId || 'Untitled session';
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

// Grades a match so a deliberate one outranks an incidental one. Contiguous
// beats scattered, and starting a word beats landing mid-word.
export function matchQuality(query: string, target: string): number {
  if (!query || !target) return QUALITY.NONE;
  const q = query.toLowerCase();
  const t = target.toLowerCase();
  if (t === q) return QUALITY.EXACT;
  if (t.startsWith(q)) return QUALITY.PREFIX;
  const at = t.indexOf(q);
  if (at > 0) return WORD_BOUNDARY.test(t[at - 1]) ? QUALITY.WORD : QUALITY.SUBSTRING;
  return fuzzyScore(query, target) > -Infinity ? QUALITY.FUZZY : QUALITY.NONE;
}

// A running agent may have no resumable-catalog row yet (no ACP session id
// recorded, or the catalog is mid-refresh), so Resume synthesizes one. It is
	// never resumed — live sessionId routes selection to focus instead.
export function sessionFromLiveAgent(agent: SessionView): ResumableSession {
  return {
		externalSessionId: '',
    source: 'tandem',
    agent: agent.agent ?? '',
    cwd: agent.workspace.cwd,
    repo: agent.workspace.repo,
    repoPath: agent.workspace.repoPath,
    title: agent.name,
    sessionId: agent.id,
    agentName: agent.name,
    branch: agent.workspace.branch,
    live: true,
    status: agent.status,
    resumable: true,
    hostId: agent.hostId,
    hostName: agent.hostName,
  };
}

// Incidental identifiers, kept searchable at the lowest field priority so a
// branch or agent name stays findable without competing with real titles.
function contextText(session: ResumableSession): string {
	return [session.agent, session.branch, session.cwd, session.externalSessionId].filter(Boolean).join(' ');
}

// Recency is the final tiebreaker, and the only ordering when the query is
// empty. Live agents have no updatedAt of their own, so they sort as "now".
function recencyOf(entry: ResumeEntry): string {
  if (entry.liveAgentId && !entry.session.updatedAt) return '￿';
  return entry.session.updatedAt ?? '';
}

function compareEntries(a: ResumeEntry, b: ResumeEntry): number {
  if (a.quality !== b.quality) return b.quality - a.quality;
  if (a.field !== b.field) return a.field - b.field;
  if (a.score !== b.score) return b.score - a.score;
  // Active sessions surface above dormant ones at equal relevance.
  if (!!a.liveAgentId !== !!b.liveAgentId) return a.liveAgentId ? -1 : 1;
  const recency = recencyOf(b).localeCompare(recencyOf(a));
  if (recency !== 0) return recency;
  return sessionKey(a.session).localeCompare(sessionKey(b.session));
}

// Picks the best-ranked candidate for one session across its searchable fields.
function rankEntry(entry: ResumeEntry, query: string): boolean {
  const candidates: { quality: number; field: number; score: number }[] = [];
  const consider = (field: number, target: string) => {
    const quality = matchQuality(query, target);
    if (quality !== QUALITY.NONE) candidates.push({ quality, field, score: fuzzyScore(query, target) });
  };
  consider(FIELD.NAME, sessionName(entry.session));
  consider(FIELD.REPO, repoLabelOf(entry.session));
  consider(FIELD.CONTEXT, contextText(entry.session));
  if (entry.hits.length > 0) {
    // The daemon already ranked the transcript results; entry.score holds that
    // ordering, so it is preserved as the within-tier tiebreak.
    candidates.push({ quality: TRANSCRIPT_QUALITY, field: FIELD.TRANSCRIPT, score: entry.score });
  }
  if (candidates.length === 0) return false;
  candidates.sort((a, b) => (b.quality - a.quality) || (a.field - b.field) || (b.score - a.score));
  Object.assign(entry, candidates[0]);
  return true;
}

// Merges the resumable catalog, the running agents, and the daemon's transcript
// search into one repo-grouped, ranked list. `historyResults` arrives in the
// daemon's relevance order, which is preserved within the transcript tier.
export function buildResumeGroups(
  query: string,
  sessions: ResumableSession[],
  liveAgents: SessionView[],
  historyResults: SessionSearchResult[],
): ResumeGroup[] {
  const trimmed = query.trim();
  const byKey = new Map<string, ResumeEntry>();
  const add = (session: ResumableSession) => {
    const key = sessionKey(session);
    const existing = byKey.get(key);
    if (existing) return existing;
    const entry: ResumeEntry = { session, hits: [], quality: QUALITY.EXACT, field: FIELD.NAME, score: 0 };
    byKey.set(key, entry);
    return entry;
  };

  for (const session of sessions) add(session);

  // A running agent is authoritative about its own liveness and workspace, so it
  // overlays the catalog row rather than adding a duplicate.
  const byAgentId = new Map<string, ResumeEntry>();
  for (const entry of byKey.values()) {
    if (entry.session.sessionId) byAgentId.set(entry.session.sessionId, entry);
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
    entry.score = -index; // the daemon's relevance order
  }

  const entries: ResumeEntry[] = [];
  for (const entry of byKey.values()) {
    if (!trimmed) {
      entry.quality = QUALITY.EXACT;
      entry.field = FIELD.NAME;
      entry.score = 0;
      entries.push(entry);
    } else if (rankEntry(entry, trimmed)) {
      entries.push(entry);
    }
  }
  entries.sort(compareEntries);

  const groups = new Map<string, ResumeGroup>();
  for (const entry of entries) {
    const key = repoKeyOf(entry.session);
    let group = groups.get(key);
    if (!group) {
      group = { repoKey: key, repo: repoLabelOf(entry.session), entries: [] };
      groups.set(key, group);
    }
    group.entries.push(entry);
  }
  // Entries are already ranked, so each repo inherits the standing of its best
  // individual match and insertion order is that ranking.
  return [...groups.values()];
}

// The flat, keyboard-navigable order — groups are visual, selection is linear.
export function flattenGroups(groups: ResumeGroup[]): ResumeEntry[] {
  return groups.flatMap((group) => group.entries);
}
