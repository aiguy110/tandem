import { act, cleanup, fireEvent, render, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { useStore } from '../../store';
import type { SessionView } from '../../store';
import { findFileToken, findSlashToken, TranscriptPane } from './TranscriptPane';
import { __resetForTests } from '../../audio/engine';
import { AudioEngineRoot } from '../audio/AudioEngineRoot';

const initialState = useStore.getState();

// Every render of TranscriptPane sets the audio engine's playlist (even when
// no message has audio yet), and switching agents pauses whatever element it
// was previously driving — jsdom's real HTMLMediaElement.pause() throws "not
// implemented", so stub it globally rather than per audio test.
beforeEach(() => {
  vi.spyOn(HTMLMediaElement.prototype, 'pause').mockImplementation(() => {});
});

function agent(): SessionView {
  return {
    id: 'session-1',
    name: 'Mobile test',
    workspace: { kind: 'existing', repo: 'repo', repoPath: '/repo', branch: 'main', cwd: '/repo' },
    status: 'idle',
    events: [{ seq: 1, event: { kind: 'message_chunk', text: 'Select these words on a phone.' } }],
    lastSeq: 1,
    pendingApprovals: [],
    turnNotifications: [],
    hasPty: false,
    shellExited: false,
    historyLoaded: true,
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
    steeringSupport: null,
    queuedPrompts: [],
    controlMode: 'transcript',
    adapter: 'acp',
    canHandoff: false,
    audioOnTurnEnd: false,
    audioState: 'idle',
    audioError: null,
    audioSeq: null,
    audioReadySeqs: [],
    audioDurations: {},
    audioReadyRevision: 0,
    audioPosition: null,
  };
}

afterEach(() => {
  cleanup();
  window.getSelection()?.removeAllRanges();
  useStore.setState(initialState, true);
  // Before restoring mocks: __resetForTests() pauses whatever element the
  // engine was driving, and the beforeEach pause() stub needs to still be in
  // place for that (jsdom's real pause() throws "not implemented").
  __resetForTests();
  vi.restoreAllMocks();
  localStorage.clear();
  delete (URL as unknown as Record<string, unknown>).createObjectURL;
  delete (URL as unknown as Record<string, unknown>).revokeObjectURL;
  delete (navigator as unknown as Record<string, unknown>).mediaSession;
});

describe('TranscriptPane voice rendering', () => {
  it('shows daemon-reported clip duration before playback', async () => {
    const ready = agent();
    ready.audioReadySeqs = [1];
    ready.audioDurations = { 1: 65_000 };
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(new Blob(['audio'], { type: 'audio/mpeg' }), { status: 200 })));
    (URL as typeof URL & { createObjectURL: (blob: Blob) => string }).createObjectURL = vi.fn().mockReturnValue('blob:ready-voice');
    useStore.setState({
      ...initialState,
      sessions: { 'session-1': ready }, order: ['session-1'], focusedId: 'session-1', annotations: { 'session-1': [] },
    }, true);

    const view = render(<><AudioEngineRoot /><TranscriptPane /></>);

    await waitFor(() => expect(view.getByText('-1:05')).toBeTruthy());
  });

  it('shows a failed image in a user prompt without corrupting React text nodes', async () => {
    const withImage = agent();
    withImage.events = [{
      seq: 1,
      event: { kind: 'user_message', blocks: [{ type: 'text', text: 'look' }, { type: 'image', assetId: 'missing', mimeType: 'image/png', name: 'shot.png' }] },
    }];
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(null, { status: 404 })));
    useStore.setState({
      ...initialState,
      sessions: { 'session-1': withImage }, order: ['session-1'], focusedId: 'session-1', annotations: { 'session-1': [] },
    }, true);

    const view = render(<TranscriptPane />);

    await waitFor(() => expect(view.getByText('Image unavailable: shot.png')).toBeTruthy());
  });

  it('shows complete command and output when an execute tool call is expanded', () => {
    const withTool = agent();
    const command = 'cd /a/very/long/path && npm run a-command-with-a-long-name -- --verbose';
    const output = 'first output line\nsecond output line\nthird output line';
    withTool.events = [{
      seq: 1,
      event: { kind: 'tool_call', id: 'bash-1', title: 'Bash', status: 'done', toolKind: 'execute', rawInput: { command }, content: output },
    }];
    withTool.lastSeq = 1;
    useStore.setState({
      ...initialState,
      sessions: { 'session-1': withTool }, order: ['session-1'], focusedId: 'session-1', annotations: { 'session-1': [] },
    }, true);

    const view = render(<TranscriptPane />);
    fireEvent.click(view.container.querySelector('.card-head')!);

    expect(view.getByText('Command')).toBeTruthy();
    expect(view.container.querySelector('.tool-terminal-command')?.textContent).toBe(command);
    expect(view.getByText('Output')).toBeTruthy();
    expect(view.container.querySelector('.tool-terminal-output')?.textContent).toBe(output);
  });

  it('shows arguments for an execute tool whose input is not a command string', () => {
    const withTool = agent();
    const code = 'import bpy\nbpy.ops.mesh.primitive_cube_add()';
    withTool.events = [{
      seq: 1,
      event: {
        kind: 'tool_call', id: 'mcp-1', title: 'mcp.blender.blender_execute_script', status: 'done',
        toolKind: 'execute', rawInput: { server: 'blender', arguments: { code } },
      },
    }];
    withTool.lastSeq = 1;
    useStore.setState({
      ...initialState,
      sessions: { 'session-1': withTool }, order: ['session-1'], focusedId: 'session-1', annotations: { 'session-1': [] },
    }, true);

    const view = render(<TranscriptPane />);
    fireEvent.click(view.container.querySelector('.card-head')!);

    expect(view.getByText('Arguments')).toBeTruthy();
    expect(view.container.querySelector('.tool-args pre')?.textContent).toContain('primitive_cube_add');
  });

  it('prefers an MCP-nested command string over the tool title', () => {
    const withTool = agent();
    withTool.events = [{
      seq: 1,
      event: {
        kind: 'tool_call', id: 'mcp-2', title: 'mcp.shell.run', status: 'done',
        toolKind: 'execute', rawInput: { server: 'shell', arguments: { command: 'ls -la' } },
      },
    }];
    withTool.lastSeq = 1;
    useStore.setState({
      ...initialState,
      sessions: { 'session-1': withTool }, order: ['session-1'], focusedId: 'session-1', annotations: { 'session-1': [] },
    }, true);

    const view = render(<TranscriptPane />);
    fireEvent.click(view.container.querySelector('.card-head')!);

    expect(view.container.querySelector('.tool-terminal-command')?.textContent).toBe('ls -la');
    expect(view.queryByText('Arguments')).toBeNull();
  });

  it('removes an outer Markdown code fence from execute output', () => {
    const withTool = agent();
    withTool.events = [{
      seq: 1,
      event: {
        kind: 'tool_call', id: 'bash-1', title: 'Bash', status: 'done', toolKind: 'execute',
        rawInput: { command: 'git status' }, content: '```console\nOn branch main\n```',
      },
    }];
    withTool.lastSeq = 1;
    useStore.setState({
      ...initialState,
      sessions: { 'session-1': withTool }, order: ['session-1'], focusedId: 'session-1', annotations: { 'session-1': [] },
    }, true);

    const view = render(<TranscriptPane />);
    fireEvent.click(view.container.querySelector('.card-head')!);

    expect(view.container.querySelector('.tool-terminal-output')?.textContent).toBe('On branch main');
  });

  it('streams ACP terminal output into its running tool card', async () => {
    const withTool = agent();
    withTool.events = [
      {
        seq: 1,
        event: {
          kind: 'tool_call', id: 'bash-1', title: 'Bash', status: 'running', toolKind: 'execute',
          rawInput: { command: 'while true; do date; done' }, terminalId: 'terminal-1',
        },
      },
      { seq: 2, event: { kind: 'terminal_output', termId: 'terminal-1', chunk: 'first output line\n', truncated: false } },
      { seq: 3, event: { kind: 'terminal_output', termId: 'terminal-1', chunk: 'second output line\n', truncated: false } },
    ];
    withTool.lastSeq = 3;
    useStore.setState({
      ...initialState,
      sessions: { 'session-1': withTool }, order: ['session-1'], focusedId: 'session-1', annotations: { 'session-1': [] },
    }, true);

    const view = render(<TranscriptPane />);
    expect(view.container.querySelector('.tool-terminal-output')).toBeNull();
    fireEvent.click(view.container.querySelector('.card-head')!);
    await waitFor(() => expect(view.container.querySelector('.tool-terminal-output')?.textContent).toBe('first output line\nsecond output line\n'));
    expect(view.container.querySelector('.mini-term')).toBeNull();
  });

  it('shows tool arguments before output when a tool call is expanded', () => {
    const withTool = agent();
    withTool.events = [{
      seq: 1,
      event: {
        kind: 'tool_call', id: 'search-1', title: 'ToolSearch', status: 'done',
        rawInput: { query: 'Microsoft 365', max_results: 15 }, content: 'Tool: ListMcpResourcesTool',
      },
    }];
    withTool.lastSeq = 1;
    useStore.setState({
      ...initialState,
      sessions: { 'session-1': withTool }, order: ['session-1'], focusedId: 'session-1', annotations: { 'session-1': [] },
    }, true);

    const view = render(<TranscriptPane />);
    fireEvent.click(view.container.querySelector('.card-head')!);

    const body = view.container.querySelector('.card-body')!;
    const argumentsLabel = Array.from(body.querySelectorAll('.tool-args-label')).find((label) => label.textContent === 'Arguments')!;
    const outputLabel = Array.from(body.querySelectorAll('.tool-args-label')).find((label) => label.textContent === 'Output')!;
    expect(argumentsLabel.compareDocumentPosition(outputLabel) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it('requests and exposes audio controls for a completed agent message', async () => {
    localStorage.setItem('tandem.token', 'test-token');
    (URL as typeof URL & { createObjectURL: (blob: Blob) => string }).createObjectURL = vi.fn().mockReturnValue('blob:voice');
    (URL as typeof URL & { revokeObjectURL: (url: string) => void }).revokeObjectURL = vi.fn();
    const fetchMock = vi.fn().mockResolvedValue(new Response(new Blob(['audio'], { type: 'audio/mpeg' }), { status: 200 }));
    vi.stubGlobal('fetch', fetchMock);
    useStore.setState({
      ...initialState,
      sessions: { 'session-1': agent() }, order: ['session-1'], focusedId: 'session-1', annotations: { 'session-1': [] },
    }, true);

    const view = render(<><AudioEngineRoot /><TranscriptPane /></>);
    expect(view.getByRole('button', { name: 'Listen' }).closest('.message-audio')?.classList.contains('message-listen')).toBe(true);
    fireEvent.click(view.getByRole('button', { name: 'Listen' }));
    // Rows no longer own a media element (playback is centralized in the
    // engine, see ui/src/audio/engine.ts) -- readiness now shows up as the
    // row's InlineAudioBar play control.
    await waitFor(() => expect(view.getByRole('button', { name: 'Play response audio' })).toBeTruthy());
    expect(fetchMock).toHaveBeenCalledWith('/api/agents/session-1/messages/1/audio', {
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
      sessions: { 'session-1': ready }, order: ['session-1'], focusedId: 'session-1', annotations: { 'session-1': [] },
    }, true);

    const view = render(<><AudioEngineRoot /><TranscriptPane /></>);

    expect(view.getByRole('status').textContent).toBe('Loading speech…');
    expect(view.container.querySelector('.message-audio')?.classList.contains('message-listen')).toBe(false);

    await waitFor(() => expect(view.getByRole('button', { name: 'Play response audio' })).toBeTruthy());
    expect(fetchMock).toHaveBeenCalledOnce();
  });

  it('autoplays through the shared element and ignores duplicate ready events', async () => {
    const ready = agent();
    (URL as typeof URL & { createObjectURL: (blob: Blob) => string }).createObjectURL = vi.fn().mockReturnValue('blob:auto-voice');
    (URL as typeof URL & { revokeObjectURL: (url: string) => void }).revokeObjectURL = vi.fn();
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(new Blob(['audio'], { type: 'audio/mpeg' }), { status: 200 })));
    const playSpy = vi.spyOn(HTMLMediaElement.prototype, 'play').mockResolvedValue(undefined);
    vi.spyOn(HTMLMediaElement.prototype, 'pause').mockImplementation(() => {});
    useStore.setState({
      ...initialState,
      sessions: { 'session-1': ready }, order: ['session-1'], focusedId: 'session-1', annotations: { 'session-1': [] },
    }, true);

    const view = render(<><AudioEngineRoot /><TranscriptPane /></>);
    act(() => {
      useStore.setState((state) => ({
        sessions: {
          ...state.sessions,
          'session-1': {
            ...state.sessions['session-1'],
            audioState: 'ready', audioSeq: 1, audioReadySeqs: [1], audioReadyRevision: 1,
          },
        },
      }));
    });

    await waitFor(() => expect(playSpy).toHaveBeenCalledOnce());
    // There is exactly one <audio> element for the whole app -- confirm it's
    // the one that was played, not some per-row element.
    expect(playSpy.mock.instances[0]).toBe(view.getByTestId('audio-engine-element'));

    act(() => {
      useStore.setState((state) => ({
        sessions: {
          ...state.sessions,
          'session-1': { ...state.sessions['session-1'], audioReadyRevision: 2 },
        },
      }));
    });
    await act(async () => { await Promise.resolve(); });
    expect(playSpy).toHaveBeenCalledOnce();
  });

  it('routes earbud seek and track commands to the active audio snippet', async () => {
    const handlers = new Map<string, ((details: { seekOffset?: number; seekTime?: number }) => void) | null>();
    const setPositionState = vi.fn();
    Object.defineProperty(navigator, 'mediaSession', {
      configurable: true,
      value: {
        playbackState: 'none',
        setActionHandler: vi.fn((action: string, handler: ((details: { seekOffset?: number; seekTime?: number }) => void) | null) => handlers.set(action, handler)),
        setPositionState,
      },
    });
    (URL as typeof URL & { createObjectURL: (blob: Blob) => string }).createObjectURL = vi.fn().mockReturnValue('blob:earbud-voice');
    (URL as typeof URL & { revokeObjectURL: (url: string) => void }).revokeObjectURL = vi.fn();
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(new Blob(['audio'], { type: 'audio/mpeg' }), { status: 200 })));
    vi.spyOn(HTMLMediaElement.prototype, 'play').mockResolvedValue(undefined);
    useStore.setState({
      ...initialState,
      sessions: { 'session-1': agent() }, order: ['session-1'], focusedId: 'session-1', annotations: { 'session-1': [] },
    }, true);

    const view = render(<><AudioEngineRoot /><TranscriptPane /></>);
    fireEvent.click(view.getByRole('button', { name: 'Listen' }));
    await waitFor(() => view.getByRole('button', { name: 'Play response audio' }));
    fireEvent.click(view.getByRole('button', { name: 'Play response audio' }));
    const player = await waitFor(() => view.getByTestId('audio-engine-element')) as HTMLAudioElement;
    await waitFor(() => expect(player.src).toContain('blob:earbud-voice'));

    Object.defineProperty(player, 'duration', { configurable: true, value: 30 });
    player.currentTime = 12;

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
    await waitFor(() => expect(setPositionState).toHaveBeenCalled());
  });

  it('keeps earbud play/pause working after a snippet finishes', async () => {
    const handlers = new Map<string, ((details: { seekOffset?: number }) => void) | null>();
    const session = {
      playbackState: 'none',
      setActionHandler: vi.fn((action: string, handler: ((details: { seekOffset?: number }) => void) | null) => handlers.set(action, handler)),
      setPositionState: vi.fn(),
    };
    Object.defineProperty(navigator, 'mediaSession', { configurable: true, value: session });
    (URL as typeof URL & { createObjectURL: (blob: Blob) => string }).createObjectURL = vi.fn().mockReturnValue('blob:earbud-voice');
    (URL as typeof URL & { revokeObjectURL: (url: string) => void }).revokeObjectURL = vi.fn();
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(new Blob(['audio'], { type: 'audio/mpeg' }), { status: 200 })));
    const playSpy = vi.spyOn(HTMLMediaElement.prototype, 'play').mockResolvedValue(undefined);
    useStore.setState({
      ...initialState,
      sessions: { 'session-1': agent() }, order: ['session-1'], focusedId: 'session-1', annotations: { 'session-1': [] },
    }, true);

    const view = render(<><AudioEngineRoot /><TranscriptPane /></>);
    fireEvent.click(view.getByRole('button', { name: 'Listen' }));
    await waitFor(() => view.getByRole('button', { name: 'Play response audio' }));
    fireEvent.click(view.getByRole('button', { name: 'Play response audio' }));
    await waitFor(() => expect(playSpy).toHaveBeenCalled());
    expect(handlers.get('play')).toBeTruthy();

    const player = view.getByTestId('audio-engine-element') as HTMLAudioElement;
    Object.defineProperty(player, 'duration', { configurable: true, value: 30 });
    fireEvent.ended(player);

    // The finished clip keeps the OS controls instead of handing them back.
    await waitFor(() => expect(session.playbackState).toBe('paused'));
    expect(session.setActionHandler).not.toHaveBeenCalledWith('play', null);
    expect(session.setActionHandler).not.toHaveBeenCalledWith('pause', null);

    playSpy.mockClear();
    handlers.get('play')?.({});
    expect(playSpy).toHaveBeenCalledOnce();
    expect(handlers.get('pause')).toBeTruthy();
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
      sessions: { 'session-1': edited }, order: ['session-1'], focusedId: 'session-1', annotations: { 'session-1': [] },
    }, true);

    const view = render(<TranscriptPane />);
    fireEvent.click(view.getByText('Edit README.md'));

    expect(view.container.querySelector('.diff-file')).not.toBeNull();
    expect(view.container.querySelector('.diff-line.remove .diff-text')?.textContent).toBe('-before');
    expect(view.container.querySelector('.diff-line.add .diff-text')?.textContent).toBe('+after');
  });

  it('shows unchanged ACP snapshot lines as context instead of replacing the entire file', () => {
    const edited = agent();
    edited.events = [{
      seq: 1,
      event: {
        kind: 'tool_call', id: 'edit-1', title: 'Edit README.md', status: 'done',
        content: [{ type: 'diff', path: 'README.md', oldText: 'one\ntwo\nthree\nfour\n', newText: 'one\ntwo changed\nthree\nfour\n' }],
      },
    }];
    useStore.setState({
      ...initialState,
      sessions: { 'session-1': edited }, order: ['session-1'], focusedId: 'session-1', annotations: { 'session-1': [] },
    }, true);

    const view = render(<TranscriptPane />);
    fireEvent.click(view.getByText('Edit README.md'));

    expect([...view.container.querySelectorAll('.diff-line.remove .diff-text')].map((node) => node.textContent)).toEqual(['-two']);
    expect([...view.container.querySelectorAll('.diff-line.add .diff-text')].map((node) => node.textContent)).toEqual(['+two changed']);
    expect([...view.container.querySelectorAll('.diff-line.context .diff-text')].map((node) => node.textContent)).toEqual([' one', ' three', ' four']);
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
      sessions: { 'session-1': withAside }, order: ['session-1'], focusedId: 'session-1', annotations: { 'session-1': [] },
      drafts: { 'session-1': '/btw Why SQLite?' }, aside: sendAside,
    }, true);

    const view = render(<TranscriptPane />);
    expect(view.getByText('Aside · excluded from future turns')).toBeTruthy();
    expect(view.getByText('It keeps deployment self-contained.')).toBeTruthy();
    fireEvent.keyDown(view.getByRole('textbox'), { key: 'Enter' });
    await waitFor(() => expect(sendAside).toHaveBeenCalledWith('session-1', 'Why SQLite?'));
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
      sessions: { 'session-1': withCommand }, order: ['session-1'], focusedId: 'session-1', annotations: { 'session-1': [] },
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
      sessions: { 'session-1': withCommand }, order: ['session-1'], focusedId: 'session-1', annotations: { 'session-1': [] },
      drafts: { 'session-1': 'Run /help then src/help.' },
    }, true);

    const view = render(<TranscriptPane />);
    const highlight = view.container.querySelector('.prompt-text-highlight');
    expect(highlight?.querySelector('.skill-mention')?.textContent).toBe('/help');
    expect(highlight?.querySelectorAll('.skill-mention')).toHaveLength(1);
  });

  it('highlights a leading /btw when asides are supported', () => {
    const withAside = agent();
    withAside.asideSupport = true;
    useStore.setState({
      ...initialState,
      sessions: { 'session-1': withAside }, order: ['session-1'], focusedId: 'session-1', annotations: { 'session-1': [] },
      drafts: { 'session-1': '/btw Is this isolated?' },
    }, true);

    const view = render(<TranscriptPane />);
    expect(view.container.querySelector('.aside-mention')?.textContent).toBe('/btw');
  });

  it('does not highlight /btw away from the start of a prompt', () => {
    const withAside = agent();
    withAside.asideSupport = true;
    useStore.setState({
      ...initialState,
      sessions: { 'session-1': withAside }, order: ['session-1'], focusedId: 'session-1', annotations: { 'session-1': [] },
      drafts: { 'session-1': 'Ask first, then /btw this.' },
    }, true);

    const view = render(<TranscriptPane />);
    expect(view.container.querySelector('.aside-mention')).toBeNull();
  });

  it('renders workspace file mentions like slash commands', () => {
    const withCommand = agent();
    withCommand.commands = [{ name: 'help', description: 'Show help' }];
    withCommand.events = [{ seq: 1, event: { kind: 'user_message', text: 'Read @FIX_ME.md then /help.' } }];
    useStore.setState({
      ...initialState,
      sessions: { 'session-1': withCommand }, order: ['session-1'], focusedId: 'session-1', annotations: { 'session-1': [] },
      drafts: { 'session-1': 'Read @FIX_ME.md then /help.' },
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
      sessions: { 'session-1': agent() }, order: ['session-1'], focusedId: 'session-1', annotations: { 'session-1': [] }, listWorkspaceEntries,
    }, true);
    const view = render(<TranscriptPane />);
    const composer = view.getByPlaceholderText(/Prompt Mobile test/i) as HTMLTextAreaElement;
    fireEvent.change(composer, { target: { value: '@src/in', selectionStart: 7 } });

    const option = await waitFor(() => view.getByText('@src/index.ts'));
    expect(listWorkspaceEntries).toHaveBeenCalledWith('session-1', 'src');
    fireEvent.mouseDown(option);
    expect(composer.value).toBe('@src/index.ts ');
  });

  it('lists and inserts file mentions from the workspace parent', async () => {
    const listWorkspaceEntries = vi.fn().mockResolvedValue([{ path: '../sibling', isDir: true }]);
    useStore.setState({
      ...initialState,
      sessions: { 'session-1': agent() }, order: ['session-1'], focusedId: 'session-1', annotations: { 'session-1': [] }, listWorkspaceEntries,
    }, true);
    const view = render(<TranscriptPane />);
    const composer = view.getByPlaceholderText(/Prompt Mobile test/i) as HTMLTextAreaElement;
    fireEvent.change(composer, { target: { value: '@../', selectionStart: 4 } });

    const option = await waitFor(() => view.getByText('@../sibling/'));
    expect(listWorkspaceEntries).toHaveBeenCalledWith('session-1', '..');
    fireEvent.mouseDown(option);
    expect(composer.value).toBe('@../sibling/');
  });

  it('lists and inserts file mentions from the home directory', async () => {
    const listWorkspaceEntries = vi.fn().mockResolvedValue([{ path: '~/Projects', isDir: true }]);
    useStore.setState({
      ...initialState,
      sessions: { 'session-1': agent() }, order: ['session-1'], focusedId: 'session-1', annotations: { 'session-1': [] }, listWorkspaceEntries,
    }, true);
    const view = render(<TranscriptPane />);
    const composer = view.getByPlaceholderText(/Prompt Mobile test/i) as HTMLTextAreaElement;
    fireEvent.change(composer, { target: { value: '@~/Projects', selectionStart: 11 } });

    const option = await waitFor(() => view.getByText('@~/Projects/'));
    expect(listWorkspaceEntries).toHaveBeenCalledWith('session-1', '~');
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
      sessions: { 'session-1': withPlan },
      order: ['session-1'],
      focusedId: 'session-1',
      annotations: {
        'session-1': [{ id: 'annotation-1', sessionId: 'session-1', seq: 1, role: 'assistant', quote: 'Select these words', comment: 'Clarify this.', createdAt: 1, updatedAt: 1 }],
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
      sessions: { 'session-1': agent() },
      order: ['session-1'],
      focusedId: 'session-1',
      annotations: { 'session-1': [] },
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
      'session-1',
      { seq: 1, role: 'assistant', quote: 'Select these words' },
      'Please clarify.',
    );
  });

  it('adds a comment on Enter and preserves a newline on Shift+Enter', async () => {
    vi.stubGlobal('matchMedia', vi.fn().mockReturnValue({ matches: true }));
    const addAnnotation = vi.fn().mockResolvedValue({});
    useStore.setState({
      ...initialState,
      sessions: { 'session-1': agent() },
      order: ['session-1'],
      focusedId: 'session-1',
      annotations: { 'session-1': [] },
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
      'session-1',
      { seq: 1, role: 'assistant', quote: 'Select these words' },
      'First line\nSecond line',
    );
  });

  it('keeps and restores an open comment draft across outside clicks, refreshes, and chat switches', async () => {
    vi.stubGlobal('matchMedia', vi.fn().mockReturnValue({ matches: true }));
    const second = agent();
    second.id = 'session-2';
    second.name = 'Second chat';
    useStore.setState({
      ...initialState,
      sessions: { 'session-1': agent(), 'session-2': second },
      order: ['session-1', 'session-2'],
      focusedId: 'session-1',
      annotations: { 'session-1': [], 'session-2': [] },
    }, true);
    const view = render(<TranscriptPane />);
    const text = view.container.querySelector('.ev.msg p')?.firstChild!;
    const range = document.createRange();
    range.setStart(text, 0);
    range.setEnd(text, 18);
    Object.defineProperty(range, 'getBoundingClientRect', {
      value: () => ({ top: 100, left: 20, width: 140, height: 20, right: 160, bottom: 120, x: 20, y: 100, toJSON: () => ({}) }),
    });
    const selection = window.getSelection()!;
    selection.removeAllRanges();
    selection.addRange(range);
    document.dispatchEvent(new Event('selectionchange'));
    fireEvent.click(await waitFor(() => view.getByRole('button', { name: /comment/i })));
    fireEvent.change(view.getByPlaceholderText('Add a comment…'), { target: { value: 'Keep this draft.' } });

    // Outside interaction is intentionally inert while a comment is open.
    fireEvent.pointerDown(document.body);
    expect((view.getByPlaceholderText('Add a comment…') as HTMLTextAreaElement).value).toBe('Keep this draft.');
    const saved = JSON.parse(localStorage.getItem('tandem.annotationCommentDrafts') ?? '{}');
    expect(saved['session-1'][0]).toMatchObject({
      text: 'Keep this draft.',
      anchor: { seq: 1, role: 'assistant', quote: 'Select these words', range: { start: 0, end: 18 } },
    });
    expect(saved['session-1'][0].position).toEqual({ top: 8, left: 332 });

    act(() => useStore.setState({ focusedId: 'session-2' }));
    await waitFor(() => expect(view.queryByPlaceholderText('Add a comment…')).toBeNull());
    act(() => useStore.setState({ focusedId: 'session-1' }));
    await waitFor(() => expect((view.getByPlaceholderText('Add a comment…') as HTMLTextAreaElement).value).toBe('Keep this draft.'));

    view.unmount();
    const restored = render(<TranscriptPane />);
    await waitFor(() => expect((restored.getByPlaceholderText('Add a comment…') as HTMLTextAreaElement).value).toBe('Keep this draft.'));
    const popover = restored.container.querySelector<HTMLElement>('.annotation-popover')!;
    expect(popover.style.top).toBe(`${saved['session-1'][0].position.top}px`);
    expect(popover.style.left).toBe(`${saved['session-1'][0].position.left}px`);
  });

  it('keeps multiple selected comment boxes editable at the same time', async () => {
    vi.stubGlobal('matchMedia', vi.fn().mockReturnValue({ matches: false }));
    useStore.setState({
      ...initialState,
      sessions: { 'session-1': agent() }, order: ['session-1'], focusedId: 'session-1', annotations: { 'session-1': [] },
    }, true);
    const view = render(<TranscriptPane />);
    const text = view.container.querySelector('.ev.msg p')?.firstChild!;
    const select = async (start: number, end: number, left: number) => {
      const range = document.createRange();
      range.setStart(text, start);
      range.setEnd(text, end);
      Object.defineProperty(range, 'getBoundingClientRect', { value: () => ({ top: 100, left, width: 80, height: 20, right: left + 80, bottom: 120, x: left, y: 100, toJSON: () => ({}) }) });
      const selection = window.getSelection()!;
      selection.removeAllRanges();
      selection.addRange(range);
      document.dispatchEvent(new Event('selectionchange'));
      fireEvent.click(await waitFor(() => view.getByRole('button', { name: /comment/i })));
    };

    await select(0, 6, 20);
    fireEvent.change(view.getAllByPlaceholderText('Add a comment…')[0], { target: { value: 'First draft' } });
    await select(7, 12, 140);
    const inputs = view.getAllByPlaceholderText('Add a comment…') as HTMLTextAreaElement[];
    expect(inputs).toHaveLength(2);
    expect(inputs[0].value).toBe('First draft');
    fireEvent.change(inputs[1], { target: { value: 'Second draft' } });
    expect((view.getAllByPlaceholderText('Add a comment…') as HTMLTextAreaElement[]).map((input) => input.value)).toEqual(['First draft', 'Second draft']);
    expect(JSON.parse(localStorage.getItem('tandem.annotationCommentDrafts') ?? '{}')['session-1']).toHaveLength(2);
  });
});

