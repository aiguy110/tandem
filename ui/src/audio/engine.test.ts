import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('../audio', () => ({ renderMessageAudio: vi.fn() }));

import { renderMessageAudio } from '../audio';
import {
  __getElementForTests,
  __resetForTests,
  getListeningAgentId,
  getState,
  pause,
  play,
  reconcileDaemonPosition,
  restoreLocalPosition,
  seedDurations,
  setKeepaliveEnabled,
  setPlaylist,
  setPositionSender,
} from './engine';

const mockRenderMessageAudio = vi.mocked(renderMessageAudio);

beforeEach(() => {
  __resetForTests();
  let counter = 0;
  mockRenderMessageAudio.mockReset();
  mockRenderMessageAudio.mockImplementation(async (_agentId: string, seq: number) => `blob:${seq}-${counter++}`);
  vi.spyOn(HTMLMediaElement.prototype, 'play').mockResolvedValue(undefined);
  vi.spyOn(HTMLMediaElement.prototype, 'pause').mockImplementation(() => {});
  (URL as unknown as { revokeObjectURL: (url: string) => void }).revokeObjectURL = vi.fn();
  localStorage.clear();
});

afterEach(() => {
  __resetForTests();
  vi.restoreAllMocks();
  localStorage.clear();
});

describe('audio engine', () => {
  it('plays one section at a time: activating a later section supersedes the earlier one', async () => {
    setPlaylist('agent-1', [10, 20]);

    play('agent-1', 10);
    expect(getState().index).toBe(0);
    const el = __getElementForTests();
    expect(el).not.toBeNull();

    play('agent-1', 20);
    // Same shared element the whole time -- there is structurally only one
    // thing that can be audible.
    expect(__getElementForTests()).toBe(el);
    expect(getState().index).toBe(1);

    await vi.waitFor(() => expect(el!.src).toContain('20'));
  });

  it('advances to the next playlist entry when a section ends', async () => {
    setPlaylist('agent-1', [10, 20]);
    play('agent-1', 10);
    await vi.waitFor(() => expect(getState().status).toBe('playing'));

    __getElementForTests()!.dispatchEvent(new Event('ended'));

    await vi.waitFor(() => expect(getState().index).toBe(1));
    await vi.waitFor(() => expect(getState().status).toBe('playing'));
  });

  it('stays armed instead of tearing down once the playlist is exhausted', async () => {
    setPlaylist('agent-1', [10]);
    play('agent-1', 10);
    await vi.waitFor(() => expect(getState().status).toBe('playing'));

    __getElementForTests()!.dispatchEvent(new Event('ended'));

    await vi.waitFor(() => expect(getState().status).toBe('paused'));
    expect(getState().index).toBe(0);
    expect(getState().playlist).toEqual([10]);
  });

  it('records a clip duration on durationchange and reuses it for the global timeline', async () => {
    setPlaylist('agent-1', [10, 20]);
    play('agent-1', 10);
    await vi.waitFor(() => expect(getState().status).toBe('playing'));

    const el = __getElementForTests()!;
    Object.defineProperty(el, 'duration', { configurable: true, value: 12 });
    el.dispatchEvent(new Event('durationchange'));

    expect(getState().durations[10]).toBe(12);
  });
});

