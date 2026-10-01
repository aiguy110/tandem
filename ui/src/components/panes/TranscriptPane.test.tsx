import { act, cleanup, fireEvent, render, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { useStore } from '../../store';
import type { AgentEnvelope } from '../../wire';
import type { SessionView } from '../../store';
import { findAgentToken, findFileToken, findSlashToken, TranscriptPane } from './TranscriptPane';
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
    waitingOn: [],
    openAsks: 0,
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
  vi.unstubAllGlobals();
  localStorage.clear();
  delete (URL as unknown as Record<string, unknown>).createObjectURL;
  delete (URL as unknown as Record<string, unknown>).revokeObjectURL;
  delete (navigator as unknown as Record<string, unknown>).mediaSession;
});

describe('TranscriptPane rate-limit widget', () => {
  it('renders daemon state and sends toggle changes back to the daemon', () => {
    const limited = agent();
    limited.events = [{ seq: 2, event: { kind: 'rate_limit', id: 'claude-1', harness: 'claude', resetAt: Date.now() + 3_600_000, detectedAt: Date.now(), enabled: false, state: 'pending' } }];
    const setRateLimitAutoContinue = vi.fn().mockResolvedValue({ sessionId: 'session-1' });
    useStore.setState({
      ...initialState,
      sessions: { 'session-1': limited }, order: ['session-1'], focusedId: 'session-1', annotations: { 'session-1': [] },
      setRateLimitAutoContinue,
    }, true);

    const view = render(<TranscriptPane />);
    expect(view.getByText('Usage limit reached')).toBeTruthy();
    fireEvent.click(view.getByRole('switch'));
    expect(setRateLimitAutoContinue).toHaveBeenCalledWith('session-1', true);
  });
});

describe('PromptBar attachments', () => {
  it('preserves a pasted image and its upload across session switches', async () => {
    const first = agent();
    first.imagePromptSupport = true;
    const second = { ...agent(), id: 'session-2', name: 'Other session', imagePromptSupport: true };
    useStore.setState({
      ...initialState,
      sessions: { 'session-1': first, 'session-2': second },
      order: ['session-1', 'session-2'],
      focusedId: 'session-1',
      annotations: { 'session-1': [], 'session-2': [] },
    }, true);

    (URL as typeof URL & { createObjectURL: (blob: Blob) => string }).createObjectURL = vi.fn().mockReturnValue('blob:pasted-image');
    (URL as typeof URL & { revokeObjectURL: (url: string) => void }).revokeObjectURL = vi.fn();

    class PendingUpload {
      static current: PendingUpload | null = null;
      upload: { onprogress: ((event: { lengthComputable: boolean; loaded: number; total: number }) => void) | null } = { onprogress: null };
      status = 201;
      responseText = JSON.stringify({ asset: { assetId: 'asset-1', mimeType: 'image/png', name: 'paste.png' } });
      onload: (() => void) | null = null;
      onerror: (() => void) | null = null;
      onabort: (() => void) | null = null;
      open() {}
      setRequestHeader() {}
      send() { PendingUpload.current = this; }
      abort() { this.onabort?.(); }
    }
    vi.stubGlobal('XMLHttpRequest', PendingUpload);

    const view = render(<TranscriptPane />);
    const image = new File(['pixels'], 'paste.png', { type: 'image/png' });
    fireEvent.paste(view.getByPlaceholderText(/Prompt Mobile test/), { clipboardData: { files: [image] } });
    expect(view.getByText(/Uploading/)).toBeTruthy();

    act(() => useStore.getState().focus('session-2'));
    expect(view.queryByAltText('')).toBeNull();
    act(() => useStore.getState().focus('session-1'));
    expect(view.getByAltText('').getAttribute('src')).toBe('blob:pasted-image');
    expect(view.getByText(/Uploading/)).toBeTruthy();

    act(() => PendingUpload.current?.onload?.());
    await waitFor(() => expect(view.getByText('Ready')).toBeTruthy());
    expect(useStore.getState().draftAttachments['session-1']?.[0]?.asset?.assetId).toBe('asset-1');
  });
});

