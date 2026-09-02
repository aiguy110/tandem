import { afterEach, describe, expect, it } from 'vitest';
import { __testApplyServerMsg, useStore } from './store';
import type { Annotation } from './wire';

// Exercises the store's ServerMsg reducer for transcript annotations
// (docs/transcript-annotations.md): hydration from `snapshot` and wholesale
// replacement on the `annotations` broadcast. __testApplyServerMsg drives the
// same reducer the WsClient feeds messages into at runtime, without a real
// WebSocket (see store.ts).

function annotation(overrides: Partial<Annotation> = {}): Annotation {
  return {
    id: 'a1',
    agentId: 'agent-1',
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
  useStore.setState({ agents: {}, order: [], annotations: {}, focusedId: null });
  localStorage.removeItem('tandem.agentOrder');
  localStorage.removeItem('tandem.focusedAgent');
});

describe('agent ordering', () => {
  it('moves an agent before or after its drop target and saves the result', () => {
    useStore.setState({ order: ['one', 'two', 'three'] });

    useStore.getState().reorderAgent('one', 'three', false);
    expect(useStore.getState().order).toEqual(['two', 'one', 'three']);

    useStore.getState().reorderAgent('three', 'two', true);
    expect(useStore.getState().order).toEqual(['two', 'three', 'one']);
    expect(JSON.parse(localStorage.getItem('tandem.agentOrder') ?? '[]')).toEqual(['two', 'three', 'one']);
  });
});

describe('thread audio preference', () => {
  it('defaults thread audio to daemon-owned disabled state', () => {
    __testApplyServerMsg({
      t: 'snapshot', agentId: 'agent-1', seq: 0, transcript: [], status: 'idle', controlMode: 'transcript', pendingApprovals: [], queuedPrompts: [],
    });
    __testApplyServerMsg({
      t: 'snapshot', agentId: 'agent-2', seq: 0, transcript: [], status: 'idle', controlMode: 'transcript', pendingApprovals: [], queuedPrompts: [],
    });

    expect(useStore.getState().agents['agent-1'].audioOnTurnEnd).toBe(false);
    expect(useStore.getState().agents['agent-2'].audioOnTurnEnd).toBe(false);
  });

  it('does not notify when a focused agent completes its turn', () => {
    __testApplyServerMsg({
      t: 'snapshot', agentId: 'agent-1', seq: 1, transcript: [], status: 'working', controlMode: 'transcript', pendingApprovals: [], queuedPrompts: [],
    });
    useStore.setState({ focusedId: 'agent-1' });

    __testApplyServerMsg({ t: 'event', agentId: 'agent-1', seq: 2, event: { kind: 'status', status: 'idle' } });

    expect(useStore.getState().agents['agent-1'].turnNotifications).toHaveLength(0);
  });

  it('keeps only the latest completed-turn notification for a background agent', () => {
    __testApplyServerMsg({
      t: 'snapshot', agentId: 'agent-2', seq: 0, transcript: [], status: 'idle', controlMode: 'transcript', pendingApprovals: [], queuedPrompts: [],
    });
    __testApplyServerMsg({
      t: 'snapshot', agentId: 'agent-1', seq: 1, transcript: [], status: 'working', controlMode: 'transcript', pendingApprovals: [], queuedPrompts: [],
    });

    __testApplyServerMsg({ t: 'event', agentId: 'agent-1', seq: 2, event: { kind: 'status', status: 'idle' } });
    __testApplyServerMsg({ t: 'event', agentId: 'agent-1', seq: 3, event: { kind: 'status', status: 'working' } });
    __testApplyServerMsg({ t: 'event', agentId: 'agent-1', seq: 4, event: { kind: 'status', status: 'error' } });

    const notifications = useStore.getState().agents['agent-1'].turnNotifications;
    expect(notifications).toHaveLength(1);
    expect(notifications[0]).toMatchObject({ seq: 4, severity: 'failure' });
  });

  it('persists the focused agent', () => {
    __testApplyServerMsg({
      t: 'snapshot', agentId: 'agent-1', seq: 0, transcript: [], status: 'idle', controlMode: 'transcript', pendingApprovals: [], queuedPrompts: [],
    });

    useStore.getState().focus('agent-1');

    expect(localStorage.getItem('tandem.focusedAgent')).toBe('agent-1');
  });

  it('hydrates daemon-owned audio preference and ready state from transcript events', () => {
    __testApplyServerMsg({
      t: 'snapshot', agentId: 'agent-1', seq: 3, status: 'idle', controlMode: 'transcript', pendingApprovals: [], queuedPrompts: [],
      transcript: [
        { seq: 1, event: { kind: 'audio_preference', enabled: true } },
        { seq: 2, event: { kind: 'audio_state', state: 'rendering', seq: 9 } },
        { seq: 3, event: { kind: 'audio_state', state: 'ready', seq: 9 } },
      ],
    });
    const agent = useStore.getState().agents['agent-1'];
    expect(agent.audioOnTurnEnd).toBe(true);
    expect(agent.audioState).toBe('ready');
    expect(agent.audioSeq).toBe(9);
  });
});

