import { act, cleanup, fireEvent, render, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { useStore } from '../../store';
import type { AgentView } from '../../store';
import { findFileToken, findSlashToken, TranscriptPane } from './TranscriptPane';

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
    asideSupport: null,
    queuedPrompts: [],
    controlMode: 'transcript',
    adapter: 'acp',
    canHandoff: false,
    audioOnTurnEnd: false,
    audioState: 'idle',
    audioError: null,
    audioSeq: null,
    audioReadySeqs: [],
    audioReadyRevision: 0,
  };
}

afterEach(() => {
  cleanup();
  window.getSelection()?.removeAllRanges();
  useStore.setState(initialState, true);
  vi.restoreAllMocks();
  localStorage.clear();
  delete (URL as unknown as Record<string, unknown>).createObjectURL;
  delete (URL as unknown as Record<string, unknown>).revokeObjectURL;
  delete (navigator as unknown as Record<string, unknown>).mediaSession;
});

describe('TranscriptPane voice rendering', () => {
  it('requests and exposes audio controls for a completed agent message', async () => {
    localStorage.setItem('tandem.token', 'test-token');
    (URL as typeof URL & { createObjectURL: (blob: Blob) => string }).createObjectURL = vi.fn().mockReturnValue('blob:voice');
    (URL as typeof URL & { revokeObjectURL: (url: string) => void }).revokeObjectURL = vi.fn();
    const fetchMock = vi.fn().mockResolvedValue(new Response(new Blob(['audio'], { type: 'audio/mpeg' }), { status: 200 }));
    vi.stubGlobal('fetch', fetchMock);
    useStore.setState({
      ...initialState,
      agents: { 'agent-1': agent() }, order: ['agent-1'], focusedId: 'agent-1', annotations: { 'agent-1': [] },
    }, true);

    const view = render(<TranscriptPane />);
    expect(view.getByRole('button', { name: 'Listen' }).closest('.message-audio')?.classList.contains('message-listen')).toBe(true);
    fireEvent.click(view.getByRole('button', { name: 'Listen' }));
    const player = await waitFor(() => view.getByLabelText('Spoken version of agent response'));
    expect(player.getAttribute('src')).toBe('blob:voice');
    expect(fetchMock).toHaveBeenCalledWith('/api/agents/agent-1/messages/1/audio', {
      method: 'POST', headers: { Authorization: 'Bearer test-token' },
    });
  });

  it('loads a daemon-cached clip when the chat is opened', async () => {
    const ready = agent();
    ready.audioState = 'ready';
    ready.audioSeq = 1;
    (URL as typeof URL & { createObjectURL: (blob: Blob) => string }).createObjectURL = vi.fn().mockReturnValue('blob:ready-voice');
    (URL as typeof URL & { revokeObjectURL: (url: string) => void }).revokeObjectURL = vi.fn();
    const fetchMock = vi.fn().mockResolvedValue(new Response(new Blob(['audio'], { type: 'audio/mpeg' }), { status: 200 }));
    vi.stubGlobal('fetch', fetchMock);
    useStore.setState({
      ...initialState,
      agents: { 'agent-1': ready }, order: ['agent-1'], focusedId: 'agent-1', annotations: { 'agent-1': [] },
    }, true);

    const view = render(<TranscriptPane />);

    expect(view.getByRole('status').textContent).toBe('Loading speech…');
    expect(view.container.querySelector('.message-audio')?.classList.contains('message-listen')).toBe(false);

    expect((await waitFor(() => view.getByLabelText('Spoken version of agent response'))).getAttribute('src')).toBe('blob:ready-voice');
    expect(fetchMock).toHaveBeenCalledOnce();
  });

  it('autoplays through the visible player and ignores duplicate ready events', async () => {
    const ready = agent();
    (URL as typeof URL & { createObjectURL: (blob: Blob) => string }).createObjectURL = vi.fn().mockReturnValue('blob:auto-voice');
    (URL as typeof URL & { revokeObjectURL: (url: string) => void }).revokeObjectURL = vi.fn();
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(new Blob(['audio'], { type: 'audio/mpeg' }), { status: 200 })));
    const play = vi.spyOn(HTMLMediaElement.prototype, 'play').mockResolvedValue(undefined);
    vi.spyOn(HTMLMediaElement.prototype, 'pause').mockImplementation(() => {});
    useStore.setState({
      ...initialState,
      agents: { 'agent-1': ready }, order: ['agent-1'], focusedId: 'agent-1', annotations: { 'agent-1': [] },
    }, true);

    const view = render(<TranscriptPane />);
    act(() => {
      useStore.setState((state) => ({
        agents: {
          ...state.agents,
          'agent-1': {
            ...state.agents['agent-1'],
            audioState: 'ready', audioSeq: 1, audioReadySeqs: [1], audioReadyRevision: 1,
          },
        },
      }));
    });

    const player = await waitFor(() => view.getByLabelText('Spoken version of agent response'));
    await waitFor(() => expect(play).toHaveBeenCalledOnce());
    expect(play.mock.instances[0]).toBe(player);

    act(() => {
      useStore.setState((state) => ({
        agents: {
          ...state.agents,
          'agent-1': { ...state.agents['agent-1'], audioReadyRevision: 2 },
        },
      }));
    });
    await act(async () => { await Promise.resolve(); });
    expect(play).toHaveBeenCalledOnce();
  });

  it('routes earbud seek and track commands to the active audio snippet', async () => {
    const handlers = new Map<string, ((details: { seekOffset?: number }) => void) | null>();
    const setPositionState = vi.fn();
    Object.defineProperty(navigator, 'mediaSession', {
      configurable: true,
      value: {
        playbackState: 'none',
        setActionHandler: vi.fn((action: string, handler: ((details: { seekOffset?: number }) => void) | null) => handlers.set(action, handler)),
        setPositionState,
      },
    });
    (URL as typeof URL & { createObjectURL: (blob: Blob) => string }).createObjectURL = vi.fn().mockReturnValue('blob:earbud-voice');
    (URL as typeof URL & { revokeObjectURL: (url: string) => void }).revokeObjectURL = vi.fn();
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(new Blob(['audio'], { type: 'audio/mpeg' }), { status: 200 })));
    useStore.setState({
      ...initialState,
      agents: { 'agent-1': agent() }, order: ['agent-1'], focusedId: 'agent-1', annotations: { 'agent-1': [] },
    }, true);

    const view = render(<TranscriptPane />);
    fireEvent.click(view.getByRole('button', { name: 'Listen' }));
    const player = await waitFor(() => view.getByLabelText('Spoken version of agent response')) as HTMLAudioElement;
    Object.defineProperty(player, 'duration', { configurable: true, value: 30 });
    player.currentTime = 12;
    fireEvent.play(player);

    handlers.get('seekforward')?.({ seekOffset: 5 });
    expect(player.currentTime).toBe(17);
    handlers.get('seekbackward')?.({ seekOffset: 20 });
    expect(player.currentTime).toBe(0);
    handlers.get('nexttrack')?.({});
    expect(player.currentTime).toBe(10);
    player.currentTime = 28;
    handlers.get('nexttrack')?.({});
    expect(player.currentTime).toBe(30);
    handlers.get('previoustrack')?.({});
    expect(player.currentTime).toBe(20);
    expect(setPositionState).toHaveBeenCalled();
  });
});