describe('TranscriptPane thinking blocks', () => {
  function renderThought() {
    const withThought = agent();
    withThought.events = [{ seq: 1, event: { kind: 'thought_chunk', text: 'I should check the mobile interaction.' } }];
    useStore.setState({
      ...initialState,
      sessions: { 'session-1': withThought }, order: ['session-1'], focusedId: 'session-1', annotations: { 'session-1': [] },
    }, true);
    return render(<TranscriptPane />).container.querySelector<HTMLDetailsElement>('details.ev.thought')!;
  }

  it('collapses an expanded thought when its body is tapped on mobile', () => {
    vi.stubGlobal('matchMedia', vi.fn().mockReturnValue({ matches: true }));
    const thought = renderThought();
    thought.open = true;

    fireEvent.click(thought);

    expect(thought.open).toBe(false);
  });

  it('keeps an expanded thought open when its body is clicked on desktop', () => {
    vi.stubGlobal('matchMedia', vi.fn().mockReturnValue({ matches: false }));
    const thought = renderThought();
    thought.open = true;

    fireEvent.click(thought);

    expect(thought.open).toBe(true);
  });
});

describe('TranscriptPane steering', () => {
  it('shows a steering-wheel action beside the other working controls', () => {
    vi.stubGlobal('matchMedia', vi.fn().mockReturnValue({ matches: false }));
    const working = agent();
    working.status = 'working';
    working.steeringSupport = true;
    useStore.setState({
      ...initialState,
      sessions: { 'session-1': working }, order: ['session-1'], focusedId: 'session-1', annotations: { 'session-1': [] },
    }, true);

    const view = render(<TranscriptPane />);
    const steer = view.getByRole('button', { name: 'Steer current turn' });
    const queue = view.getByRole('button', { name: 'Queue' });
    const actions = steer.parentElement!;
    expect(actions).toBe(queue.parentElement);
    expect(actions.classList.contains('working')).toBe(true);
    expect(actions.classList.contains('has-steering')).toBe(true);
    expect(steer.querySelector('svg')).not.toBeNull();
    expect(Array.from(actions.children)).toContain(steer);
    expect(Array.from(actions.children)).toContain(queue);
  });
});
