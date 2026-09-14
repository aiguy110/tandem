import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { getGlobalDuration, play, seekGlobal, setKeepaliveEnabled, skip, subscribeFrame, toggle, useEngineState } from '../../audio/engine';
import { formatAudioTime } from './InlineAudioBar';

const KEEPALIVE_TITLE = 'Keep the player alive between responses — lets playback continue with the screen off; uses battery and leaves a persistent notification.';

// Evenly-spaced tick fallback for Phase 1: real per-clip durations aren't
// reported by the daemon yet, so every section gets an equal-width slice.
// Once `durations` is fully populated (locally by playback today; from the
// daemon once a parallel change seeds it via engine.seedDurations), this
// switches to the true cumulative-duration layout automatically — no caller
// needs to change.
function tickFractions(playlist: number[], durations: Record<number, number>): number[] {
  if (playlist.length === 0) return [];
  const hasAllDurations = playlist.every((seq) => Number.isFinite(durations[seq]) && durations[seq] > 0);
  if (hasAllDurations) {
    const total = playlist.reduce((sum, seq) => sum + durations[seq], 0);
    let acc = 0;
    return playlist.map((seq) => {
      const fraction = total > 0 ? acc / total : 0;
      acc += durations[seq];
      return fraction;
    });
  }
  return playlist.map((_, i) => i / playlist.length);
}

// One per focused chat: the whole-playlist transport, docked above the
// prompt bar. Like InlineAudioBar, this is a pure view over the engine —
// dragging/tapping call seekGlobal/skip and the engine resolves which
// section that lands in.
export function GlobalAudioPlayer({ sessionId }: { sessionId: string }) {
  const s = useEngineState();
  const [collapsed, setCollapsed] = useState(false);
  const trackRef = useRef<HTMLDivElement>(null);
  const elapsedRef = useRef<HTMLSpanElement>(null);
  const remainingRef = useRef<HTMLSpanElement>(null);
  const dragging = useRef(false);

  const active = s.sessionId === sessionId;
  const playlist = active ? s.playlist : [];
  const ticks = useMemo(() => tickFractions(playlist, s.durations), [playlist, s.durations]);
  const totalDuration = active ? getGlobalDuration() : 0;
  const isPlaying = active && s.status === 'playing';
  const sectionLabel = active && s.index >= 0 ? `${s.index + 1} / ${playlist.length}` : `– / ${playlist.length}`;

  // Shared by the rAF loop and by dragging: while a drag is in flight the
  // frame loop is suppressed (the element's currentTime lags the seeks we're
  // issuing, so it would fight the thumb), which means the drag itself has to
  // paint — otherwise the track sits frozen until the pointer is released.
  const write = useCallback((global: number) => {
    const dur = totalDuration;
    const fraction = dur > 0 ? Math.min(1, Math.max(0, global / dur)) : 0;
    trackRef.current?.style.setProperty('--played', String(fraction));
    if (elapsedRef.current) elapsedRef.current.textContent = formatAudioTime(global);
    if (remainingRef.current) remainingRef.current.textContent = `-${formatAudioTime(Math.max(0, dur - global))}`;
  }, [totalDuration]);

  useEffect(() => {
    if (!active) return;
    return subscribeFrame((_position, global) => {
      if (dragging.current) return;
      write(global);
    });
  }, [active, write, s.index]);

  if (!active || playlist.length === 0) return null;

  const seekFromEvent = (e: React.PointerEvent<HTMLDivElement>) => {
    const track = trackRef.current;
    if (!track) return;
    const rect = track.getBoundingClientRect();
    const fraction = rect.width > 0 ? Math.min(1, Math.max(0, (e.clientX - rect.left) / rect.width)) : 0;
    const global = fraction * totalDuration;
    write(global);
    seekGlobal(global);
  };

  if (collapsed) {
    return (
      <div className="global-audio-player collapsed">
        <button type="button" className="global-audio-expand" onClick={() => setCollapsed(false)} title="Expand playback controls">
          ▲ {sectionLabel}
        </button>
      </div>
    );
  }

  return (
    <div className="global-audio-player">
      <div className="global-audio-row">
        <button type="button" className="global-audio-skip" aria-label="Back 10 seconds" onClick={() => skip(-10)}>«10</button>
        <button
          type="button"
          className="global-audio-play"
          aria-label={isPlaying ? 'Pause' : 'Play'}
          onClick={() => (s.index === -1 ? play(sessionId) : toggle(sessionId))}
        >
          {isPlaying ? '❚❚' : '▶'}
        </button>
        <button type="button" className="global-audio-skip" aria-label="Forward 10 seconds" onClick={() => skip(10)}>10»</button>
        <span className="global-audio-section">{sectionLabel}</span>
        <button
          type="button"
          className={`global-audio-keepalive${s.keepaliveEnabled ? ' active' : ''}`}
          aria-pressed={s.keepaliveEnabled}
          onClick={() => setKeepaliveEnabled(!s.keepaliveEnabled)}
          title={KEEPALIVE_TITLE}
        >
          <span aria-hidden="true">◔</span>
          <span className="sr-only">{s.keepaliveEnabled ? 'Disable' : 'Enable'} keeping the player alive between responses (uses battery)</span>
        </button>
        <button type="button" className="global-audio-collapse" onClick={() => setCollapsed(true)} title="Collapse playback controls">▼</button>
      </div>
      <div
        ref={trackRef}
        className="global-audio-track"
        role="slider"
        aria-label="Playback position across the whole conversation"
        aria-valuemin={0}
        aria-valuemax={100}
        onPointerDown={(e) => {
          dragging.current = true;
          e.currentTarget.setPointerCapture(e.pointerId);
          seekFromEvent(e);
        }}
        onPointerMove={(e) => { if (dragging.current) seekFromEvent(e); }}
        onPointerUp={(e) => {
          dragging.current = false;
          if (e.currentTarget.hasPointerCapture(e.pointerId)) e.currentTarget.releasePointerCapture(e.pointerId);
          seekFromEvent(e);
        }}
        onPointerCancel={() => { dragging.current = false; }}
      >
        <div className="global-audio-fill" />
        {ticks.map((fraction, i) => (
          <button
            key={playlist[i]}
            type="button"
            className={`global-audio-tick${i === s.index ? ' current' : ''}${i < s.index ? ' past' : ''}`}
            style={{ left: `${fraction * 100}%` }}
            aria-label={`Jump to section ${i + 1}`}
            onPointerDown={(e) => e.stopPropagation()}
            onClick={(e) => { e.stopPropagation(); play(sessionId, playlist[i]); }}
          />
        ))}
        <div className="global-audio-thumb" />
      </div>
      <div className="global-audio-time">
        <span ref={elapsedRef}>0:00</span>
        <span ref={remainingRef}>-0:00</span>
      </div>
    </div>
  );
}
