import { cleanup, fireEvent, render, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { useStore } from '../../store';
import type { AgentView } from '../../store';
import { TranscriptPane } from './TranscriptPane';

const initialState = useStore.getState();

function agent(): AgentView {
  return {
    id: 'agent-1',
    name: 'Mobile test',
    workspace: { kind: 'existing', repo: 'repo', repoPath: '/repo', branch: 'main', cwd: '/repo' },
    status: 'idle',
    events: [{ seq: 1, event: { kind: 'message_chunk', text: 'Select these words on a phone.' } }],
    lastSeq: 1,
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
    usage: null,
    commands: [],
    imagePromptSupport: null,
    queuedPrompts: [],
    controlMode: 'transcript',
    adapter: 'acp',
    canHandoff: false,
  };
}

afterEach(() => {
  cleanup();
  window.getSelection()?.removeAllRanges();
  useStore.setState(initialState, true);
  vi.restoreAllMocks();
});

describe('TranscriptPane annotations', () => {
  it('offers the comment action when native selection emits selectionchange', async () => {
    vi.stubGlobal('matchMedia', vi.fn().mockReturnValue({ matches: true }));
    vi.stubGlobal('PointerEvent', MouseEvent);
    const addAnnotation = vi.fn().mockResolvedValue({});
    useStore.setState({
      ...initialState,
      agents: { 'agent-1': agent() },
      order: ['agent-1'],
      focusedId: 'agent-1',
      annotations: { 'agent-1': [] },
      addAnnotation,
    }, true);
    const view = render(<TranscriptPane />);
    const text = view.container.querySelector('.ev.msg p')?.firstChild;
    expect(text).toBeInstanceOf(Text);

    const range = document.createRange();
    range.setStart(text!, 0);
    range.setEnd(text!, 18);
    Object.defineProperty(range, 'getBoundingClientRect', {
      value: () => ({ top: 100, left: 20, width: 140, height: 20, right: 160, bottom: 120, x: 20, y: 100, toJSON: () => ({}) }),
    });
    const selection = window.getSelection()!;
    selection.removeAllRanges();
    selection.addRange(range);

    document.dispatchEvent(new Event('selectionchange'));

    const comment = await waitFor(() => view.getByRole('button', { name: /comment/i }));
    fireEvent.click(comment);
    const handle = view.getByTitle('Drag to move comment');
    const popover = handle.parentElement!;
    Object.defineProperties(popover, {
      offsetWidth: { configurable: true, value: 320 },
      offsetHeight: { configurable: true, value: 160 },
    });
    Object.assign(handle, {
      setPointerCapture: vi.fn(),
      hasPointerCapture: vi.fn().mockReturnValue(true),
      releasePointerCapture: vi.fn(),
    });
    fireEvent.pointerDown(handle, { pointerId: 1, clientX: 400, clientY: 30 });
    fireEvent.pointerMove(handle, { pointerId: 1, clientX: 500, clientY: 200 });

    expect(popover.style.left).toBe('432px');
    expect(popover.style.top).toBe('178px');

    fireEvent.change(view.getByPlaceholderText('Add a comment…'), { target: { value: 'Please clarify.' } });
    fireEvent.click(view.getByRole('button', { name: 'Add' }));

    expect(addAnnotation).toHaveBeenCalledWith(
      'agent-1',
      { seq: 1, role: 'assistant', quote: 'Select these words' },
      'Please clarify.',
    );
  });
});