describe('TranscriptPane tool diffs', () => {
  it('renders ACP edit content with the shared colored unified diff', () => {
    const edited = agent();
    edited.events = [{
      seq: 1,
      event: {
        kind: 'tool_call', id: 'edit-1', title: 'Edit README.md', status: 'done',
        content: [{ type: 'diff', path: 'README.md', oldText: 'before\n', newText: 'after\n' }],
      },
    }];
    useStore.setState({
      ...initialState,
      agents: { 'agent-1': edited }, order: ['agent-1'], focusedId: 'agent-1', annotations: { 'agent-1': [] },
    }, true);

    const view = render(<TranscriptPane />);
    fireEvent.click(view.getByText('Edit README.md'));

    expect(view.container.querySelector('.diff-file')).not.toBeNull();
    expect(view.container.querySelector('.diff-line.remove .diff-text')?.textContent).toBe('-before');
    expect(view.container.querySelector('.diff-line.add .diff-text')?.textContent).toBe('+after');
  });
});

describe('TranscriptPane composer completions', () => {
  it('sends /btw through the aside path and renders its durable answer card', async () => {
    const withAside = agent();
    withAside.asideSupport = true;
    withAside.events = [
      { seq: 1, event: { kind: 'aside_started', asideId: 'aside-1', question: 'Why SQLite?' } },
      { seq: 2, event: { kind: 'aside_event', asideId: 'aside-1', event: { kind: 'message_chunk', text: 'It keeps deployment self-contained.' } } },
      { seq: 3, event: { kind: 'aside_completed', asideId: 'aside-1', stopReason: 'end_turn' } },
    ];
    const sendAside = vi.fn().mockResolvedValue({});
    useStore.setState({
      ...initialState,
      agents: { 'agent-1': withAside }, order: ['agent-1'], focusedId: 'agent-1', annotations: { 'agent-1': [] },
      drafts: { 'agent-1': '/btw Why SQLite?' }, aside: sendAside,
    }, true);

    const view = render(<TranscriptPane />);
    expect(view.getByText('Aside · excluded from future turns')).toBeTruthy();
    expect(view.getByText('It keeps deployment self-contained.')).toBeTruthy();
    fireEvent.keyDown(view.getByRole('textbox'), { key: 'Enter' });
    await waitFor(() => expect(sendAside).toHaveBeenCalledWith('agent-1', 'Why SQLite?'));
  });

  it('only recognizes slash commands at a message or whitespace boundary', () => {
    expect(findSlashToken('/help', 5)).toMatchObject({ start: 0, query: 'help' });
    expect(findSlashToken('ask /help', 9)).toMatchObject({ start: 4, query: 'help' });
    expect(findSlashToken('src/help', 8)).toBeNull();
    expect(findSlashToken('email/help', 10)).toBeNull();
  });

  it('recognizes workspace-relative @ file mentions', () => {
    expect(findFileToken('@src/index', 10)).toMatchObject({ start: 0, query: 'src/index' });
    expect(findFileToken('check @src/', 11)).toMatchObject({ start: 6, query: 'src/' });
	    expect(findFileToken('@~/Projects', 11)).toMatchObject({ start: 0, query: '~/Projects' });
    expect(findFileToken('person@example', 14)).toBeNull();
  });

  it('renders matching slash commands in the picker color after sending', () => {
    const withCommand = agent();
    withCommand.commands = [{ name: 'help', description: 'Show help' }];
    withCommand.events = [{ seq: 1, event: { kind: 'user_message', text: 'Run /help then src/help.' } }];
    useStore.setState({
      ...initialState,
      agents: { 'agent-1': withCommand }, order: ['agent-1'], focusedId: 'agent-1', annotations: { 'agent-1': [] },
    }, true);

    const view = render(<TranscriptPane />);
    expect(view.container.querySelector('.skill-mention')?.textContent).toBe('/help');
    expect(view.container.querySelectorAll('.skill-mention')).toHaveLength(1);
  });

  it('renders matching slash commands in the composer highlight layer', () => {
    const withCommand = agent();
    withCommand.commands = [{ name: 'help', description: 'Show help' }];
    useStore.setState({
      ...initialState,
      agents: { 'agent-1': withCommand }, order: ['agent-1'], focusedId: 'agent-1', annotations: { 'agent-1': [] },
      drafts: { 'agent-1': 'Run /help then src/help.' },
    }, true);

    const view = render(<TranscriptPane />);
    const highlight = view.container.querySelector('.prompt-text-highlight');
    expect(highlight?.querySelector('.skill-mention')?.textContent).toBe('/help');
    expect(highlight?.querySelectorAll('.skill-mention')).toHaveLength(1);
  });

  it('renders workspace file mentions like slash commands', () => {
    const withCommand = agent();
    withCommand.commands = [{ name: 'help', description: 'Show help' }];
    withCommand.events = [{ seq: 1, event: { kind: 'user_message', text: 'Read @FIX_ME.md then /help.' } }];
    useStore.setState({
      ...initialState,
      agents: { 'agent-1': withCommand }, order: ['agent-1'], focusedId: 'agent-1', annotations: { 'agent-1': [] },
      drafts: { 'agent-1': 'Read @FIX_ME.md then /help.' },
    }, true);

    const view = render(<TranscriptPane />);
    expect(view.container.querySelectorAll('.skill-mention')).toHaveLength(4);
    expect(Array.from(view.container.querySelectorAll('.skill-mention')).map((node) => node.textContent))
      .toEqual(['@FIX_ME.md', '/help', '@FIX_ME.md', '/help']);
  });

  it('lists and inserts workspace file mentions', async () => {
    const listWorkspaceEntries = vi.fn().mockResolvedValue([{ path: 'src/index.ts', isDir: false }]);
    useStore.setState({
      ...initialState,
      agents: { 'agent-1': agent() }, order: ['agent-1'], focusedId: 'agent-1', annotations: { 'agent-1': [] }, listWorkspaceEntries,
    }, true);
    const view = render(<TranscriptPane />);
    const composer = view.getByPlaceholderText(/Prompt agent-1/i) as HTMLTextAreaElement;
    fireEvent.change(composer, { target: { value: '@src/in', selectionStart: 7 } });

    const option = await waitFor(() => view.getByText('@src/index.ts'));
    expect(listWorkspaceEntries).toHaveBeenCalledWith('agent-1', 'src');
    fireEvent.mouseDown(option);
    expect(composer.value).toBe('@src/index.ts ');
  });

  it('lists and inserts file mentions from the workspace parent', async () => {
    const listWorkspaceEntries = vi.fn().mockResolvedValue([{ path: '../sibling', isDir: true }]);
    useStore.setState({
      ...initialState,
      agents: { 'agent-1': agent() }, order: ['agent-1'], focusedId: 'agent-1', annotations: { 'agent-1': [] }, listWorkspaceEntries,
    }, true);
    const view = render(<TranscriptPane />);
    const composer = view.getByPlaceholderText(/Prompt agent-1/i) as HTMLTextAreaElement;
    fireEvent.change(composer, { target: { value: '@../', selectionStart: 4 } });

    const option = await waitFor(() => view.getByText('@../sibling/'));
    expect(listWorkspaceEntries).toHaveBeenCalledWith('agent-1', '..');
    fireEvent.mouseDown(option);
    expect(composer.value).toBe('@../sibling/');
  });

  it('lists and inserts file mentions from the home directory', async () => {
    const listWorkspaceEntries = vi.fn().mockResolvedValue([{ path: '~/Projects', isDir: true }]);
    useStore.setState({
      ...initialState,
      agents: { 'agent-1': agent() }, order: ['agent-1'], focusedId: 'agent-1', annotations: { 'agent-1': [] }, listWorkspaceEntries,
    }, true);
    const view = render(<TranscriptPane />);
    const composer = view.getByPlaceholderText(/Prompt agent-1/i) as HTMLTextAreaElement;
    fireEvent.change(composer, { target: { value: '@~/Projects', selectionStart: 11 } });

    const option = await waitFor(() => view.getByText('@~/Projects/'));
    expect(listWorkspaceEntries).toHaveBeenCalledWith('agent-1', '~');
    fireEvent.mouseDown(option);
    expect(composer.value).toBe('@~/Projects/');
  });
});

