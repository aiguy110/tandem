import type { WireEvent } from './wire';

// Browser speech is deliberately a UI-only preference: it is tied to the
// browser/device playing the audio and must never be triggered by transcript
// replay in another client.
export function lastAgentMessage(events: { seq: number; event: WireEvent }[]): string | null {
  let end = -1;
  for (let i = events.length - 1; i >= 0; i--) {
    if (events[i].event.kind === 'message_chunk') {
      end = i;
      break;
    }
  }
  if (end === -1) return null;

  const chunks: string[] = [];
  for (let i = end; i >= 0; i--) {
    const event = events[i].event;
    if (event.kind !== 'message_chunk') break;
    chunks.unshift(event.text);
  }
  const message = chunks.join('').trim();
  return message || null;
}

// Keep the spoken result natural without changing the response's meaning.
// Code itself is retained; only common Markdown delimiters are removed.
export function speechText(message: string): string {
  return message
    .replace(/!?(\[[^\]]*\])\([^\s)]+(?:\s+[^)]*)?\)/g, '$1')
    .replace(/[`*_~>#]/g, '')
    .replace(/\s+/g, ' ')
    .trim();
}

export interface SpeechCallbacks {
  onStart?: () => void;
  onEnd?: () => void;
  onError?: (message: string) => void;
  onUnavailable?: () => void;
}

// Use the browser's device-local speech engine for automatic replies. Unlike
// the transcript's explicit Listen control this does not create a server-side
// audio file, so report its lifecycle to the UI rather than leaving a silent
// best-effort call with no indication of what happened.
export function speak(message: string, callbacks: SpeechCallbacks = {}): void {
  if (typeof window === 'undefined' || !('speechSynthesis' in window) || typeof SpeechSynthesisUtterance === 'undefined') {
    callbacks.onUnavailable?.();
    return;
  }
  const text = speechText(message);
  if (!text) {
    callbacks.onError?.('The completed reply has no text to speak.');
    return;
  }
  // Avoid a backlog when multiple agents finish close together.
  window.speechSynthesis.cancel();
  const utterance = new SpeechSynthesisUtterance(text);
  utterance.onstart = () => callbacks.onStart?.();
  utterance.onend = () => callbacks.onEnd?.();
  utterance.onerror = (event) => callbacks.onError?.(event.error || 'The browser could not play this reply.');
  try {
    window.speechSynthesis.speak(utterance);
  } catch (cause) {
    callbacks.onError?.(cause instanceof Error ? cause.message : 'The browser could not start speech.');
  }
}
