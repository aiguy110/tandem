import { describe, expect, it } from 'vitest';
import { buildResumeGroups, FIELD, flattenGroups, QUALITY } from './resume';
import type { AgentView } from './store';
import type { ResumableSession, SessionSearchResult } from './wire';

function session(overrides: Partial<ResumableSession> = {}): ResumableSession {
  return {
    sessionId: 'session-1',
    source: 'history',
    agent: 'pi',
    cwd: '/src/tandem',
    repo: 'tandem',
    repoPath: '/src/tandem',
    title: 'Deployment session',
    updatedAt: '2026-01-01T00:00:00Z',
    resumable: true,
    ...overrides,
  };
}

function agent(overrides: Partial<AgentView> = {}): AgentView {
  return {
    id: 'agent-1',
    name: 'Live agent',
    agent: 'claude',
    status: 'working',
    workspace: { kind: 'worktree', repo: 'tandem', repoPath: '/src/tandem', branch: 'feature', cwd: '/wt/feature' },
    ...overrides,
  } as AgentView;
}

function result(value: ResumableSession, text: string, hitCount = 1): SessionSearchResult {
  return {
    session: value,
    score: -1,
    hits: Array.from({ length: hitCount }, (_, index) => ({
      entryId: `${value.sessionId}-entry-${index}`,
      role: index ? 'assistant' : 'user',
      match: { text: `${text} ${index}`, highlights: [{ start: 0, end: Array.from(text).length }] },
    })),
  };
}

describe('buildResumeGroups grouping', () => {
  it('groups sessions by repository', () => {
    const groups = buildResumeGroups(
      '',
      [
        session({ sessionId: 'a' }),
        session({ sessionId: 'b', repo: 'outpost', repoPath: '/src/outpost', cwd: '/src/outpost' }),
        session({ sessionId: 'c' }),
      ],
      [],
      [],
    );
    expect(groups.map((group) => [group.repo, group.entries.length])).toEqual([
      ['tandem', 2],
      ['outpost', 1],
    ]);
  });

  it('groups worktree sessions under their source repo, not their checkout dir', () => {
    const groups = buildResumeGroups(
      '',
      [session({ sessionId: 'wt', cwd: '/home/me/.tandem/worktrees/tandem/meitner-336' })],
      [],
      [],
    );
    expect(groups).toHaveLength(1);
    expect(groups[0].repo).toBe('tandem');
  });

  it('falls back to the working directory when no repo was attributed', () => {
    const groups = buildResumeGroups('', [session({ repo: undefined, repoPath: undefined, cwd: '/tmp/scratch' })], [], []);
    expect(groups[0].repo).toBe('scratch');
    expect(groups[0].repoKey).toBe('/tmp/scratch');
  });
});

