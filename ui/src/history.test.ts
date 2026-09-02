import { describe, expect, it } from 'vitest';
import { buildHistoryGroups, flattenGroups, TIER } from './history';
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

describe('buildHistoryGroups grouping', () => {
  it('groups sessions by repository', () => {
    const groups = buildHistoryGroups(
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
    const groups = buildHistoryGroups(
      '',
      [session({ sessionId: 'wt', cwd: '/home/me/.tandem/worktrees/tandem/meitner-336' })],
      [],
      [],
    );
    expect(groups).toHaveLength(1);
    expect(groups[0].repo).toBe('tandem');
  });

  it('falls back to the working directory when no repo was attributed', () => {
    const groups = buildHistoryGroups('', [session({ repo: undefined, repoPath: undefined, cwd: '/tmp/scratch' })], [], []);
    expect(groups[0].repo).toBe('scratch');
    expect(groups[0].repoKey).toBe('/tmp/scratch');
  });
});

describe('buildHistoryGroups ranking', () => {
  it('ranks a session-name match above a repo match above a transcript match', () => {
    const named = session({ sessionId: 'named', title: 'refactor the parser', repo: 'alpha', repoPath: '/alpha' });
    const repoMatch = session({ sessionId: 'repo', title: 'unrelated', repo: 'refactor-tools', repoPath: '/refactor-tools' });
    const transcript = session({ sessionId: 'transcript', title: 'unrelated', repo: 'beta', repoPath: '/beta' });
    const entries = flattenGroups(
      buildHistoryGroups('refactor', [named, repoMatch, transcript], [], [result(transcript, 'refactor')]),
    );
    expect(entries.map((entry) => entry.session.sessionId)).toEqual(['named', 'repo', 'transcript']);
    expect(entries.map((entry) => entry.tier)).toEqual([TIER.NAME, TIER.REPO, TIER.TRANSCRIPT]);
  });

  it('keeps a name match at the top tier even when it also has transcript hits', () => {
    const both = session({ sessionId: 'both', title: 'deployment notes' });
    const [entry] = flattenGroups(buildHistoryGroups('deployment', [both], [], [result(both, 'deployment', 2)]));
    expect(entry.tier).toBe(TIER.NAME);
    expect(entry.hits).toHaveLength(2);
  });

  it('preserves the daemon relevance order within the transcript tier', () => {
    const first = session({ sessionId: 'first', title: 'aaa' });
    const second = session({ sessionId: 'second', title: 'bbb' });
    const entries = flattenGroups(
      buildHistoryGroups('needle', [], [], [result(first, 'needle'), result(second, 'needle')]),
    );
    expect(entries.map((entry) => entry.session.sessionId)).toEqual(['first', 'second']);
  });

  it('drops sessions that match nothing', () => {
    const entries = flattenGroups(buildHistoryGroups('zzzz', [session()], [], []));
    expect(entries).toHaveLength(0);
  });

  it('still finds a session by branch or agent, but below transcript hits', () => {
    const byBranch = session({ sessionId: 'branch', title: 'unrelated', branch: 'meitner' });
    const byTranscript = session({ sessionId: 'transcript', title: 'unrelated' });
    const entries = flattenGroups(
      buildHistoryGroups('meitner', [byBranch, byTranscript], [], [result(byTranscript, 'meitner')]),
    );
    expect(entries.map((entry) => entry.session.sessionId)).toEqual(['transcript', 'branch']);
    expect(entries[1].tier).toBe(TIER.CONTEXT);
  });

  it('orders by recency, active first, with no query', () => {
    const old = session({ sessionId: 'old', updatedAt: '2020-01-01T00:00:00Z' });
    const recent = session({ sessionId: 'recent', updatedAt: '2026-06-01T00:00:00Z' });
    const entries = flattenGroups(buildHistoryGroups('', [old, recent], [agent()], []));
    expect(entries.map((entry) => entry.session.sessionId || entry.liveAgentId)).toEqual(['agent-1', 'recent', 'old']);
  });
});

describe('buildHistoryGroups active sessions', () => {
  it('marks a catalog session live when a running agent owns it', () => {
    const owned = session({ sessionId: 'owned', agentId: 'agent-1', live: true, source: 'tandem' });
    const entries = flattenGroups(buildHistoryGroups('', [owned], [agent()], []));
    expect(entries).toHaveLength(1);
    expect(entries[0].liveAgentId).toBe('agent-1');
    expect(entries[0].session.status).toBe('working');
  });

  it('synthesizes a row for a running agent the catalog does not list', () => {
    const entries = flattenGroups(buildHistoryGroups('', [], [agent({ id: 'fresh', name: 'Fresh agent' })], []));
    expect(entries).toHaveLength(1);
    expect(entries[0].liveAgentId).toBe('fresh');
    expect(entries[0].session.title).toBe('Fresh agent');
  });

  it('does not duplicate an agent that already has a catalog row', () => {
    const owned = session({ sessionId: 'owned', agentId: 'agent-1' });
    const groups = buildHistoryGroups('', [owned], [agent()], []);
    expect(flattenGroups(groups)).toHaveLength(1);
  });

  it('finds an active session by name like any other', () => {
    const entries = flattenGroups(buildHistoryGroups('fresh', [], [agent({ id: 'fresh', name: 'Fresh agent' })], []));
    expect(entries.map((entry) => entry.liveAgentId)).toEqual(['fresh']);
  });
});
