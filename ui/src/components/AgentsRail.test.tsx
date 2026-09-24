import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { LOCAL_HOST_ID, useStore } from '../store';
import type { SessionView } from '../store';
import { SessionsRail } from './AgentsRail';

// Tests that stub store actions must not leak them into the next test.
const realActions = { reorderAgent: useStore.getState().reorderAgent };

afterEach(() => {
  cleanup();
  useStore.setState({ sessions: {}, order: [], focusedId: null, ...realActions });
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

  it('dismisses the menu only when a mobile drag changes the card position', () => {
    const [first, second] = ['first', 'second'].map((id) => ({
      ...session(), id, name: id, hostId: LOCAL_HOST_ID, hostName: 'This host',
    }));
    useStore.setState({
      sessions: { [first.id]: first, [second.id]: second },
      order: [first.id, second.id],
    });
    const view = render(<SessionsRail />);
    const rows = view.container.querySelectorAll('.session-row');
    const dataTransfer = { setData: vi.fn(), effectAllowed: '', dropEffect: '' };
    const dragStart = new MouseEvent('dragstart', { bubbles: true });
    Object.defineProperty(dragStart, 'dataTransfer', { value: dataTransfer });

    fireEvent.contextMenu(rows[0]!);
    fireEvent(rows[0]!, dragStart);
    fireEvent.dragEnd(rows[0]!);
    expect(screen.getByRole('menu', { name: 'Actions for first' })).toBeTruthy();

    const reorderStart = new MouseEvent('dragstart', { bubbles: true });
    Object.defineProperty(reorderStart, 'dataTransfer', { value: dataTransfer });
    fireEvent(rows[0]!, reorderStart);
    const drop = new MouseEvent('drop', { bubbles: true, clientY: 20 });
    Object.defineProperty(drop, 'dataTransfer', { value: dataTransfer });
    (rows[1] as HTMLElement).getBoundingClientRect = () => ({ top: 0, height: 20 }) as DOMRect;
    fireEvent(rows[1]!, drop);
    fireEvent.dragEnd(rows[0]!);
    fireEvent.contextMenu(rows[0]!);

    expect(screen.getByRole('menu', { name: 'Actions for first' }).classList.contains('closing')).toBe(true);
  });
});

describe('SessionsRail drag-to-reorder indicator', () => {
  const dataTransfer = () => ({ setData: vi.fn(), effectAllowed: '', dropEffect: '' });
  const rowFor = (view: ReturnType<typeof render>, index: number) =>
    view.container.querySelectorAll('.session-row')[index] as HTMLElement;
  // jsdom's DragEvent carries no pointer coordinates and lays nothing out, so
  // dispatch a mouse event under the drag type and give the row a rect.
  const dragEvent = (type: string, clientY: number) => {
    const event = new MouseEvent(type, { bubbles: true, clientY });
    Object.defineProperty(event, 'dataTransfer', { value: dataTransfer() });
    return event;
  };
  const over = (row: HTMLElement, half: 'top' | 'bottom', type: 'dragover' | 'drop') => {
    row.getBoundingClientRect = () => ({ top: 0, height: 20 }) as DOMRect;
    fireEvent(row, dragEvent(type, half === 'top' ? 4 : 16));
  };

  function railWithThree() {
    const sessions = ['a', 'b', 'c'].map((suffix) => ({
      ...session(), id: suffix, name: suffix, hostId: LOCAL_HOST_ID, hostName: 'This host',
    }));
    const reorderAgent = vi.fn();
    useStore.setState({
      sessions: Object.fromEntries(sessions.map((s) => [s.id, s])),
      order: ['a', 'b', 'c'],
      hosts: [{ id: LOCAL_HOST_ID, name: 'This host', status: 'connected', local: true }],
      reorderAgent,
    });
    const view = render(<SessionsRail />);
    fireEvent(rowFor(view, 1), dragEvent('dragstart', 0));
    return { view, reorderAgent };
  }

  it('shows one line per crack and none for the dragged card’s own slots', () => {
    const { view } = railWithThree();

    // The crack above the dragged row, reached from the row above's bottom
    // half or the dragged row itself: no move to promise, so no line.
    over(rowFor(view, 0), 'bottom', 'dragover');
    expect(view.container.querySelector('.drop-before, .drop-after')).toBeNull();
    // The crack below the dragged row, reached from the next row's top half.
    over(rowFor(view, 2), 'top', 'dragover');
    expect(view.container.querySelector('.drop-before, .drop-after')).toBeNull();

    // A real destination renders exactly one line, on the row below the crack.
    over(rowFor(view, 0), 'top', 'dragover');
    expect(view.container.querySelectorAll('.drop-before, .drop-after')).toHaveLength(1);
    expect(rowFor(view, 0).className).toContain('drop-before');
    // Past the last row the line has nowhere below it and hugs that row.
    over(rowFor(view, 2), 'bottom', 'dragover');
    expect(rowFor(view, 2).className).toContain('drop-after');
  });

  it('ignores a release on a crack the dragged card already occupies', () => {
    const { view, reorderAgent } = railWithThree();

    over(rowFor(view, 0), 'bottom', 'drop');
    expect(reorderAgent).not.toHaveBeenCalled();
  });

  it('reorders against the row below the crack the line was drawn at', () => {
    const { view, reorderAgent } = railWithThree();

    over(rowFor(view, 0), 'top', 'drop');
    expect(reorderAgent).toHaveBeenCalledWith('b', 'a', false);
  });
});

