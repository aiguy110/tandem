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
  localStorage.removeItem('tandem.threadAudio');
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
  it('toggles and persists audio for only the selected thread', () => {
    __testApplyServerMsg({
      t: 'snapshot', agentId: 'agent-1', seq: 0, transcript: [], status: 'idle', controlMode: 'transcript', pendingApprovals: [], queuedPrompts: [],
    });
    __testApplyServerMsg({
      t: 'snapshot', agentId: 'agent-2', seq: 0, transcript: [], status: 'idle', controlMode: 'transcript', pendingApprovals: [], queuedPrompts: [],
    });

    useStore.getState().toggleThreadAudio('agent-1');

    expect(useStore.getState().agents['agent-1'].audioOnTurnEnd).toBe(true);
    expect(useStore.getState().agents['agent-2'].audioOnTurnEnd).toBe(false);
    expect(JSON.parse(localStorage.getItem('tandem.threadAudio') ?? '{}')).toEqual({ 'agent-1': true });
  });

  it('keeps a focused agent’s completed turn in Notifications', () => {
    __testApplyServerMsg({
      t: 'snapshot', agentId: 'agent-1', seq: 1, transcript: [], status: 'working', controlMode: 'transcript', pendingApprovals: [], queuedPrompts: [],
    });
    useStore.setState({ focusedId: 'agent-1' });

    __testApplyServerMsg({ t: 'event', agentId: 'agent-1', seq: 2, event: { kind: 'status', status: 'idle' } });

    expect(useStore.getState().agents['agent-1'].turnNotifications).toHaveLength(1);
    expect(useStore.getState().agents['agent-1'].turnNotifications[0].severity).toBe('success');
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
