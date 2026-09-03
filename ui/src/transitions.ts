import { useEffect, useRef, useState } from 'react';

/* Enter/exit animation helpers.
 *
 * React unmounts a popup the instant its condition flips false, which kills any
 * CSS exit animation before it can paint. These hooks keep the outgoing content
 * mounted for the length of the exit animation and hand back a `closing` flag
 * to switch the element over to its exit keyframes. The durations here must
 * stay in step with the `--motion-*` custom properties in styles.css. */

/** How long the exit keyframes in styles.css run for, in ms. */
export const EXIT_MS = 130;

/**
 * Keeps the last non-null `value` mounted while it animates away, so callers
 * can render the outgoing popup's content during its exit.
 */
export function useValuePresence<T>(value: T | null | undefined, exitMs = EXIT_MS) {
  const [rendered, setRendered] = useState<T | null>(value ?? null);
  useEffect(() => {
    if (value != null) {
      setRendered(value);
      return;
    }
    if (rendered == null) return;
    const timer = window.setTimeout(() => setRendered(null), exitMs);
    return () => window.clearTimeout(timer);
  }, [value, rendered, exitMs]);
  return { rendered, closing: value == null && rendered != null };
}

/** Boolean flavour of {@link useValuePresence} for simple open/closed popups. */
export function usePresence(open: boolean, exitMs = EXIT_MS) {
  const { rendered, closing } = useValuePresence(open ? true : null, exitMs);
  return { mounted: rendered != null, closing };
}

/**
 * Returns a class name that re-triggers a CSS flash every time `signal`
 * changes (but not on first mount). The class alternates between two
 * identically-animated names because re-applying the same class would not
 * restart the animation.
 */
export function useUpdateFlash(signal: unknown, base = 'is-updated'): string {
  const previous = useRef(signal);
  const [phase, setPhase] = useState(0);
  useEffect(() => {
    if (Object.is(previous.current, signal)) return;
    previous.current = signal;
    setPhase((current) => (current === 1 ? 2 : 1));
  }, [signal]);
  if (phase === 0) return '';
  return `${base} ${base}-${phase === 1 ? 'a' : 'b'}`;
}
