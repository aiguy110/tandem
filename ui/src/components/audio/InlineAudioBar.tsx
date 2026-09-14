import { useEffect, useRef } from 'react';
import { play, seekWithin, subscribeFrame, toggle, useEngineState } from '../../audio/engine';

export function formatAudioTime(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds < 0) return '0:00';
  const total = Math.floor(seconds);
  const m = Math.floor(total / 60);
  const sec = total % 60;
  return `${m}:${sec.toString().padStart(2, '0')}`;
}

// Per-row playback widget. Purely a view over the shared engine's state — it
// owns no media element and no local progress bookkeeping. The three-state
// progress rule (played / live / not-yet-played) is derived from comparing
// this row's seq to the engine's current playlist index; see ui/src/audio/engine.ts.
export function InlineAudioBar({ sessionId, seq }: { sessionId: string; seq: number }) {
  const s = useEngineState();
  const trackRef = useRef<HTMLDivElement>(null);
  const elapsedRef = useRef<HTMLSpanElement>(null);
  const remainingRef = useRef<HTMLSpanElement>(null);
  const dragging = useRef(false);

  const sectionIndex = s.sessionId === sessionId ? s.playlist.indexOf(seq) : -1;
  const isCurrent = sectionIndex !== -1 && sectionIndex === s.index;
  const isPast = sectionIndex !== -1 && s.index !== -1 && sectionIndex < s.index;
  const duration = isCurrent && Number.isFinite(s.duration) ? s.duration : s.durations[seq];
  const isPlaying = isCurrent && s.status === 'playing';

  useEffect(() => {
    const write = (position: number) => {
      const dur = Number.isFinite(duration) && duration! > 0 ? duration! : 0;
      const fraction = isPast ? 1 : !isCurrent ? 0 : dur > 0 ? Math.min(1, Math.max(0, position / dur)) : 0;
      trackRef.current?.style.setProperty('--played', String(fraction));
      if (elapsedRef.current) elapsedRef.current.textContent = formatAudioTime(isCurrent ? position : isPast ? dur : 0);
      if (remainingRef.current) remainingRef.current.textContent = `-${formatAudioTime(isCurrent ? Math.max(0, dur - position) : isPast ? 0 : dur)}`;
    };
    write(isCurrent ? s.position : 0);
    // The shared rAF loop (one per app, regardless of how many bars are
    // mounted) drives the live write for whichever row is current; other
    // rows just get the one discrete write above.
    return subscribeFrame((position) => {
      if (!isCurrent || dragging.current) return;
      write(position);
    });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [isCurrent, isPast, duration, s.position]);

  const seekFromEvent = (e: React.PointerEvent<HTMLDivElement>) => {
    const track = trackRef.current;
    if (!track) return;
    const rect = track.getBoundingClientRect();
    const fraction = rect.width > 0 ? Math.min(1, Math.max(0, (e.clientX - rect.left) / rect.width)) : 0;
    const dur = Number.isFinite(duration) ? duration! : 0;
    seekWithin(seq, fraction * dur);
  };

  return (
    <div className="inline-audio-bar">
      <button
        type="button"
        className="inline-audio-play"
        aria-label={isPlaying ? 'Pause response audio' : 'Play response audio'}
        onClick={() => (isCurrent ? toggle(sessionId) : play(sessionId, seq))}
      >
        {isPlaying ? '❚❚' : '▶'}
      </button>
      <div
        ref={trackRef}
        className={`inline-audio-track${isCurrent ? ' current' : ''}`}
        style={{ '--played': isPast ? 1 : 0 } as React.CSSProperties}
        role="slider"
        aria-label="Playback position"
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={isPast ? 100 : isCurrent ? Math.round(((s.position / (duration || 1)) || 0) * 100) : 0}
        onPointerDown={(e) => {
          dragging.current = true;
          e.currentTarget.setPointerCapture?.(e.pointerId);
          seekFromEvent(e);
        }}
        onPointerMove={(e) => { if (dragging.current) seekFromEvent(e); }}
        onPointerUp={(e) => {
          dragging.current = false;
          if (e.currentTarget.hasPointerCapture?.(e.pointerId)) e.currentTarget.releasePointerCapture(e.pointerId);
          seekFromEvent(e);
        }}
        onPointerCancel={() => { dragging.current = false; }}
      >
        <div className="inline-audio-fill" />
        {isCurrent && <div className="inline-audio-thumb" />}
      </div>
      <span className="inline-audio-time">
        <span ref={elapsedRef} className="inline-audio-elapsed">0:00</span>
        <span ref={remainingRef} className="inline-audio-remaining">-0:00</span>
      </span>
    </div>
  );
}