describe('TranscriptPane annotations', () => {
  it('renders annotations above the task list', () => {
    const withPlan = agent();
    withPlan.events = [
      ...withPlan.events,
      { seq: 2, event: { kind: 'plan', entries: [{ label: 'Implement the change', status: 'in_progress' }] } },
    ];
    useStore.setState({
      ...initialState,
      agents: { 'agent-1': withPlan },
      order: ['agent-1'],
      focusedId: 'agent-1',
      annotations: {
        'agent-1': [{ id: 'annotation-1', agentId: 'agent-1', seq: 1, role: 'assistant', quote: 'Select these words', comment: 'Clarify this.', createdAt: 1, updatedAt: 1 }],
      },
    }, true);

    const view = render(<TranscriptPane />);
    const tray = view.container.querySelector('.annotation-tray');
    const taskList = view.container.querySelector('.task-list');

    expect(tray).not.toBeNull();
    expect(taskList).not.toBeNull();
    expect(tray!.compareDocumentPosition(taskList!) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

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

  it('adds a comment on Enter and preserves a newline on Shift+Enter', async () => {
    vi.stubGlobal('matchMedia', vi.fn().mockReturnValue({ matches: true }));
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

    fireEvent.click(await waitFor(() => view.getByRole('button', { name: /comment/i })));
    const input = view.getByPlaceholderText('Add a comment…');
    fireEvent.change(input, { target: { value: 'First line' } });
    expect(fireEvent.keyDown(input, { key: 'Enter', shiftKey: true })).toBe(true);
    expect(addAnnotation).not.toHaveBeenCalled();
    fireEvent.change(input, { target: { value: 'First line\nSecond line' } });
    fireEvent.keyDown(input, { key: 'Enter' });

    expect(addAnnotation).toHaveBeenCalledWith(
      'agent-1',
      { seq: 1, role: 'assistant', quote: 'Select these words' },
      'First line\nSecond line',
    );
  });
});
