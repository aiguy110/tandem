import { cleanup, render } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { InlineAudioBar } from './InlineAudioBar';

vi.mock('../../audio', () => ({ renderMessageAudio: vi.fn(async (_agentId: string, seq: number) => `blob:${seq}`) }));

// Imported after the mock above so engine.ts picks up the mocked renderMessageAudio.
import { __resetForTests, getState, play, setPlaylist } from '../../audio/engine';

beforeEach(() => {
  __resetForTests();
  vi.spyOn(HTMLMediaElement.prototype, 'play').mockResolvedValue(undefined);
  vi.spyOn(HTMLMediaElement.prototype, 'pause').mockImplementation(() => {});
  (URL as unknown as { revokeObjectURL: (url: string) => void }).revokeObjectURL = vi.fn();
});

afterEach(() => {
  cleanup();
  __resetForTests();
  vi.restoreAllMocks();
});

describe('InlineAudioBar', () => {
  it('renders the three-state progress rule: before=1, current=live, after=0', () => {
    setPlaylist('session-1', [1, 2, 3]);
    // activateSection sets `index` synchronously before its first await, so
    // the engine already reports section 2 (index 1) as current here.
    play('session-1', 2);

    const view = render(
      <>
        <InlineAudioBar sessionId="session-1" seq={1} />
        <InlineAudioBar sessionId="session-1" seq={2} />
        <InlineAudioBar sessionId="session-1" seq={3} />
      </>,
    );

    const tracks = view.container.querySelectorAll<HTMLDivElement>('.inline-audio-track');
    expect(tracks).toHaveLength(3);
    // Before the current section: fully played, dimmed (no `current` class).
    expect(tracks[0].style.getPropertyValue('--played')).toBe('1');
    expect(tracks[0].classList.contains('current')).toBe(false);
    // The current section: live progress, accent-colored.
    expect(tracks[1].classList.contains('current')).toBe(true);
    // After the current section: untouched.
    expect(tracks[2].style.getPropertyValue('--played')).toBe('0');
    expect(tracks[2].classList.contains('current')).toBe(false);
  });

  it('lets tapping a non-current row start that section', () => {
    setPlaylist('session-1', [1, 2]);
    play('session-1', 1);

    const view = render(<InlineAudioBar sessionId="session-1" seq={2} />);
    const track = view.container.querySelector('.inline-audio-track')!;
    track.getBoundingClientRect = () => ({ left: 0, right: 100, width: 100, top: 0, bottom: 0, height: 0, x: 0, y: 0, toJSON() {} });
    track.dispatchEvent(new MouseEvent('pointerdown', { bubbles: true, clientX: 50 }));

    const s = getState();
    expect(s.playlist[s.index]).toBe(2);
  });
});
