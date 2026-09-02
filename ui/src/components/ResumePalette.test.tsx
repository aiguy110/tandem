import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeAll, describe, expect, it, vi } from 'vitest';
import { useStore } from '../store';
import type { AgentView } from '../store';
import type { ResumableSession, SessionSearchResult } from '../wire';
import { ResumePalette } from './ResumePalette';

const PLACEHOLDER = 'Resume a session — name, repo, or transcript…';

function session(overrides: Partial<ResumableSession> = {}): ResumableSession {
  return {
    sessionId: 'session-1',
    source: 'history',
    agent: 'pi',
    cwd: '/repo',
    repo: 'repo',
    repoPath: '/repo',
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
    workspace: { kind: 'worktree', repo: 'repo', repoPath: '/repo', branch: 'feature', cwd: '/wt/feature' },
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

function setup(state: Partial<ReturnType<typeof useStore.getState>>) {
  useStore.setState({
    resumeCatalog: { sessions: [], adapters: [] },
    resumeLoading: false,
    agents: {},
    order: [],
    searchSessions: vi.fn().mockResolvedValue([]),
    resumeSession: vi.fn().mockResolvedValue({}),
    focus: vi.fn(),
    setPane: vi.fn(),
    setModal: vi.fn(),
    ...state,
  });
}

// jsdom does not implement scrollIntoView, which the palette calls to keep the
// keyboard selection visible across long grouped lists.
beforeAll(() => {
  Element.prototype.scrollIntoView = vi.fn();
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

describe('ResumePalette', () => {
  it('groups rows under a repository heading', () => {
    setup({
      resumeCatalog: {
        sessions: [
          session(),
          session({ sessionId: 'other', repo: 'outpost', repoPath: '/outpost', updatedAt: '2020-01-01T00:00:00Z' }),
        ],
        adapters: [],
      },
    });
    const view = render(<ResumePalette />);
    const headings = [...view.container.querySelectorAll('.resume-repo')].map((node) => node.textContent);
    expect(headings).toEqual(['repo', 'outpost']);
  });

  it('badges an active session and focuses it instead of resuming', () => {
    const focus = vi.fn();
    const resumeSession = vi.fn();
    const setPane = vi.fn();
    const setModal = vi.fn();
    setup({
      resumeCatalog: { sessions: [session({ sessionId: 'owned', agentId: 'agent-1', source: 'tandem' })], adapters: [] },
      agents: { 'agent-1': agent() },
      order: ['agent-1'],
      focus,
      resumeSession,
      setPane,
      setModal,
    });
    const view = render(<ResumePalette />);
    expect(view.container.querySelector('.resume-badge')?.textContent).toBe('active');
    fireEvent.keyDown(view.container.querySelector('.modal')!, { key: 'Enter' });
    expect(focus).toHaveBeenCalledWith('agent-1');
    expect(setPane).toHaveBeenCalledWith('chat');
    expect(setModal).toHaveBeenCalledWith('none');
    expect(resumeSession).not.toHaveBeenCalled();
  });

  it('resumes a closed session', async () => {
    const target = session();
    const resumeSession = vi.fn().mockResolvedValue({});
    setup({ resumeCatalog: { sessions: [target], adapters: [] }, resumeSession });
    const view = render(<ResumePalette />);
    expect(view.container.querySelector('.resume-badge')).toBeNull();
    await act(async () => {
      fireEvent.keyDown(view.container.querySelector('.modal')!, { key: 'Enter' });
    });
    expect(resumeSession).toHaveBeenCalledWith(target);
  });

  it('rejects stale out-of-order search responses', async () => {
    vi.useFakeTimers();
    const resolvers = new Map<string, (value: SessionSearchResult[]) => void>();
    setup({
      searchSessions: vi.fn((query: string) => new Promise<SessionSearchResult[]>((resolve) => resolvers.set(query, resolve))),
    });
    const view = render(<ResumePalette />);
    const input = screen.getByPlaceholderText(PLACEHOLDER);

    fireEvent.change(input, { target: { value: 'old' } });
    await act(async () => vi.advanceTimersByTime(125));
    fireEvent.change(input, { target: { value: 'new' } });
    await act(async () => vi.advanceTimersByTime(125));

    await act(async () => resolvers.get('new')!([result(session({ sessionId: 'new', title: 'zzz' }), 'new context')]));
    expect(view.container.textContent).toContain('new context');
    await act(async () => resolvers.get('old')!([result(session({ sessionId: 'old', title: 'zzz' }), 'old context')]));
    expect(view.container.textContent).toContain('new context');
    expect(view.container.textContent).not.toContain('old context');
  });

  it('does not resume a history-only row', () => {
    const resumeSession = vi.fn();
    setup({
      resumeCatalog: {
        sessions: [session({ resumable: false, historyOnly: true, resumeError: 'No resume adapter is configured.' })],
        adapters: [],
      },
      resumeSession,
    });
    const view = render(<ResumePalette />);
    fireEvent.keyDown(view.container.querySelector('.modal')!, { key: 'Enter' });
    expect(resumeSession).not.toHaveBeenCalled();
    expect(view.container.textContent).toContain('No resume adapter is configured.');
  });

  it('Enter opens the grouped session rather than an individual hit', async () => {
    vi.useFakeTimers();
    const target = session({ title: 'zzz' });
    const resumeSession = vi.fn().mockResolvedValue({});
    setup({ searchSessions: vi.fn().mockResolvedValue([result(target, 'needle', 2)]), resumeSession });
    const view = render(<ResumePalette />);
    fireEvent.change(screen.getByPlaceholderText(PLACEHOLDER), { target: { value: 'needle' } });
    await act(async () => {
      vi.advanceTimersByTime(125);
      await Promise.resolve();
    });
    fireEvent.keyDown(view.container.querySelector('.modal')!, { key: 'Enter' });
    expect(resumeSession).toHaveBeenCalledTimes(1);
    expect(resumeSession).toHaveBeenCalledWith(target);
  });

  it('arrow keys walk rows across repository groups', () => {
    setup({
      resumeCatalog: {
        sessions: [session({ sessionId: 'a' }), session({ sessionId: 'b', repo: 'outpost', repoPath: '/outpost' })],
        adapters: [],
      },
    });
    const view = render(<ResumePalette />);
    const modal = view.container.querySelector('.modal')!;
    expect(view.container.querySelectorAll('.resume-row')[0].className).toContain('sel');
    fireEvent.keyDown(modal, { key: 'ArrowDown' });
    expect(view.container.querySelectorAll('.resume-row')[1].className).toContain('sel');
  });
});