describe('buildResumeGroups ranking', () => {
  it('ranks a session-name match above a repo match above a transcript match', () => {
    const named = session({ sessionId: 'named', title: 'refactor the parser', repo: 'alpha', repoPath: '/alpha' });
    const repoMatch = session({ sessionId: 'repo', title: 'unrelated', repo: 'refactor-tools', repoPath: '/refactor-tools' });
    const transcript = session({ sessionId: 'transcript', title: 'unrelated', repo: 'beta', repoPath: '/beta' });
    const entries = flattenGroups(
      buildResumeGroups('refactor', [named, repoMatch, transcript], [], [result(transcript, 'refactor')]),
    );
    expect(entries.map((entry) => entry.session.sessionId)).toEqual(['named', 'repo', 'transcript']);
    expect(entries.map((entry) => entry.field)).toEqual([FIELD.NAME, FIELD.REPO, FIELD.TRANSCRIPT]);
  });

  it('keeps a name match at the top tier even when it also has transcript hits', () => {
    const both = session({ sessionId: 'both', title: 'deployment notes' });
    const [entry] = flattenGroups(buildResumeGroups('deployment', [both], [], [result(both, 'deployment', 2)]));
    expect(entry.field).toBe(FIELD.NAME);
    expect(entry.hits).toHaveLength(2);
  });

  it('preserves the daemon relevance order within the transcript tier', () => {
    const first = session({ sessionId: 'first', title: 'aaa' });
    const second = session({ sessionId: 'second', title: 'bbb' });
    const entries = flattenGroups(
      buildResumeGroups('needle', [], [], [result(first, 'needle'), result(second, 'needle')]),
    );
    expect(entries.map((entry) => entry.session.sessionId)).toEqual(['first', 'second']);
  });

  it('drops sessions that match nothing', () => {
    const entries = flattenGroups(buildResumeGroups('zzzz', [session()], [], []));
    expect(entries).toHaveLength(0);
  });

  it('still finds a session by branch or agent', () => {
    const byBranch = session({ sessionId: 'branch', title: 'unrelated', branch: 'meitner' });
    const [entry] = flattenGroups(buildResumeGroups('meitner', [byBranch], [], []));
    expect(entry.session.sessionId).toBe('branch');
    expect(entry.field).toBe(FIELD.CONTEXT);
  });

  it('ranks an incidental subsequence in a title below a transcript hit', () => {
    // "Trim And Deduplicate Empty Metadata" contains t-a-n-d-e-m as a
    // subsequence; a real transcript hit is the better answer.
    const accidental = session({ sessionId: 'accidental', title: 'Trim And Deduplicate Empty Metadata', repo: 'alpha', repoPath: '/alpha', cwd: '/alpha' });
    const transcript = session({ sessionId: 'transcript', title: 'unrelated', repo: 'beta', repoPath: '/beta', cwd: '/beta' });
    const entries = flattenGroups(
      buildResumeGroups('tandem', [accidental, transcript], [], [result(transcript, 'tandem')]),
    );
    expect(entries.map((entry) => entry.session.sessionId)).toEqual(['transcript', 'accidental']);
    expect(entries[1].quality).toBe(QUALITY.FUZZY);
  });

  it('brings an exact repo match to the top over a junk subsequence in a title', () => {
    // The reported bug: strict field tiering let any title that merely contained
    // the query as a subsequence outrank the repo the user actually named.
    const accidental = session({
      sessionId: 'accidental',
      title: 'Trim And Deduplicate Empty Metadata',
      repo: 'alpha',
      repoPath: '/alpha',
      cwd: '/alpha',
    });
    const exactRepo = session({ sessionId: 'exact', title: 'unrelated', repo: 'tandem', repoPath: '/src/tandem' });
    const groups = buildResumeGroups('tandem', [accidental, exactRepo], [], []);
    expect(groups[0].repo).toBe('tandem');
    expect(groups[0].entries[0].quality).toBe(QUALITY.EXACT);
    expect(flattenGroups(groups).map((entry) => entry.session.sessionId)).toEqual(['exact', 'accidental']);
  });

  it('prefers an exact session name over an exact repo name', () => {
    const named = session({ sessionId: 'named', title: 'tandem', repo: 'alpha', repoPath: '/alpha' });
    const repoMatch = session({ sessionId: 'repo', title: 'unrelated', repo: 'tandem', repoPath: '/tandem' });
    const entries = flattenGroups(buildResumeGroups('tandem', [named, repoMatch], [], []));
    expect(entries.map((entry) => entry.session.sessionId)).toEqual(['named', 'repo']);
  });

  it('sorts a repo group by its best individual match', () => {
    // The "outpost" group leads because one of its sessions matches exactly,
    // even though the other repo holds more (weaker) matches.
    const weakA = session({ sessionId: 'weak-a', title: 'deploy pipeline', repo: 'alpha', repoPath: '/alpha' });
    const weakB = session({ sessionId: 'weak-b', title: 'deploy runner', repo: 'alpha', repoPath: '/alpha' });
    const strong = session({ sessionId: 'strong', title: 'deploy', repo: 'outpost', repoPath: '/outpost' });
    const groups = buildResumeGroups('deploy', [weakA, weakB, strong], [], []);
    expect(groups.map((group) => group.repo)).toEqual(['outpost', 'alpha']);
    expect(groups[1].entries).toHaveLength(2);
  });

  it('prefers a prefix match over a mid-word one', () => {
    const prefix = session({ sessionId: 'prefix', title: 'deploy the daemon', repo: 'alpha', repoPath: '/alpha' });
    const midWord = session({ sessionId: 'mid', title: 'redeploy the daemon', repo: 'beta', repoPath: '/beta' });
    const entries = flattenGroups(buildResumeGroups('deploy', [prefix, midWord], [], []));
    expect(entries.map((entry) => entry.session.sessionId)).toEqual(['prefix', 'mid']);
  });

  it('orders by recency, active first, with no query', () => {
    const old = session({ sessionId: 'old', updatedAt: '2020-01-01T00:00:00Z' });
    const recent = session({ sessionId: 'recent', updatedAt: '2026-06-01T00:00:00Z' });
    const entries = flattenGroups(buildResumeGroups('', [old, recent], [agent()], []));
    expect(entries.map((entry) => entry.session.sessionId || entry.liveAgentId)).toEqual(['agent-1', 'recent', 'old']);
  });
});

describe('buildResumeGroups active sessions', () => {
  it('marks a catalog session live when a running agent owns it', () => {
    const owned = session({ sessionId: 'owned', agentId: 'agent-1', live: true, source: 'tandem' });
    const entries = flattenGroups(buildResumeGroups('', [owned], [agent()], []));
    expect(entries).toHaveLength(1);
    expect(entries[0].liveAgentId).toBe('agent-1');
    expect(entries[0].session.status).toBe('working');
  });

  it('synthesizes a row for a running agent the catalog does not list', () => {
    const entries = flattenGroups(buildResumeGroups('', [], [agent({ id: 'fresh', name: 'Fresh agent' })], []));
    expect(entries).toHaveLength(1);
    expect(entries[0].liveAgentId).toBe('fresh');
    expect(entries[0].session.title).toBe('Fresh agent');
  });

  it('does not duplicate an agent that already has a catalog row', () => {
    const owned = session({ sessionId: 'owned', agentId: 'agent-1' });
    const groups = buildResumeGroups('', [owned], [agent()], []);
    expect(flattenGroups(groups)).toHaveLength(1);
  });

  it('finds an active session by name like any other', () => {
    const entries = flattenGroups(buildResumeGroups('fresh', [], [agent({ id: 'fresh', name: 'Fresh agent' })], []));
    expect(entries.map((entry) => entry.liveAgentId)).toEqual(['fresh']);
  });
});
