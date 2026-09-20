import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { LOCAL_HOST_ID, useStore } from '../store';
import type { SessionView } from '../store';
import { SessionsRail } from './AgentsRail';

afterEach(() => {
  cleanup();
  useStore.setState({ sessions: {}, order: [], focusedId: null });
});

function session(): SessionView {
  return {
    id: 'fed~worker~agent-1',
    name: 'Remote agent',
    hostId: 'worker',
    hostName: 'Worker',
    workspace: {
      kind: 'worktree', repo: 'repo', repoPath: '/missing/repo', branch: 'agent-1', cwd: '/managed/agent-1',
    },
    status: 'idle',
    events: [],
    lastSeq: 0,
    pendingApprovals: [],
    turnNotifications: [],
    hasPty: false,
    shellExited: false,
    shellExitMessage: null,
    browserActive: false,
    browserOwner: 'agent',
    browserTakeoverHeld: false,
    takeovers: [],
    sessionConfig: null,
    controlMode: 'transcript',
    adapter: 'acp',
    canHandoff: true,
  } as unknown as SessionView;
}

function touchPointer(type: string, pointerId: number, clientX: number, clientY: number) {
  // jsdom's PointerEvent shim does not retain pointerType, whereas browsers do.
  const event = new MouseEvent(type, { bubbles: true, clientX, clientY });
  Object.defineProperties(event, {
    pointerId: { value: pointerId },
    pointerType: { value: 'touch' },
  });
  return event;
}

describe('SessionsRail close recovery', () => {
  it('offers a forced retry when a clean preview belongs to an orphaned worktree', async () => {
    const closeAgent = vi.fn()
      .mockResolvedValueOnce({ error: 'orphaned_worktree: repository no longer exists; close with force:true' })
      .mockResolvedValueOnce({});
    const agent = session();
    useStore.setState({
      sessions: { [agent.id]: agent },
      order: [agent.id],
      focusedId: agent.id,
      hosts: [
        { id: LOCAL_HOST_ID, name: 'This host', status: 'connected', local: true },
        { id: 'worker', name: 'Worker', status: 'connected' },
      ],
      getClosePreview: vi.fn().mockResolvedValue({ kind: 'worktree' }),
      closeAgent,
    });

    const view = render(<SessionsRail />);
    fireEvent.contextMenu(view.container.querySelector('.session-row')!);
    fireEvent.click(await screen.findByRole('menuitem', { name: 'Delete' }));

    expect(await screen.findByText('Orphaned worktree')).toBeTruthy();
    expect(closeAgent).toHaveBeenNthCalledWith(1, agent.id, false, true);

    fireEvent.click(screen.getByRole('button', { name: 'Delete agent' }));
    await waitFor(() => expect(closeAgent).toHaveBeenNthCalledWith(2, agent.id, true, true, false));
  });
});

describe('SessionsRail touch context menu', () => {
  it('keeps cards draggable on touch-capable devices', () => {
    const originalMatchMedia = window.matchMedia;
    Object.defineProperty(window, 'matchMedia', {
      configurable: true,
      value: vi.fn().mockReturnValue({ matches: true }),
    });
    const agent = session();
    useStore.setState({ sessions: { [agent.id]: agent }, order: [agent.id] });
    const view = render(<SessionsRail />);

    expect(view.container.querySelector('.session-row')?.getAttribute('draggable')).toBe('true');
    Object.defineProperty(window, 'matchMedia', { configurable: true, value: originalMatchMedia });
  });

  it('opens from a stationary touch long-press and suppresses the following row click', async () => {
    vi.useFakeTimers();
    const agent = session();
    const focus = vi.fn();
    useStore.setState({ sessions: { [agent.id]: agent }, order: [agent.id], focus });
    const view = render(<SessionsRail />);
    const row = view.container.querySelector('.session-row')!;

    fireEvent(row, touchPointer('pointerdown', 4, 20, 30));
    await act(async () => { await vi.advanceTimersByTimeAsync(550); });
    fireEvent(row, touchPointer('pointerup', 4, 20, 30));
    fireEvent.click(row);

    expect(screen.getByRole('menu', { name: 'Actions for Remote agent' })).toBeTruthy();
    expect(focus).not.toHaveBeenCalled();
    vi.useRealTimers();
  });

  it('cancels a long-press when the finger starts scrolling', async () => {
    vi.useFakeTimers();
    const agent = session();
    useStore.setState({ sessions: { [agent.id]: agent }, order: [agent.id] });
    const view = render(<SessionsRail />);
    const row = view.container.querySelector('.session-row')!;

    fireEvent(row, touchPointer('pointerdown', 5, 20, 30));
    fireEvent(row, touchPointer('pointermove', 5, 20, 43));
    await act(async () => { await vi.advanceTimersByTimeAsync(550); });

    expect(screen.queryByRole('menu', { name: 'Actions for Remote agent' })).toBeNull();
    vi.useRealTimers();
  });
});
