import { afterEach, describe, expect, it, vi } from 'vitest';
import { __testApplyServerMsg, LOCAL_HOST_ID, notificationsSummary, useStore } from './store';
import type { SessionSummary, Annotation } from './wire';
import { WsClient } from './ws/client';
import {
  __getElementForTests as __getEngineElementForTests,
  __resetForTests as __resetEngineForTests,
  getState as getEngineState,
  pause as enginePause,
  play as enginePlay,
  setPlaylist as setEnginePlaylist,
} from './audio/engine';

// Exercises the store's ServerMsg reducer for transcript annotations
// (docs/transcript-annotations.md): hydration from `snapshot` and wholesale
// replacement on the `annotations` broadcast. __testApplyServerMsg drives the
// same reducer the WsClient feeds messages into at runtime, without a real
// WebSocket (see store.ts).

function annotation(overrides: Partial<Annotation> = {}): Annotation {
  return {
    id: 'a1',
    sessionId: 'session-1',
    seq: 5,
    role: 'assistant',
    quote: 'quoted text',
    comment: 'a comment',
    createdAt: 1,
    updatedAt: 1,
    ...overrides,
  };
}

afterEach(() => {
  useStore.setState({ sessions: {}, order: [], annotations: {}, focusedId: null, pendingSpawns: [], focusedSpawnId: null, pane: 'chat', panesBySession: {}, drafts: {}, systemNotifications: [], hosts: [{ id: LOCAL_HOST_ID, name: 'This host', status: 'connected', local: true }], dirs: [], dirsByHost: {}, agentCatalog: null, agentCatalogByHost: {}, resumeCatalog: null, resumeCatalogByHost: {}, resumePendingHostIds: {}, resumeLoading: false });
  localStorage.removeItem('tandem.agentOrder');
  localStorage.removeItem('tandem.focusedAgent');
  localStorage.removeItem('tandem.agentPanes');
  localStorage.removeItem('tandem.promptDrafts');
});

function summary(overrides: Partial<SessionSummary> = {}): SessionSummary {
  return {
    id: 'session-1', name: 'Agent', workspace: { kind: 'existing', repo: 'repo', repoPath: '/repo', branch: 'main', cwd: '/repo' },
    status: 'idle', pendingApprovals: 0, controlMode: 'transcript', adapter: 'acp', canHandoff: true,
    ...overrides,
  };
}

describe('federation projections', () => {
  it('keeps host-scoped directories/catalogs separate and synthesizes local compatibility', () => {
    __testApplyServerMsg({ t: 'hosts', hosts: [{ id: 'worker-1', name: 'Build host', status: 'connected' }] });
    __testApplyServerMsg({ t: 'dirs', dirs: [{ path: '/remote/repo', name: 'repo', currentBranch: 'main', dirty: false, hasLiveAgent: false }], hostId: 'worker-1' });
    __testApplyServerMsg({ t: 'agent_catalog', hostId: 'worker-1', catalog: { defaultAgent: 'pi', agents: [], harnesses: [] } });
    __testApplyServerMsg({ t: 'dirs', dirs: [{ path: '/local/repo', name: 'local', currentBranch: 'main', dirty: false, hasLiveAgent: false }] });

    const state = useStore.getState();
    expect(state.hosts.map((host) => host.id)).toEqual([LOCAL_HOST_ID, 'worker-1']);
    expect(state.dirs.map((dir) => dir.path)).toEqual(['/local/repo']);
    expect(state.dirsByHost['worker-1'].map((dir) => dir.path)).toEqual(['/remote/repo']);
    expect(state.agentCatalogByHost['worker-1']?.defaultAgent).toBe('pi');
  });

  it('retains remote host labels on normal agent summaries', () => {
    __testApplyServerMsg({ t: 'agents', sessions: [summary({ hostId: 'worker-1', hostName: 'Build host' })] });
    expect(useStore.getState().sessions['session-1']).toMatchObject({ hostId: 'worker-1', hostName: 'Build host' });
  });

  it('keeps session discovery loading until every requested host replies', () => {
    useStore.setState({
      hosts: [{ id: LOCAL_HOST_ID, local: true }, { id: 'worker-1', name: 'Build host', status: 'connected' }],
      resumeLoading: true,
      resumePendingHostIds: { [LOCAL_HOST_ID]: true, 'worker-1': true },
    });
    __testApplyServerMsg({ t: 'sessions', catalog: { sessions: [], adapters: [] } });
    expect(useStore.getState().resumeLoading).toBe(true);
    __testApplyServerMsg({ t: 'sessions', hostId: 'worker-1', catalog: { sessions: [{ externalSessionId: 'remote', source: 'history', agent: 'pi', cwd: '/repo', resumable: true }], adapters: [] } });
    expect(useStore.getState().resumeLoading).toBe(false);
    expect(useStore.getState().resumeCatalog?.sessions[0]).toMatchObject({ hostId: 'worker-1', hostName: 'Build host' });
  });
});