describe('annotations store reducer', () => {
  it('hydrates annotations from the snapshot message', () => {
    __testApplyServerMsg({
      t: 'snapshot',
      agentId: 'agent-1',
      seq: 5,
      transcript: [],
      status: 'idle',
      controlMode: 'transcript',
      pendingApprovals: [],
      queuedPrompts: [],
      annotations: [annotation()],
    });
    expect(useStore.getState().annotations['agent-1']).toEqual([annotation()]);
  });

  it('defaults to an empty list when the snapshot omits annotations', () => {
    __testApplyServerMsg({
      t: 'snapshot',
      agentId: 'agent-2',
      seq: 0,
      transcript: [],
      status: 'idle',
      controlMode: 'transcript',
      pendingApprovals: [],
      queuedPrompts: [],
    });
    expect(useStore.getState().annotations['agent-2']).toEqual([]);
  });

  it('replaces the annotation list wholesale on the annotations broadcast', () => {
    __testApplyServerMsg({
      t: 'snapshot',
      agentId: 'agent-1',
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
      agentId: 'agent-1',
      annotations: [annotation({ id: 'a2', comment: 'edited' })],
    });

    expect(useStore.getState().annotations['agent-1']).toEqual([annotation({ id: 'a2', comment: 'edited' })]);
  });

  it('only replaces annotations for the targeted agent, keyed by agentId', () => {
    __testApplyServerMsg({ t: 'annotations', agentId: 'agent-1', annotations: [annotation()] });
    __testApplyServerMsg({ t: 'annotations', agentId: 'agent-2', annotations: [] });

    expect(useStore.getState().annotations['agent-1']).toEqual([annotation()]);
    expect(useStore.getState().annotations['agent-2']).toEqual([]);
  });
});

// The context-usage meter is daemon-owned: usage events carry the timestamp of
// the last real change and live in the event log, so a fresh page load rebuilds
// the meter (and its age) from the snapshot rather than browser storage.
describe('context usage', () => {
  it('rebuilds usage from the replayed transcript with the daemon timestamp', () => {
    __testApplyServerMsg({
      t: 'snapshot',
      agentId: 'agent-1',
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

    expect(useStore.getState().agents['agent-1'].usage).toEqual({
      used: 250,
      size: 1000,
      cost: { amount: 0.5, currency: 'USD' },
      updatedAt: 1_700_000_060_000,
    });
  });

  it('keeps the daemon timestamp for live usage events instead of arrival time', () => {
    __testApplyServerMsg({
      t: 'snapshot', agentId: 'agent-1', seq: 0, transcript: [], status: 'idle', controlMode: 'transcript', pendingApprovals: [], queuedPrompts: [],
    });
    __testApplyServerMsg({
      t: 'event', agentId: 'agent-1', seq: 1, event: { kind: 'usage', used: 42, size: 1000, updatedAt: 1_700_000_000_000 },
    });

    expect(useStore.getState().agents['agent-1'].usage?.updatedAt).toBe(1_700_000_000_000);
  });
});