describe('audio engine position restore', () => {
  it('flushes position immediately on pause, on section change, and when the playlist ends', async () => {
    const sent: { agentId: string; seq: number; positionMs: number }[] = [];
    setPositionSender((agentId, seq, positionMs) => sent.push({ agentId, seq, positionMs }));

    setPlaylist('agent-1', [10, 20]);
    play('agent-1', 10);
    await vi.waitFor(() => expect(getState().status).toBe('playing'));
    expect(sent).toHaveLength(0); // starting playback alone doesn't flush

    pause();
    expect(sent.at(-1)).toMatchObject({ agentId: 'agent-1', seq: 10 });
    const stored = JSON.parse(localStorage.getItem('tandem.audio.position.agent-1')!);
    expect(stored).toMatchObject({ seq: 10 });

    play('agent-1', 20); // section change: flushes the outgoing section (10)
    expect(sent.at(-1)).toMatchObject({ seq: 10 });
    await vi.waitFor(() => expect(getState().status).toBe('playing'));

    __getElementForTests()!.dispatchEvent(new Event('ended')); // playlist end
    await vi.waitFor(() => expect(getState().status).toBe('paused'));
    expect(sent.at(-1)).toMatchObject({ seq: 20 });
  });

  it('reconciliation: the daemon wins when its updatedAt is not older than the local copy', () => {
    setPlaylist('agent-1', [10, 20]);
    localStorage.setItem('tandem.audio.position.agent-1', JSON.stringify({ seq: 20, positionMs: 4000, updatedAt: 100 }));

    reconcileDaemonPosition('agent-1', { seq: 10, positionMs: 1000, updatedAt: 200 });

    const s = getState();
    expect(s.index).toBe(0);
    expect(s.position).toBeCloseTo(1);
    expect(s.status).toBe('paused');
  });

  it('reconciliation: the local copy wins only when it is strictly newer than the daemon', () => {
    setPlaylist('agent-2', [10, 20]);
    localStorage.setItem('tandem.audio.position.agent-2', JSON.stringify({ seq: 20, positionMs: 4000, updatedAt: 300 }));

    reconcileDaemonPosition('agent-2', { seq: 10, positionMs: 1000, updatedAt: 200 });

    const s = getState();
    expect(s.index).toBe(1);
    expect(s.position).toBeCloseTo(4);
  });

  it('restoring a saved position leaves the engine paused with three-state progress ready across the playlist', () => {
    setPlaylist('agent-1', [10, 20, 30]);
    localStorage.setItem('tandem.audio.position.agent-1', JSON.stringify({ seq: 20, positionMs: 5000, updatedAt: Date.now() }));

    restoreLocalPosition('agent-1');

    const s = getState();
    expect(s.status).toBe('paused'); // never auto-starts playback
    expect(s.index).toBe(1); // section 20 (index 1) is "current"
    expect(s.position).toBeCloseTo(5);
    // Sections before index 1 (seq 10) read as fully played and sections
    // after (seq 30) as untouched via InlineAudioBar's own index comparison
    // against `s.index` -- nothing else to seed for that here.
    expect(mockRenderMessageAudio).not.toHaveBeenCalled(); // restoring never fetches/plays
  });

  it('falls back to no position when the saved seq no longer exists in the playlist', () => {
    setPlaylist('agent-1', [10, 20]);
    localStorage.setItem('tandem.audio.position.agent-1', JSON.stringify({ seq: 999, positionMs: 5000, updatedAt: Date.now() }));

    restoreLocalPosition('agent-1');

    expect(getState().index).toBe(-1);
    expect(getState().status).toBe('idle');
  });

  it('clamps a saved position that exceeds the clip\'s known duration', () => {
    setPlaylist('agent-1', [10]);
    seedDurations('agent-1', { 10: 8 });
    localStorage.setItem('tandem.audio.position.agent-1', JSON.stringify({ seq: 10, positionMs: 20000, updatedAt: Date.now() }));

    restoreLocalPosition('agent-1');

    expect(getState().position).toBeLessThanOrEqual(8);
    expect(getState().status).toBe('paused');
  });
});