describe('system notifications', () => {
  it('hydrates the daemon snapshot and includes it in the rail badge', () => {
    __testApplyServerMsg({
      t: 'system_notifications',
      notifications: [{ id: 'update', severity: 'attention', title: 'Update available', actions: [{ id: 'install', label: 'Update' }] }],
    });

    expect(useStore.getState().systemNotifications).toHaveLength(1);
    expect(notificationsSummary(useStore.getState())).toEqual({ total: 1, severity: 'attention' });
  });
});

describe('agent ordering', () => {
  it('moves an agent before or after its drop target and asks the daemon to persist it', () => {
    useStore.setState({ order: ['one', 'two', 'three'] });

    useStore.getState().reorderAgent('one', 'three', false);
    expect(useStore.getState().order).toEqual(['two', 'one', 'three']);

    useStore.getState().reorderAgent('three', 'two', true);
    expect(useStore.getState().order).toEqual(['two', 'three', 'one']);
  });

  it('adopts the daemon-reported order', () => {
    useStore.setState({ order: ['a', 'b'] });
    __testApplyServerMsg({ t: 'agents', sessions: [summary({ id: 'b' }), summary({ id: 'c' }), summary({ id: 'a' })] });
    expect(useStore.getState().order).toEqual(['b', 'c', 'a']);
  });
});