describe('SessionsRail drop animation', () => {
  const dataTransfer = () => ({ setData: vi.fn(), effectAllowed: '', dropEffect: '' });
  const dragEvent = (type: string, clientY: number) => {
    const event = new MouseEvent(type, { bubbles: true, clientY });
    Object.defineProperty(event, 'dataTransfer', { value: dataTransfer() });
    return event;
  };
  // jsdom lays nothing out, so give every row a 40px slot derived from where
  // it currently sits among its siblings — the reorder then moves it for real.
  const layOutRows = (view: ReturnType<typeof render>) => {
    for (const node of view.container.querySelectorAll('.session-row')) {
      const row = node as HTMLElement;
      row.getBoundingClientRect = () =>
        ({ top: [...row.parentElement!.children].indexOf(row) * 40, height: 40 }) as DOMRect;
    }
  };

  it('slides the displaced cards, leaving the dropped one where the pointer left it', () => {
    const animate = vi.fn();
    Object.defineProperty(Element.prototype, 'animate', { configurable: true, writable: true, value: animate });
    const sessions = ['a', 'b', 'c'].map((suffix) => ({
      ...session(), id: suffix, name: suffix, hostId: LOCAL_HOST_ID, hostName: 'This host',
    }));
    useStore.setState({
      sessions: Object.fromEntries(sessions.map((s) => [s.id, s])),
      order: ['a', 'b', 'c'],
      hosts: [{ id: LOCAL_HOST_ID, name: 'This host', status: 'connected', local: true }],
    });
    const view = render(<SessionsRail />);
    layOutRows(view);
    const rows = () => [...view.container.querySelectorAll('.session-row')].map((row) => row.querySelector('.name')?.textContent?.trim());

    const last = view.container.querySelectorAll('.session-row')[2] as HTMLElement;
    fireEvent(last, dragEvent('dragstart', 0));
    // The top half of the first row: 'c' goes to the front, 'a' and 'b' shift down.
    fireEvent(view.container.querySelectorAll('.session-row')[0]!, dragEvent('drop', 4));

    expect(useStore.getState().order).toEqual(['c', 'a', 'b']);
    expect(rows()[0]).toContain('c');
    // 'a' and 'b' each drop a slot from where they were; 'c' is already under
    // the pointer at the front, so it is not animated at all.
    const offsets = animate.mock.calls.map(([frames]) => (frames as Keyframe[])[0].transform);
    expect(offsets).toEqual(['translateY(-40px)', 'translateY(-40px)']);
    for (const [frames] of animate.mock.calls) expect((frames as Keyframe[])[1].transform).toBe('translateY(0)');
  });
});