describe('getListeningAgentId (audio-focus retention predicate)', () => {
  it('returns the agent while a section is actually playing', async () => {
    setPlaylist('agent-1', [10]);
    play('agent-1', 10);
    await vi.waitFor(() => expect(getState().status).toBe('playing'));

    expect(getListeningAgentId()).toBe('agent-1');
  });

  it('returns the agent while armed and paused mid-section (not merely "has a playlist")', async () => {
    setPlaylist('agent-1', [10, 20]);
    play('agent-1', 10);
    await vi.waitFor(() => expect(getState().status).toBe('playing'));

    pause();
    // jsdom's stubbed .pause() doesn't dispatch a real 'pause' event; fire it
    // manually so the status transition attach() drives from it happens, as
    // it would in a real browser.
    __getElementForTests()!.dispatchEvent(new Event('pause'));

    expect(getState().status).toBe('paused');
    expect(getState().index).toBe(0);
    expect(getListeningAgentId()).toBe('agent-1');
  });

  it('returns null for a chat that has an unplayed playlist but was never started (genuinely not listening)', () => {
    setPlaylist('agent-1', [10, 20]);

    expect(getState().status).toBe('idle');
    expect(getListeningAgentId()).toBeNull();
  });

  it('returns null once there is no playlist at all', () => {
    expect(getState().agentId).toBeNull();
    expect(getListeningAgentId()).toBeNull();
  });
});