describe('thread audio preference', () => {
  it('defaults thread audio to daemon-owned disabled state', () => {
    __testApplyServerMsg({
      t: 'snapshot', sessionId: 'session-1', seq: 0, transcript: [], status: 'idle', controlMode: 'transcript', pendingApprovals: [], queuedPrompts: [],
    });
    __testApplyServerMsg({
      t: 'snapshot', sessionId: 'session-2', seq: 0, transcript: [], status: 'idle', controlMode: 'transcript', pendingApprovals: [], queuedPrompts: [],
    });

    expect(useStore.getState().sessions['session-1'].audioOnTurnEnd).toBe(false);
    expect(useStore.getState().sessions['session-2'].audioOnTurnEnd).toBe(false);
  });

  it('does not notify when a focused agent completes its turn', () => {
    __testApplyServerMsg({
      t: 'snapshot', sessionId: 'session-1', seq: 1, transcript: [], status: 'working', controlMode: 'transcript', pendingApprovals: [], queuedPrompts: [],
    });
    useStore.setState({ focusedId: 'session-1' });

    __testApplyServerMsg({ t: 'event', sessionId: 'session-1', seq: 2, event: { kind: 'status', status: 'idle' } });

    expect(useStore.getState().sessions['session-1'].turnNotifications).toHaveLength(0);
  });

  it('keeps only the latest completed-turn notification for a background agent', () => {
    __testApplyServerMsg({
      t: 'snapshot', sessionId: 'session-2', seq: 0, transcript: [], status: 'idle', controlMode: 'transcript', pendingApprovals: [], queuedPrompts: [],
    });
    __testApplyServerMsg({
      t: 'snapshot', sessionId: 'session-1', seq: 1, transcript: [], status: 'working', controlMode: 'transcript', pendingApprovals: [], queuedPrompts: [],
    });

    __testApplyServerMsg({ t: 'event', sessionId: 'session-1', seq: 2, event: { kind: 'status', status: 'idle' } });
    __testApplyServerMsg({ t: 'event', sessionId: 'session-1', seq: 3, event: { kind: 'status', status: 'working' } });
    __testApplyServerMsg({ t: 'event', sessionId: 'session-1', seq: 4, event: { kind: 'status', status: 'error' } });

    const notifications = useStore.getState().sessions['session-1'].turnNotifications;
    expect(notifications).toHaveLength(1);
    expect(notifications[0]).toMatchObject({ seq: 4, severity: 'failure' });
  });

  it('persists the focused agent', () => {
    __testApplyServerMsg({
      t: 'snapshot', sessionId: 'session-1', seq: 0, transcript: [], status: 'idle', controlMode: 'transcript', pendingApprovals: [], queuedPrompts: [],
    });

    useStore.getState().focus('session-1');

    expect(localStorage.getItem('tandem.focusedSession')).toBe('session-1');
  });

  it('persists focus changes made by keyboard navigation', () => {
    __testApplyServerMsg({
      t: 'snapshot', sessionId: 'session-1', seq: 0, transcript: [], status: 'idle', controlMode: 'transcript', pendingApprovals: [], queuedPrompts: [],
    });
    __testApplyServerMsg({
      t: 'snapshot', sessionId: 'session-2', seq: 0, transcript: [], status: 'idle', controlMode: 'transcript', pendingApprovals: [], queuedPrompts: [],
    });

    useStore.getState().focus('session-1');
    useStore.getState().nav(1);

    expect(useStore.getState().focusedId).toBe('session-2');
    expect(localStorage.getItem('tandem.focusedSession')).toBe('session-2');
  });

  it('persists every unsent prompt draft', () => {
    useStore.getState().setDraft('session-1', 'Keep this prompt after refreshing.');
    useStore.getState().setDraft('session-2', 'And this one too.');

    expect(JSON.parse(localStorage.getItem('tandem.promptDrafts') ?? '{}')).toEqual({
      'session-1': 'Keep this prompt after refreshing.',
      'session-2': 'And this one too.',
    });
  });

  it('restores the selected pane for each focused agent', () => {
    __testApplyServerMsg({
      t: 'snapshot', sessionId: 'session-1', seq: 0, transcript: [], status: 'idle', controlMode: 'transcript', pendingApprovals: [], queuedPrompts: [],
    });
    __testApplyServerMsg({
      t: 'snapshot', sessionId: 'session-2', seq: 0, transcript: [], status: 'idle', controlMode: 'transcript', pendingApprovals: [], queuedPrompts: [],
    });

    useStore.getState().focus('session-1');
    useStore.getState().setPane('diff');
    useStore.getState().focus('session-2');
    expect(useStore.getState().pane).toBe('chat');

    useStore.getState().setPane('shell');
    useStore.getState().focus('session-1');
    expect(useStore.getState().pane).toBe('diff');
    expect(JSON.parse(localStorage.getItem('tandem.sessionPanes') ?? '{}')).toEqual({ 'session-1': 'diff', 'session-2': 'shell' });
  });

  it('hydrates daemon-owned audio preference and ready state from transcript events', () => {
    __testApplyServerMsg({
      t: 'snapshot', sessionId: 'session-1', seq: 3, status: 'idle', controlMode: 'transcript', pendingApprovals: [], queuedPrompts: [],
      transcript: [
        { seq: 1, event: { kind: 'audio_preference', enabled: true } },
        { seq: 2, event: { kind: 'audio_state', state: 'rendering', seq: 9 } },
        { seq: 3, event: { kind: 'audio_state', state: 'ready', seq: 9 } },
      ],
    });
    const agent = useStore.getState().sessions['session-1'];
    expect(agent.audioOnTurnEnd).toBe(true);
    expect(agent.audioState).toBe('ready');
    expect(agent.audioSeq).toBe(9);
  });
});

describe('annotations store reducer', () => {
  it('hydrates annotations from the snapshot message', () => {
    __testApplyServerMsg({
      t: 'snapshot',
      sessionId: 'session-1',
      seq: 5,
      transcript: [],
      status: 'idle',
      controlMode: 'transcript',
      pendingApprovals: [],
      queuedPrompts: [],
      annotations: [annotation()],
    });
    expect(useStore.getState().annotations['session-1']).toEqual([annotation()]);
  });

  it('defaults to an empty list when the snapshot omits annotations', () => {
    __testApplyServerMsg({
      t: 'snapshot',
      sessionId: 'session-2',
      seq: 0,
      transcript: [],
      status: 'idle',
      controlMode: 'transcript',
      pendingApprovals: [],
      queuedPrompts: [],
    });
    expect(useStore.getState().annotations['session-2']).toEqual([]);
  });

  it('replaces the annotation list wholesale on the annotations broadcast', () => {
    __testApplyServerMsg({
      t: 'snapshot',
      sessionId: 'session-1',
      seq: 5,
      transcript: [],
      status: 'idle',
      controlMode: 'transcript',
      pendingApprovals: [],
      queuedPrompts: [],
      annotations: [annotation(), annotation({ id: 'a2' })],
    });

    __testApplyServerMsg({
      t: 'annotations',
      sessionId: 'session-1',
      annotations: [annotation({ id: 'a2', comment: 'edited' })],
    });

    expect(useStore.getState().annotations['session-1']).toEqual([annotation({ id: 'a2', comment: 'edited' })]);
  });

  it('only replaces annotations for the targeted agent, keyed by sessionId', () => {
    __testApplyServerMsg({ t: 'annotations', sessionId: 'session-1', annotations: [annotation()] });
    __testApplyServerMsg({ t: 'annotations', sessionId: 'session-2', annotations: [] });

    expect(useStore.getState().annotations['session-1']).toEqual([annotation()]);
    expect(useStore.getState().annotations['session-2']).toEqual([]);
  });
});

