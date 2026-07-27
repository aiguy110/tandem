import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { useStore } from '../store';
import type { ResumableSession, SessionSearchResult } from '../wire';
import { mergeResumeResults, ResumePalette } from './ResumePalette';

function session(overrides: Partial<ResumableSession> = {}): ResumableSession {
  return {
    sessionId: 'session-1',
    source: 'history',
    agent: 'pi',
    cwd: '/repo',
    title: 'Deployment session',
    resumable: true,
    ...overrides,
  };
}

function result(value: ResumableSession, text: string, hitCount = 1): SessionSearchResult {
  return {
    session: value,
    score: -1,
    hits: Array.from({ length: hitCount }, (_, index) => ({
      entryId: `entry-${index}`,
      role: index ? 'assistant' : 'user',
      match: { text: `${text} ${index}`, highlights: [{ start: 0, end: Array.from(text).length }] },
    })),
  };
}

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

describe('ResumePalette search', () => {
  it('merges metadata and grouped history results once per session', () => {
    const metadata = session();
    const transcriptOnly = session({ sessionId: 'session-2', title: 'Unrelated title' });
    const merged = mergeResumeResults(
      'deployment',
      [metadata],
      [result(metadata, 'deployment', 2), result(transcriptOnly, 'deployment')],
    );
    expect(merged.map((item) => item.session.sessionId)).toEqual(['session-1', 'session-2']);
    expect(merged[0].hits).toHaveLength(2);
  });

  it('rejects stale out-of-order search responses', async () => {
    vi.useFakeTimers();
    const resolvers = new Map<string, (value: SessionSearchResult[]) => void>();
    const searchSessions = vi.fn((query: string) => new Promise<SessionSearchResult[]>((resolve) => resolvers.set(query, resolve)));
    useStore.setState({
      resumeCatalog: { sessions: [], adapters: [] },
      resumeLoading: false,
      searchSessions,
      resumeSession: vi.fn(),
      setModal: vi.fn(),
    });
    const view = render(<ResumePalette />);
    const input = screen.getByPlaceholderText('Resume a session…');

    fireEvent.change(input, { target: { value: 'old' } });
    await act(async () => vi.advanceTimersByTime(125));
    fireEvent.change(input, { target: { value: 'new' } });
    await act(async () => vi.advanceTimersByTime(125));

    await act(async () => resolvers.get('new')!([result(session({ sessionId: 'new' }), 'new context')]));
    expect(view.container.textContent).toContain('new context');
    await act(async () => resolvers.get('old')!([result(session({ sessionId: 'old' }), 'old context')]));
    expect(view.container.textContent).toContain('new context');
    expect(view.container.textContent).not.toContain('old context');
  });

  it('does not resume a history-only row', () => {
    const disabled = session({ resumable: false, historyOnly: true, resumeError: 'No resume adapter is configured.' });
    const resumeSession = vi.fn();
    useStore.setState({
      resumeCatalog: { sessions: [disabled], adapters: [] },
      resumeLoading: false,
      searchSessions: vi.fn(),
      resumeSession,
      setModal: vi.fn(),
    });
    const view = render(<ResumePalette />);
    fireEvent.keyDown(view.container.querySelector('.modal')!, { key: 'Enter' });
    expect(resumeSession).not.toHaveBeenCalled();
    expect(view.container.textContent).toContain('No resume adapter is configured.');
  });

  it('Enter resumes the grouped session rather than an individual hit', async () => {
    vi.useFakeTimers();
    const target = session();
    const resumeSession = vi.fn().mockResolvedValue({});
    useStore.setState({
      resumeCatalog: { sessions: [], adapters: [] },
      resumeLoading: false,
      searchSessions: vi.fn().mockResolvedValue([result(target, 'needle', 2)]),
      resumeSession,
      setModal: vi.fn(),
    });
    const view = render(<ResumePalette />);
    fireEvent.change(screen.getByPlaceholderText('Resume a session…'), { target: { value: 'needle' } });
    await act(async () => {
      vi.advanceTimersByTime(125);
      await Promise.resolve();
    });
    fireEvent.keyDown(view.container.querySelector('.modal')!, { key: 'Enter' });
    expect(resumeSession).toHaveBeenCalledTimes(1);
    expect(resumeSession).toHaveBeenCalledWith(target);
  });
});