describe('TranscriptPane agent messages', () => {
  const envelope = (overrides: Partial<AgentEnvelope> = {}): AgentEnvelope => ({
    id: 'msg_1', threadId: 'thr_1', kind: 'ask', requestId: 'req_1',
    from: { host: 'node-b', agent: 'sess-b', name: 'api-worker' },
    to: { host: 'node-a', agent: 'session-1', name: 'Mobile test' },
    body: 'Which **port** does the API use?', hop: 0, sentAt: '2026-01-01T00:00:00Z', ...overrides,
  });

  it('renders an inbound message as a card, not a user prompt', () => {
    const view = agent();
    view.events = [{ seq: 1, event: { kind: 'agent_message', direction: 'in', envelope: envelope(), status: 'steered' } }];
    useStore.setState({
      ...initialState,
      hosts: [{ id: 'local', local: true, status: 'connected', nodeId: 'node-a' }, { id: 'b', name: 'Builder', status: 'connected', nodeId: 'node-b' }],
      sessions: { 'session-1': view }, order: ['session-1'], focusedId: 'session-1', annotations: { 'session-1': [] },
    }, true);

    const { container } = render(<TranscriptPane />);
    const card = container.querySelector('[data-envelope-id="msg_1"]') as HTMLElement;
    expect(card.textContent).toContain('✉ from');
    expect(card.textContent).toContain('@agent:Builder/api-worker');
    expect(card.textContent).toContain('ask');
    expect(card.textContent).toContain('steered');
    expect(card.querySelector('strong')?.textContent).toBe('port');
    expect(container.querySelector('.ev.user')).toBeNull();
  });

  it('renders an outbound message and patches its status by envelope id', () => {
    const view = agent();
    view.events = [
      { seq: 1, event: { kind: 'agent_message', direction: 'out', envelope: envelope({ kind: 'send', from: { host: 'node-a', agent: 'session-1' }, to: { host: 'node-b', agent: 'sess-b', name: 'api-worker' } }), status: 'pending' } },
    ];
    useStore.setState({ ...initialState, sessions: { 'session-1': view }, order: ['session-1'], focusedId: 'session-1', annotations: { 'session-1': [] } }, true);

    const { container } = render(<TranscriptPane />);
    const card = () => container.querySelector('[data-envelope-id="msg_1"]') as HTMLElement;
    expect(card().textContent).toContain('→');
    expect(card().querySelector('.agent-msg-status')?.textContent).toBe('pending');

    const patched = { ...view, events: [...view.events, { seq: 2, event: { kind: 'agent_message_status', id: 'msg_1', status: 'rejected', error: 'no_link' } as const }] };
    act(() => useStore.setState({ sessions: { 'session-1': patched } }));
    expect(container.querySelectorAll('[data-envelope-id]').length).toBe(1);
    expect(card().querySelector('.agent-msg-status')?.textContent).toBe('rejected · no_link');
  });

  it('shows the system event and opens the peer agent when its name is clicked', () => {
    const view = agent();
    view.events = [{ seq: 1, event: { kind: 'agent_message', direction: 'in', envelope: envelope({ kind: 'system', system: { event: 'timeout' }, body: '' }), status: 'started' } }];
    const peer = { ...agent(), id: 'fed~b~sess-b', name: 'api-worker', hostId: 'b' };
    const focus = vi.fn();
    useStore.setState({
      ...initialState,
      hosts: [{ id: 'local', local: true, status: 'connected', nodeId: 'node-a' }, { id: 'b', name: 'Builder', status: 'connected', nodeId: 'node-b' }],
      sessions: { 'session-1': view, [peer.id]: peer }, order: ['session-1', peer.id], focusedId: 'session-1', annotations: { 'session-1': [] },
      focus,
    }, true);

    const { container, getByText } = render(<TranscriptPane />);
    expect(container.querySelector('.agent-msg-kind')?.textContent).toBe('system · timeout');
    fireEvent.click(getByText('@agent:Builder/api-worker'));
    expect(focus).toHaveBeenCalledWith('fed~b~sess-b');
  });
});

