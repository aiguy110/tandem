import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('../audio', () => ({ renderMessageAudio: vi.fn() }));

import { renderMessageAudio } from '../audio';
import {
  __getElementForTests,
  __resetForTests,
  getState,
  pause,
  play,
  reconcileDaemonPosition,
  restoreLocalPosition,
  seedDurations,
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