describe('silence keepalive', () => {
  it('defaults to enabled', () => {
    expect(getState().keepaliveEnabled).toBe(true);
  });

  it('does not engage during the very first fetch — only once something has actually played', async () => {
    // Never resolves, so the engine stays in 'loading' for this section.
    mockRenderMessageAudio.mockImplementation(() => new Promise<string>(() => {}));
    setPlaylist('agent-1', [10]);

    play('agent-1', 10);
    await vi.waitFor(() => expect(getState().status).toBe('loading'));

    const el = __getElementForTests()!;
    expect(el.loop).toBe(false);
  });

  it('quietly loops the shared element once a playlist is exhausted, without surfacing as a playing section', async () => {
    setPlaylist('agent-1', [10]);
    play('agent-1', 10);
    await vi.waitFor(() => expect(getState().status).toBe('playing'));

    __getElementForTests()!.dispatchEvent(new Event('ended'));
    await vi.waitFor(() => expect(getState().status).toBe('paused'));

    const el = __getElementForTests()!;
    await vi.waitFor(() => expect(el.loop).toBe(true));
    // Never visible as "playing" to the transcript / global player.
    expect(getState().status).toBe('paused');
    expect(getState().index).toBe(0);
  });

  it('never advances the section or records a duration for the silence loop', async () => {
    setPlaylist('agent-1', [10]);
    play('agent-1', 10);
    await vi.waitFor(() => expect(getState().status).toBe('playing'));

    __getElementForTests()!.dispatchEvent(new Event('ended'));
    await vi.waitFor(() => expect(getState().status).toBe('paused'));
    const el = __getElementForTests()!;
    await vi.waitFor(() => expect(el.loop).toBe(true));

    // A duration/ended event on the (looping) silence clip must not be
    // recorded against the real seq or advance the (nonexistent) next section.
    Object.defineProperty(el, 'duration', { configurable: true, value: 1 });
    el.dispatchEvent(new Event('durationchange'));
    el.dispatchEvent(new Event('ended'));

    expect(getState().durations[10]).toBeUndefined();
    expect(getState().status).toBe('paused');
    expect(getState().index).toBe(0);
  });

  it('does not write a position for the silence clip and preserves seq: 0 clear semantics', async () => {
    const sent: { agentId: string; seq: number; positionMs: number }[] = [];
    setPositionSender((agentId, seq, positionMs) => sent.push({ agentId, seq, positionMs }));
    setPlaylist('agent-1', [10]);
    play('agent-1', 10);
    await vi.waitFor(() => expect(getState().status).toBe('playing'));

    __getElementForTests()!.dispatchEvent(new Event('ended'));
    await vi.waitFor(() => expect(getState().status).toBe('paused'));
    // The playlist-end flush still reports the real section (10), never the
    // silence clip's own (irrelevant) position — the keepalive is looping at
    // this point (see the previous test).
    expect(sent.at(-1)).toMatchObject({ agentId: 'agent-1', seq: 10 });

    // Close the chat (same agent, playlist now empty) while the keepalive is
    // still looping, then force a flush the way a hidden/backgrounded tab
    // would (engine.ts's own visibilitychange listener). It must report
    // seq: 0 (no active section) — never a position derived from the
    // (irrelevant) silence clip that's still physically playing.
    setPlaylist('agent-1', []);
    Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => 'hidden' });
    try {
      document.dispatchEvent(new Event('visibilitychange'));
      expect(sent.at(-1)).toMatchObject({ agentId: 'agent-1', seq: 0, positionMs: 0 });
    } finally {
      Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => 'visible' });
    }
  });

  it('stops on an explicit user pause instead of quietly looping', async () => {
    setPlaylist('agent-1', [10, 20]);
    play('agent-1', 10);
    await vi.waitFor(() => expect(getState().status).toBe('playing'));

    pause();
    __getElementForTests()!.dispatchEvent(new Event('pause')); // see note above on jsdom's stubbed .pause()

    const el = __getElementForTests()!;
    expect(el.loop).toBe(false);
    expect(getState().status).toBe('paused');
  });

  it('does not resume by itself after playlist end once the user explicitly pauses again', async () => {
    setPlaylist('agent-1', [10]);
    play('agent-1', 10);
    await vi.waitFor(() => expect(getState().status).toBe('playing'));
    __getElementForTests()!.dispatchEvent(new Event('ended'));
    await vi.waitFor(() => expect(getState().status).toBe('paused'));
    const el = __getElementForTests()!;
    await vi.waitFor(() => expect(el.loop).toBe(true)); // auto-armed keepalive after natural end

    pause(); // an explicit pause on top of the auto-armed state

    expect(el.loop).toBe(false);
  });

  it('stops when the setting is turned off', async () => {
    setPlaylist('agent-1', [10]);
    play('agent-1', 10);
    await vi.waitFor(() => expect(getState().status).toBe('playing'));
    __getElementForTests()!.dispatchEvent(new Event('ended'));
    await vi.waitFor(() => expect(getState().status).toBe('paused'));
    const el = __getElementForTests()!;
    await vi.waitFor(() => expect(el.loop).toBe(true));

    setKeepaliveEnabled(false);

    expect(el.loop).toBe(false);
    expect(getState().keepaliveEnabled).toBe(false);
  });

  it('stops on teardown: the chat closing (playlist cleared) leaves nothing looping', async () => {
    setPlaylist('agent-1', [10]);
    play('agent-1', 10);
    await vi.waitFor(() => expect(getState().status).toBe('playing'));
    __getElementForTests()!.dispatchEvent(new Event('ended'));
    await vi.waitFor(() => expect(getState().status).toBe('paused'));
    const el = __getElementForTests()!;
    await vi.waitFor(() => expect(el.loop).toBe(true));

    setPlaylist('agent-1', []); // same chat, e.g. the agent was closed

    expect(el.loop).toBe(false);
  });

  it('lets a tap on the global player resume the real clip instead of leaving silence looping', async () => {
    setPlaylist('agent-1', [10]);
    play('agent-1', 10);
    await vi.waitFor(() => expect(getState().status).toBe('playing'));
    __getElementForTests()!.dispatchEvent(new Event('ended'));
    await vi.waitFor(() => expect(getState().status).toBe('paused'));
    const el = __getElementForTests()!;
    await vi.waitFor(() => expect(el.loop).toBe(true));
    const silenceCallCount = mockRenderMessageAudio.mock.calls.length;

    play('agent-1'); // "replay" tap on the global player
    __getElementForTests()!.dispatchEvent(new Event('play')); // see note above on jsdom's stubbed .play()

    expect(el.loop).toBe(false);
    // Resumed the already-cached clip directly rather than re-fetching it.
    expect(mockRenderMessageAudio.mock.calls.length).toBe(silenceCallCount);
    expect(getState().status).toBe('playing');
  });
});