describe('TranscriptPane agent message navigation', () => {
  it('scrolls the peer transcript to the card with the same envelope id', () => {
    const scrollIntoView = vi.fn();
    Element.prototype.scrollIntoView = scrollIntoView;
    const sent = { ...agent(), events: [{ seq: 1, event: { kind: 'agent_message', direction: 'out', status: 'started', envelope: {
      id: 'msg_9', threadId: 'thr_9', kind: 'send', from: { host: 'local', agent: 'session-1' }, to: { host: 'b', agent: 'sess-b', name: 'api-worker' }, body: 'hi', hop: 0, sentAt: '2026-01-01T00:00:00Z',
    } } as const }] };
    const received = { ...agent(), id: 'fed~b~sess-b', name: 'api-worker', hostId: 'b', events: [{ seq: 1, event: { kind: 'agent_message', direction: 'in', status: 'started', envelope: {
      id: 'msg_9', threadId: 'thr_9', kind: 'send', from: { host: 'local', agent: 'session-1' }, to: { host: 'b', agent: 'sess-b' }, body: 'hi', hop: 0, sentAt: '2026-01-01T00:00:00Z',
    } } as const }] };
    useStore.setState({
      ...initialState,
      hosts: [{ id: 'local', local: true, status: 'connected' }, { id: 'b', name: 'Builder', status: 'connected' }],
      sessions: { 'session-1': sent, [received.id]: received }, order: ['session-1', received.id], focusedId: 'session-1',
      annotations: { 'session-1': [], [received.id]: [] },
    }, true);

    const { container, getByText } = render(<TranscriptPane />);
    fireEvent.click(getByText('@agent:Builder/api-worker'));
    expect(useStore.getState().focusedId).toBe('fed~b~sess-b');
    expect(scrollIntoView).toHaveBeenCalled();
    expect((scrollIntoView.mock.contexts.at(-1) as HTMLElement).dataset.envelopeId).toBe('msg_9');
    expect(container.querySelector('.agent-msg.in')).toBeTruthy();
    delete (Element.prototype as unknown as Record<string, unknown>).scrollIntoView;
  });
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

  it('patches one compaction card in place and shows its retained summary', () => {
    const withCompaction = agent();
    withCompaction.events = [
      { seq: 1, event: { kind: 'compaction', id: 'c1', status: 'running' } },
      { seq: 2, event: { kind: 'compaction_summary_chunk', id: 'c1', text: 'Partial' } },
      { seq: 3, event: { kind: 'compaction', id: 'c1', status: 'done', summary: 'Fixed the **share** menu.', trigger: 'manual', preTokens: 180000, postTokens: 12000 } },
    ];
    withCompaction.lastSeq = 3;
    useStore.setState({
      ...initialState,
      sessions: { 'session-1': withCompaction }, order: ['session-1'], focusedId: 'session-1', annotations: { 'session-1': [] },
    }, true);

    const view = render(<TranscriptPane />);
    expect(view.container.querySelectorAll('.compaction-card')).toHaveLength(1);
    expect(view.container.querySelector('.compaction-card .title')?.textContent).toBe('Compacted conversation · 180k → 12k tokens');
    fireEvent.click(view.container.querySelector('.compaction-card .card-head')!);
    expect(view.getByText('Retained summary')).toBeTruthy();
    expect(view.container.querySelector('.compaction-card strong')?.textContent).toBe('share');
    expect(view.queryByText(/Partial/)).toBeNull();
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
    expect(findFileToken('@~/Downloads/State of Israel', 28)).toMatchObject({ start: 0, query: '~/Downloads/State of Israel' });
    expect(findFileToken('@"~/Downloads/State of Israel"', 30)).toMatchObject({ start: 0, query: '~/Downloads/State of Israel' });
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

  it('renders sent user messages as markdown without highlighting mentions in code', () => {
    const withCommand = agent();
    withCommand.commands = [{ name: 'help', description: 'Show help' }];
    withCommand.events = [{ seq: 1, event: { kind: 'user_message', text: 'Use the **agent** user, then /help.\n```\ncurl /help\n```' } }];
    useStore.setState({
      ...initialState,
      sessions: { 'session-1': withCommand }, order: ['session-1'], focusedId: 'session-1', annotations: { 'session-1': [] },
    }, true);

    const view = render(<TranscriptPane />);
    const user = view.container.querySelector('.ev.user');
    expect(user?.querySelector('strong')?.textContent).toBe('agent');
    expect(user?.querySelector('pre code')?.textContent).toBe('curl /help\n');
    expect(user?.querySelectorAll('.skill-mention')).toHaveLength(1);
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

  it('keeps a trailing newline line box in the composer highlight layer', () => {
    useStore.setState({
      ...initialState,
      sessions: { 'session-1': agent() }, order: ['session-1'], focusedId: 'session-1', annotations: { 'session-1': [] },
      drafts: { 'session-1': 'first line\n' },
    }, true);

    const view = render(<TranscriptPane />);
    expect(view.container.querySelector('.prompt-text-highlight')?.textContent).toBe('first line\n\u200b');
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
    withCommand.events = [{ seq: 1, event: { kind: 'user_message', text: 'Read @"docs/State of Israel.md" then /help.' } }];
    useStore.setState({
      ...initialState,
      sessions: { 'session-1': withCommand }, order: ['session-1'], focusedId: 'session-1', annotations: { 'session-1': [] },
      drafts: { 'session-1': 'Read @"docs/State of Israel.md" then /help.' },
    }, true);

    const view = render(<TranscriptPane />);
    expect(view.container.querySelectorAll('.skill-mention')).toHaveLength(4);
    expect(Array.from(view.container.querySelectorAll('.skill-mention')).map((node) => node.textContent))
      .toEqual(['@"docs/State of Israel.md"', '/help', '@"docs/State of Israel.md"', '/help']);
  });

  it('highlights a whole @agent: reference as one mention', () => {
    const withMention = agent();
    withMention.events = [{ seq: 1, event: { kind: 'user_message', text: 'Ask @agent:bifrost/slow-drag. Thanks' } }];
    useStore.setState({
      ...initialState,
      sessions: { 'session-1': withMention }, order: ['session-1'], focusedId: 'session-1', annotations: { 'session-1': [] },
      drafts: { 'session-1': 'Ask @agent:bifrost/slow-drag. Read @src/a.ts' },
    }, true);

    const view = render(<TranscriptPane />);
    const highlight = view.container.querySelector('.prompt-text-highlight');
    expect(Array.from(highlight?.querySelectorAll('.mention-agent') ?? []).map((n) => n.textContent)).toEqual(['@agent:bifrost/slow-drag']);
    expect(Array.from(highlight?.querySelectorAll('.skill-mention') ?? []).map((n) => n.textContent)).toEqual(['@src/a.ts']);
    expect(Array.from(view.container.querySelectorAll('.ev.user .mention-agent')).map((n) => n.textContent)).toEqual(['@agent:bifrost/slow-drag']);
  });

  it('recognizes @agent: tokens at a mention boundary', () => {
    expect(findAgentToken('@agent:', 7)).toMatchObject({ start: 0, end: 7, query: '' });
    expect(findAgentToken('ask @agent:bif/sl', 17)).toMatchObject({ start: 4, query: 'bif/sl' });
    expect(findAgentToken('@agent:bif/sl more', 13)).toMatchObject({ start: 0, query: 'bif/sl' });
    expect(findAgentToken('x@agent:bif', 11)).toBeNull();
    expect(findAgentToken('@agent', 6)).toBeNull();
    expect(findAgentToken('@agent:a b', 10)).toBeNull();
    // The file token never claims an agent token.
    expect(findFileToken('@agent:sl', 9)).toBeNull();
  });

  const agentComposerState = (draft: string) => {
    const self = agent();
    const remote = { ...agent(), id: 'fed~b~sess-b', name: 'slow-drag', hostId: 'b', status: 'working' as const };
    const hidden = { ...agent(), id: 'fed~b~sess-c', name: 'slow-secret', hostId: 'b', unlisted: true };
    useStore.setState({
      ...initialState,
      hosts: [{ id: 'local', local: true, status: 'connected', nodeId: 'node-a' }, { id: 'b', name: 'bifrost', status: 'connected', nodeId: 'node-b' }],
      sessions: { 'session-1': self, [remote.id]: remote, [hidden.id]: hidden }, order: ['session-1', remote.id, hidden.id],
      focusedId: 'session-1', annotations: { 'session-1': [] }, drafts: { 'session-1': draft },
      listWorkspaceEntries: vi.fn().mockResolvedValue([]),
    }, true);
  };

  it('completes @agent: mentions from listed agents on every host', () => {
    agentComposerState('');
    const view = render(<TranscriptPane />);
    const composer = view.getByPlaceholderText(/Prompt Mobile test/i) as HTMLTextAreaElement;
    fireEvent.change(composer, { target: { value: '@agent:sl', selectionStart: 9 } });

    expect(view.getByText('@slow-drag')).toBeTruthy();
    expect(view.queryByText('@slow-secret')).toBeNull();
    expect(view.queryByText('@Mobile test')).toBeNull();
    fireEvent.keyDown(composer, { key: 'Enter' });
    expect(composer.value).toBe('@agent:bifrost/slow-drag ');
  });

  it('offers an agent: hint row for @ that switches to agent completion', () => {
    agentComposerState('');
    const view = render(<TranscriptPane />);
    const composer = view.getByPlaceholderText(/Prompt Mobile test/i) as HTMLTextAreaElement;
    fireEvent.change(composer, { target: { value: '@a', selectionStart: 2 } });

    const hint = view.getByText('agent:');
    expect(view.getByText('mention another agent')).toBeTruthy();
    fireEvent.mouseDown(hint);
    expect(composer.value).toBe('@agent:');
    expect(view.getByText('@slow-drag')).toBeTruthy();
    expect(view.queryByText('agent:')).toBeNull();

    fireEvent.change(composer, { target: { value: '@src', selectionStart: 4 } });
    expect(view.queryByText('agent:')).toBeNull();
  });

  it('omits the agent: hint when there are no other listed agents', () => {
    useStore.setState({
      ...initialState, sessions: { 'session-1': agent() }, order: ['session-1'], focusedId: 'session-1',
      annotations: { 'session-1': [] }, listWorkspaceEntries: vi.fn().mockResolvedValue([]),
    }, true);
    const view = render(<TranscriptPane />);
    fireEvent.change(view.getByPlaceholderText(/Prompt Mobile test/i), { target: { value: '@', selectionStart: 1 } });
    expect(view.queryByText('agent:')).toBeNull();
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

  it('quotes selected file mentions whose paths contain spaces', async () => {
    const listWorkspaceEntries = vi.fn().mockResolvedValue([{
      path: '~/Downloads/State of Israel - Ministry of Finance.odt', isDir: false,
    }]);
    useStore.setState({
      ...initialState,
      sessions: { 'session-1': agent() }, order: ['session-1'], focusedId: 'session-1', annotations: { 'session-1': [] }, listWorkspaceEntries,
    }, true);
    const view = render(<TranscriptPane />);
    const composer = view.getByPlaceholderText(/Prompt Mobile test/i) as HTMLTextAreaElement;
    fireEvent.change(composer, { target: { value: '@~/Downloads/State', selectionStart: 18 } });

    const option = await waitFor(() => view.getByText('@~/Downloads/State of Israel - Ministry of Finance.odt'));
    fireEvent.mouseDown(option);
    expect(composer.value).toBe('@"~/Downloads/State of Israel - Ministry of Finance.odt" ');
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

  it('keeps a native selection anchored when a row already has a highlighted annotation', async () => {
    vi.stubGlobal('matchMedia', vi.fn().mockReturnValue({ matches: true }));
    useStore.setState({
      ...initialState,
      sessions: { 'session-1': agent() },
      order: ['session-1'],
      focusedId: 'session-1',
      annotations: { 'session-1': [{ id: 'a1', sessionId: 'session-1', seq: 1, role: 'assistant', quote: 'Select', comment: 'x', createdAt: 0, updatedAt: 0 }] },
    }, true);
    const view = render(<TranscriptPane />);
    const highlight = view.container.querySelector('.ev.msg .annotation-quote-highlight');
    expect(highlight?.textContent).toBe('Select');
    const text = highlight!.nextSibling as Text;
    expect(text).toBeInstanceOf(Text);

    const range = document.createRange();
    range.setStart(text, 1);
    range.setEnd(text, 6);
    Object.defineProperty(range, 'getBoundingClientRect', {
      value: () => ({ top: 100, left: 20, width: 140, height: 20, right: 160, bottom: 120, x: 20, y: 100, toJSON: () => ({}) }),
    });
    const selection = window.getSelection()!;
    selection.removeAllRanges();
    selection.addRange(range);
    document.dispatchEvent(new Event('selectionchange'));

    await waitFor(() => view.getByRole('button', { name: /comment/i }));
    expect(text.isConnected).toBe(true);
    expect(selection.anchorNode).toBe(text);
    expect(selection.toString()).toBe('these');
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

describe('TranscriptPane message forking', () => {
  beforeEach(() => {
    vi.stubGlobal('matchMedia', vi.fn().mockReturnValue({ matches: false }));
  });

  function forkable(asideSupport: boolean | null) {
    const view = agent();
    view.asideSupport = asideSupport;
    view.events = [
      { seq: 1, event: { kind: 'user_message', blocks: [{ type: 'text', text: '## Tandem session\n\nguidance\n\n## User task\n\nfix the bug' }] } },
      { seq: 2, event: { kind: 'message_chunk', text: 'Done.' } },
    ];
    return view;
  }

  it('edits a user message into a forked session', async () => {
    const forkSession = vi.fn().mockResolvedValue({ sessionId: 'session-1-fork' });
    useStore.setState({
      ...initialState,
      sessions: { 'session-1': forkable(true) }, order: ['session-1'], focusedId: 'session-1', annotations: { 'session-1': [] },
      forkSession,
    }, true);
    const view = render(<TranscriptPane />);

    fireEvent.contextMenu(view.container.querySelector('.ev.user')!);
    fireEvent.click(view.getByRole('menuitem', { name: 'Edit…' }));
    const textarea = view.container.querySelector('.user-edit textarea') as HTMLTextAreaElement;
    // The daemon's first-turn framing is not part of what the user typed.
    expect(textarea.value).toBe('fix the bug');
    fireEvent.change(textarea, { target: { value: 'fix the other bug' } });
    fireEvent.click(within(view.container.querySelector('.user-edit') as HTMLElement).getByRole('button', { name: 'Send' }));

    await waitFor(() => expect(forkSession).toHaveBeenCalledWith('session-1', { seq: 1, edit: [{ type: 'text', text: 'fix the other bug' }] }));
  });

  it('forks after a user message turn', async () => {
    const forkSession = vi.fn().mockResolvedValue({ sessionId: 'session-1-fork' });
    useStore.setState({
      ...initialState,
      sessions: { 'session-1': forkable(true) }, order: ['session-1'], focusedId: 'session-1', annotations: { 'session-1': [] },
      forkSession,
    }, true);
    const view = render(<TranscriptPane />);

    fireEvent.contextMenu(view.container.querySelector('.ev.user')!);
    fireEvent.click(view.getByRole('menuitem', { name: 'Fork from here' }));

    await waitFor(() => expect(forkSession).toHaveBeenCalledWith('session-1', { seq: 1, edit: undefined }));
  });

  it('offers no message menu when the agent cannot fork', () => {
    useStore.setState({
      ...initialState,
      sessions: { 'session-1': forkable(false) }, order: ['session-1'], focusedId: 'session-1', annotations: { 'session-1': [] },
    }, true);
    const view = render(<TranscriptPane />);

    fireEvent.contextMenu(view.container.querySelector('.ev.user')!);
    expect(view.queryByRole('menu')).toBeNull();
  });
});