// The context-usage meter is daemon-owned: usage events carry the timestamp of
// the last real change and live in the event log, so a fresh page load rebuilds
// the meter (and its age) from the snapshot rather than browser storage.
describe('context usage', () => {
  it('rebuilds usage from the replayed transcript with the daemon timestamp', () => {
    __testApplyServerMsg({
      t: 'snapshot',
      sessionId: 'session-1',
      seq: 2,
      transcript: [
        { seq: 1, event: { kind: 'usage', used: 100, size: 1000, updatedAt: 1_700_000_000_000 } },
        { seq: 2, event: { kind: 'usage', used: 250, size: 1000, cost: { amount: 0.5, currency: 'USD' }, updatedAt: 1_700_000_060_000 } },
      ],
      status: 'idle',
      controlMode: 'transcript',
      pendingApprovals: [],
      queuedPrompts: [],
    });

    expect(useStore.getState().sessions['session-1'].usage).toEqual({
      used: 250,
      size: 1000,
      cost: { amount: 0.5, currency: 'USD' },
      updatedAt: 1_700_000_060_000,
    });
  });

  it('keeps the daemon timestamp for live usage events instead of arrival time', () => {
    __testApplyServerMsg({
      t: 'snapshot', sessionId: 'session-1', seq: 0, transcript: [], status: 'idle', controlMode: 'transcript', pendingApprovals: [], queuedPrompts: [],
    });
    __testApplyServerMsg({
      t: 'event', sessionId: 'session-1', seq: 1, event: { kind: 'usage', used: 42, size: 1000, updatedAt: 1_700_000_000_000 },
    });

    expect(useStore.getState().sessions['session-1'].usage?.updatedAt).toBe(1_700_000_000_000);
  });
});

// The screen-off listening fix: syncAudioFocus (store.ts) must retain audio
// focus on a hidden document while the audio engine is actually playing or
// armed mid-section, and only fall back to "no focus" once the engine is
// genuinely idle. Drives the engine for real (setPlaylist/play/pause) rather
// than faking EngineState, and spies on WsClient.prototype.send (a real
// instance already backs the store's `client` — see store.ts) to observe the
// set_audio_focus messages it sends, since the client never opens a real
// socket in this test environment (send() just queues silently otherwise).
describe('audio focus retention on a hidden document', () => {
  afterEach(() => {
    __resetEngineForTests();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
    Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => 'visible' });
  });

  it('keeps focus pinned on the listening chat while hidden, and drops it once the engine is genuinely idle', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(new Blob(['audio'], { type: 'audio/mpeg' }), { status: 200 })));
    vi.spyOn(HTMLMediaElement.prototype, 'play').mockResolvedValue(undefined);
    vi.spyOn(HTMLMediaElement.prototype, 'pause').mockImplementation(() => {});

    __testApplyServerMsg({
      t: 'snapshot', sessionId: 'session-1', seq: 0, transcript: [], status: 'idle', controlMode: 'transcript', pendingApprovals: [], queuedPrompts: [],
    });
    useStore.setState({ order: ['session-1'], pane: 'chat', focusedId: null });

    // Prime the store's internal audioFocusAgent tracking to a known (null)
    // baseline first: syncAudioFocus short-circuits when nothing changed, so
    // asserting on the very first call would be at the mercy of whatever
    // earlier tests in this file left it pointing at.
    Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => 'hidden' });
    useStore.getState().focus('session-1'); // hidden + idle engine -> null baseline

    const sendSpy = vi.spyOn(WsClient.prototype, 'send').mockImplementation(() => {});

    setEnginePlaylist('session-1', [1]);
    enginePlay('session-1', 1);
    await vi.waitFor(() => expect(getEngineState().status).toBe('playing'));

    useStore.getState().focus('session-1'); // still hidden, now actually listening

    expect(sendSpy).toHaveBeenLastCalledWith({ t: 'set_audio_focus', sessionId: 'session-1', focused: true });

    // Paused mid-section (not an idle/never-started engine) — still counts
    // as listening, so focus must stay pinned.
    enginePause();
    __getEngineElementForTests()!.dispatchEvent(new Event('pause'));
    sendSpy.mockClear();
    useStore.getState().focus('session-1');
    expect(sendSpy).not.toHaveBeenCalled(); // already pinned to session-1; nothing changed

    // Now the engine is genuinely torn down (e.g. the chat closed) — a
    // hidden document with nothing playing/armed must not keep pre-rendering
    // pinned forever.
    setEnginePlaylist('session-1', []);
    useStore.getState().focus('session-1');
    expect(sendSpy).toHaveBeenLastCalledWith({ t: 'set_audio_focus', sessionId: 'session-1', focused: false });
  });

  it('still uses the plain visible/chat-pane rule when not hidden', () => {
    __testApplyServerMsg({
      t: 'snapshot', sessionId: 'session-1', seq: 0, transcript: [], status: 'idle', controlMode: 'transcript', pendingApprovals: [], queuedPrompts: [],
    });
    useStore.setState({ order: ['session-1'], pane: 'chat', focusedId: null });

    // Prime to a known (null) baseline first — see note in the previous test.
    Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => 'hidden' });
    useStore.getState().focus('session-1');

    const sendSpy = vi.spyOn(WsClient.prototype, 'send').mockImplementation(() => {});
    Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => 'visible' });

    useStore.getState().focus('session-1');

    expect(sendSpy).toHaveBeenLastCalledWith({ t: 'set_audio_focus', sessionId: 'session-1', focused: true });
  });
});

describe('pending spawns', () => {
  const spec = { agent: 'claude', adapter: 'acp' as const, workspace: { kind: 'worktree' as const, repo: '/repo/demo' } };
  function sentSpawnCorr(sendSpy: ReturnType<typeof vi.spyOn>): string {
    const msg = sendSpy.mock.calls.map((c: unknown[]) => c[0] as { t: string; corrId?: string }).find((m: { t: string }) => m.t === 'spawn_agent');
    return msg!.corrId!;
  }

  it('shows progress, then focuses the new session when the user is still watching', async () => {
    const sendSpy = vi.spyOn(WsClient.prototype, 'send').mockImplementation(() => {});
    const done = useStore.getState().spawn(spec);
    const corrId = sentSpawnCorr(sendSpy);
    expect(useStore.getState().focusedSpawnId).toBe(corrId);
    expect(useStore.getState().pendingSpawns[0]).toMatchObject({ label: 'claude · demo', error: null });

    __testApplyServerMsg({ t: 'spawn_progress', corrId, phase: 'Launching ACP adapter process…' });
    expect(useStore.getState().pendingSpawns[0].phase).toBe('Launching ACP adapter process…');
    // A summary refresh mid-spawn must not steal focus from the progress view.
    __testApplyServerMsg({ t: 'agents', sessions: [summary()] });
    expect(useStore.getState().focusedId).toBeNull();

    __testApplyServerMsg({ t: 'ack', corrId, sessionId: 'new-1' });
    await done;
    expect(useStore.getState()).toMatchObject({ focusedId: 'new-1', focusedSpawnId: null, pendingSpawns: [] });
    sendSpy.mockRestore();
  });

  it('keeps a failed spawn in the rail after the user moved elsewhere', async () => {
    const sendSpy = vi.spyOn(WsClient.prototype, 'send').mockImplementation(() => {});
    __testApplyServerMsg({ t: 'agents', sessions: [summary()] });
    const done = useStore.getState().spawn(spec);
    const corrId = sentSpawnCorr(sendSpy);
    useStore.getState().focus('session-1');
    expect(useStore.getState().focusedSpawnId).toBeNull();

    __testApplyServerMsg({ t: 'ack', corrId, error: 'boom' });
    await done;
    expect(useStore.getState().focusedId).toBe('session-1');
    expect(useStore.getState().pendingSpawns).toMatchObject([{ corrId, error: 'boom' }]);

    useStore.getState().focusSpawn(corrId);
    expect(useStore.getState()).toMatchObject({ focusedId: null, focusedSpawnId: corrId });
    useStore.getState().dismissPendingSpawn(corrId);
    expect(useStore.getState()).toMatchObject({ pendingSpawns: [], focusedSpawnId: null });
    sendSpy.mockRestore();
  });
});
